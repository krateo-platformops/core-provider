package resources

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"github.com/krateo-platformops/chart-inspector/internal/handlers/resources"
	helmconfig "github.com/krateo-platformops/plumbing/helm"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

// Separating what a chart OWNS from what it merely LOOKS UP (#228).
//
// The tracer cannot tell the two apart and says so: under `--dry-run=server` a named Helm `lookup`
// and a create's existence-check are both a GET, so a named observation conservatively gets the
// management superset ["*"]. That is sound at that layer and unfixable there.
//
// It is fixable one layer up, because the handler holds the other half of the evidence and used to
// throw it away: the same dry-run install that produces the trace also returns the RENDERED
// MANIFEST. A chart never looks up an object it is about to create and then fails to render it, so:
//
//	named observation IN the manifest      -> the chart owns it      -> keep ["*"]
//	named observation NOT in the manifest  -> read-only lookup       -> get/list/watch
//
// Why this matters beyond tidiness. A chart gating on the presence of a ValidatingAdmissionPolicy
// was granted "*" on ValidatingAdmissionPolicy — permission to delete the policy that is the
// enforcement. And a lookup's name is a template expression, so it can come from a field of the CR,
// which tenants hold update/patch on: a CR-chosen name decided WHICH resource received "*". That
// composition is closed here by construction, since a CR-chosen name selects an object the chart
// does not render and so lands in the read-only branch.
//
// Why not just drop "delete" from the ambiguous superset, which is the obvious narrowing: the grant
// covers the composition's whole LIFECYCLE, and uninstalling a composition deletes the children the
// chart created. The ambiguity is not "lookup vs create", it is "lookup vs OWN", and owning needs
// delete. Narrowing on verbs alone breaks teardown.

// readOnlyVerbs is what a named observation gets once it is known to be a lookup. It matches what
// the tracer already assigns to a collection read, so both kinds of lookup agree.
var readOnlyVerbs = []string{"get", "list", "watch"}

// renderedKey identifies one object a chart renders. Version is deliberately excluded: RBAC is
// granted per group+resource, and a chart that renders v1 while looking up v1beta1 of the same
// resource still owns it.
type renderedKey struct {
	group     string
	resource  string
	namespace string
	name      string
}

// kindPluralizer maps a GVK to its resource. Satisfied by handlers.HandlerOptions.Plurarizer.
type kindPluralizer interface {
	GVKtoGVR(gvk schema.GroupVersionKind) (schema.GroupVersionResource, error)
}

// renderedSet is what a dry-run manifest told us: the objects the chart demonstrably owns, and the
// API groups where it told us something we could not read.
type renderedSet struct {
	owned map[renderedKey]struct{}
	// opaqueGroups are groups containing a rendered Kind that could not be resolved to a resource.
	// Nothing in such a group may be narrowed.
	//
	// This exists because the first version of this code was WRONG in the dangerous direction. A
	// chart that installs a CRD and then renders a custom resource of that new Kind cannot have it
	// pluralized at dry-run time — the CRD is not registered yet — so the object was skipped from
	// the owned set. A comment claimed that was "conservative". It is the opposite: absence from
	// the owned set means NARROWED, so the chart's own custom resources would have been demoted to
	// read-only and the composition could no longer manage them. Caught by
	// TestRenderedObjects_UnresolvableKindIsSkippedNotFatal.
	//
	// Scoped to the group rather than disabling narrowing outright, because the group is exactly
	// what the uncertainty covers: we know the chart renders SOMETHING in it that we cannot name.
	opaqueGroups map[string]struct{}
}

// renderedObjects parses a dry-run release manifest into the set of objects the chart renders.
//
// defaultNamespace is the release namespace, applied to any object whose manifest omits one —
// otherwise a namespaced object would never match the trace, which always carries the namespace the
// request was issued against.
func renderedObjects(manifest, defaultNamespace string, pl kindPluralizer) (renderedSet, error) {
	out := renderedSet{owned: map[renderedKey]struct{}{}, opaqueGroups: map[string]struct{}{}}
	reader := k8syaml.NewYAMLReader(bufio.NewReader(strings.NewReader(manifest)))
	for {
		doc, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return renderedSet{}, fmt.Errorf("reading rendered manifest: %w", err)
		}
		if len(strings.TrimSpace(string(doc))) == 0 {
			continue
		}
		var u unstructured.Unstructured
		if err := yaml.Unmarshal(doc, &u.Object); err != nil {
			// A single unparseable document must not discard the whole set: that would silently
			// re-grant "*" everywhere. Skip it; its object simply stays conservative.
			continue
		}
		if u.GetKind() == "" || u.GetAPIVersion() == "" || u.GetName() == "" {
			continue
		}
		gvk := u.GroupVersionKind()
		gvr, err := pl.GVKtoGVR(gvk)
		if err != nil {
			// We cannot name the resource, but we know the chart renders something in this group.
			// Mark the group opaque so nothing in it is narrowed on evidence we do not have.
			out.opaqueGroups[gvk.Group] = struct{}{}
			continue
		}
		ns := u.GetNamespace()
		if ns == "" {
			ns = defaultNamespace
		}
		out.owned[renderedKey{group: gvk.Group, resource: gvr.Resource, namespace: ns, name: u.GetName()}] = struct{}{}
		// Also record it as cluster-scoped: the trace carries no namespace for a cluster-scoped
		// object, while the manifest may still have been defaulted above. Recording both spellings
		// costs nothing and avoids a miss that would leave the object on ["*"].
		out.owned[renderedKey{group: gvk.Group, resource: gvr.Resource, namespace: "", name: u.GetName()}] = struct{}{}
	}
	return out, nil
}

// narrowLookupVerbs downgrades named observations the chart does not render to read-only, and
// reports how many it changed.
//
// It only ever REMOVES permission, and only from an entry the tracer marked with the superset. A
// collection read is already read-only and is left alone; an entry with any other explicit verb set
// is left alone too, so this cannot widen a grant however the tracer changes.
//
// An empty owned set means the manifest told us nothing — a parse failure, or a chart whose every
// Kind is unresolvable. Then nothing is narrowed: no evidence is not evidence of a lookup, and
// guessing here removes permissions a chart needs to manage its own children. The same rule applies
// per-group via opaqueGroups.
func narrowLookupVerbs(observed []resources.Resource, rendered renderedSet) ([]resources.Resource, int) {
	if len(rendered.owned) == 0 {
		return observed, 0
	}
	narrowed := 0
	out := make([]resources.Resource, len(observed))
	copy(out, observed)
	for i := range out {
		r := &out[i]
		if r.Name == "" || !isSuperset(r.Verbs) {
			continue
		}
		if _, opaque := rendered.opaqueGroups[r.Group]; opaque {
			continue
		}
		k := renderedKey{group: r.Group, resource: r.Resource, namespace: r.Namespace, name: r.Name}
		if _, owns := rendered.owned[k]; owns {
			continue
		}
		r.Verbs = append([]string(nil), readOnlyVerbs...)
		narrowed++
	}
	return out, narrowed
}

// isSuperset reports whether verbs is the tracer's ambiguous ["*"]. An absent verb set means the
// same thing for backward compatibility with an older inspector (see resources.Resource.Verbs).
func isSuperset(verbs []string) bool {
	if len(verbs) == 0 {
		return true
	}
	return len(verbs) == 1 && verbs[0] == "*"
}

// hookObjects reads a release's lifecycle hooks into the two things the RBAC derivation needs:
// resource entries the composition's ServiceAccount must be granted, and renderedKeys marking them
// as chart-owned.
//
// # Why hooks need this at all (core-provider#311)
//
// The resource set comes from a tracer over a DRY-RUN install, narrowed against the release's
// Manifest. A dry run executes no hooks, and helm keeps hooks OUT of Manifest by construction
// (releaseutil.SortManifests splits them into their own slice). So a hook object was invisible to
// both halves and NO grant was ever generated for it.
//
// Measured: a chart whose single template renders a ServiceAccount, a Role, a RoleBinding and a Job
// — only the Job annotated `helm.sh/hook: pre-delete` — got grants for three of the four. The hook
// did not, and `helm uninstall` could then never succeed, because helm must delete the hook object
// under before-hook-creation and lacked `delete` on jobs. A composition that installs fine for
// weeks cannot be torn down, and the failure is invisible until the one moment it matters.
//
// # THE MANAGEMENT VERB SET, NOT THE NARROWED ONE
//
// A hook is created AND deleted by helm. `delete` is exactly the verb that was missing, so these
// entries carry the superset, and the renderedKeys are what stop narrowLookupVerbs from demoting
// them: that function narrows any superset entry whose key it cannot find in the owned set, and a
// hook is absent from Manifest by definition. Adding the keys is not a bypass — a hook IS rendered
// from a chart template, so the chart genuinely owns it; Manifest simply is not where helm files it.
//
// # Events are deliberately ignored
//
// An object needs its permissions regardless of which phase fires it, and before-hook-creation
// makes helm delete a previous hook object whatever its phase. Filtering by event would grant a
// pre-delete hook nothing until a delete was already underway, which is the failure this fixes.
//
// Unresolvable kinds are RETURNED rather than swallowed, so the caller can report at INSTALL time
// what it could not cover — the loud failure that the alternative design (refusing such charts
// outright) was reaching for, without rejecting the charts that work today on incidental overlap.
func hookObjects(hooks []helmconfig.Hook, defaultNamespace string, pl kindPluralizer) ([]resources.Resource, []renderedKey, []string) {
	var (
		out        []resources.Resource
		keys       []renderedKey
		unresolved []string
		seen       = map[renderedKey]struct{}{}
	)
	for _, h := range hooks {
		if strings.TrimSpace(h.Manifest) == "" {
			// A hook with no manifest is nothing we can grant for. Reported, not skipped silently:
			// it means helm gave us a hook whose content we cannot read.
			unresolved = append(unresolved, fmt.Sprintf("%s (%s): empty manifest", h.Name, h.Path))
			continue
		}
		reader := k8syaml.NewYAMLReader(bufio.NewReader(strings.NewReader(h.Manifest)))
		for {
			doc, err := reader.Read()
			if err == io.EOF {
				break
			}
			if err != nil {
				unresolved = append(unresolved, fmt.Sprintf("%s (%s): unreadable manifest: %v", h.Name, h.Path, err))
				break
			}
			if len(strings.TrimSpace(string(doc))) == 0 {
				continue
			}
			var u unstructured.Unstructured
			if err := yaml.Unmarshal(doc, &u.Object); err != nil {
				unresolved = append(unresolved, fmt.Sprintf("%s (%s): unparseable document: %v", h.Name, h.Path, err))
				continue
			}
			if u.GetKind() == "" || u.GetAPIVersion() == "" || u.GetName() == "" {
				continue
			}
			gvk := u.GroupVersionKind()
			gvr, err := pl.GVKtoGVR(gvk)
			if err != nil {
				// Same reasoning as renderedObjects: we know the chart touches this group but
				// cannot name the resource. Reported so an operator learns at install time, and the
				// caller marks the group opaque so nothing in it is narrowed on evidence we lack.
				unresolved = append(unresolved, fmt.Sprintf("%s (%s): cannot resolve %s: %v",
					h.Name, h.Path, gvk.String(), err))
				continue
			}
			ns := u.GetNamespace()
			if ns == "" {
				ns = defaultNamespace
			}
			r := resources.Resource{
				Group:     gvk.Group,
				Version:   gvk.Version,
				Resource:  gvr.Resource,
				Name:      u.GetName(),
				Namespace: ns,
				// The superset. See the comment above: helm creates and DELETES this object.
				Verbs: []string{"*"},
			}
			k := renderedKey{group: gvk.Group, resource: gvr.Resource, namespace: ns, name: u.GetName()}
			if _, dup := seen[k]; dup {
				continue
			}
			seen[k] = struct{}{}
			out = append(out, r)
			keys = append(keys, k)
			// Both spellings, for the same reason renderedObjects records both: a cluster-scoped
			// object carries no namespace in the trace while the manifest may have been defaulted.
			keys = append(keys, renderedKey{group: gvk.Group, resource: gvr.Resource, namespace: "", name: u.GetName()})
		}
	}
	return out, keys, unresolved
}

// hookGroupsOf returns the API groups named by hook documents, so a caller can mark a group opaque
// when a hook's kind could not be resolved.
func hookGroupsOf(hooks []helmconfig.Hook) map[string]struct{} {
	groups := map[string]struct{}{}
	for _, h := range hooks {
		reader := k8syaml.NewYAMLReader(bufio.NewReader(strings.NewReader(h.Manifest)))
		for {
			doc, err := reader.Read()
			if err != nil {
				break
			}
			if len(strings.TrimSpace(string(doc))) == 0 {
				continue
			}
			var u unstructured.Unstructured
			if err := yaml.Unmarshal(doc, &u.Object); err != nil {
				continue
			}
			if u.GetAPIVersion() == "" {
				continue
			}
			groups[u.GroupVersionKind().Group] = struct{}{}
		}
	}
	return groups
}

package resources

import (
	"fmt"
	"strings"
	"testing"

	"github.com/krateo-platformops/chart-inspector/internal/handlers/resources"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// fakePluralizer lowercases and adds "s", which is right for every Kind used below. Kinds listed
// in unresolvable return an error, standing for a CR whose CRD the chart itself installs and which
// therefore cannot be pluralized at dry-run time.
type fakePluralizer struct{ unresolvable map[string]bool }

func (f fakePluralizer) GVKtoGVR(gvk schema.GroupVersionKind) (schema.GroupVersionResource, error) {
	if f.unresolvable[gvk.Kind] {
		return schema.GroupVersionResource{}, fmt.Errorf("no mapping for %s", gvk.Kind)
	}
	return schema.GroupVersionResource{
		Group: gvk.Group, Version: gvk.Version, Resource: strings.ToLower(gvk.Kind) + "s",
	}, nil
}

const manifest = `
apiVersion: v1
kind: ConfigMap
metadata:
  name: owned-cm
  namespace: demo
---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicy
metadata:
  name: owned-policy
---
apiVersion: v1
kind: Secret
metadata:
  name: owned-secret
`

func named(group, resource, ns, name string, verbs ...string) resources.Resource {
	return resources.Resource{Group: group, Version: "v1", Resource: resource, Namespace: ns, Name: name, Verbs: verbs}
}

// The case #228 is about: a chart that GATES on a ValidatingAdmissionPolicy it does not render was
// granted "*" on it — permission to delete the policy that is the enforcement. The gate and the
// thing it checks ended up in the same blast radius.
func TestNarrowLookupVerbs_AReadOnlyGateLosesTheSuperset(t *testing.T) {
	rendered, err := renderedObjects(manifest, "demo", fakePluralizer{})
	if err != nil {
		t.Fatalf("renderedObjects: %v", err)
	}

	observed := []resources.Resource{
		// looked up, NOT rendered — the gate
		named("admissionregistration.k8s.io", "validatingadmissionpolicys", "", "some-other-policy", "*"),
		// rendered — the chart owns it
		named("", "configmaps", "demo", "owned-cm", "*"),
	}

	got, narrowed := narrowLookupVerbs(observed, rendered)
	if narrowed != 1 {
		t.Fatalf("narrowed %d, want 1", narrowed)
	}
	if v := strings.Join(got[0].Verbs, ","); v != "get,list,watch" {
		t.Errorf("the gate kept verbs %q; it can still delete the policy that constrains it", v)
	}
	// The control arm. An owned object must KEEP the superset — the grant covers the whole
	// lifecycle, and uninstalling the composition deletes the children the chart created. A fix
	// that narrowed this too would pass the assertion above and break teardown.
	if v := strings.Join(got[1].Verbs, ","); v != "*" {
		t.Errorf("an object the chart RENDERS was narrowed to %q; teardown needs delete", v)
	}
}

// The composed escalation: a lookup's name is a template expression, so it can come from a CR
// field tenants hold update/patch on. A CR-chosen name therefore decided which resource received
// "*". It selects something the chart does not render, so it lands in the read-only branch by
// construction rather than by special handling.
func TestNarrowLookupVerbs_ACRSuppliedNameCannotReachTheSuperset(t *testing.T) {
	rendered, _ := renderedObjects(manifest, "demo", fakePluralizer{})

	for _, tenantChosen := range []string{"kube-root-ca.crt", "some-operator-token", "owned-cm-but-elsewhere"} {
		observed := []resources.Resource{named("", "secrets", "kube-system", tenantChosen, "*")}
		got, narrowed := narrowLookupVerbs(observed, rendered)
		if narrowed != 1 || strings.Join(got[0].Verbs, ",") != "get,list,watch" {
			t.Errorf("a CR-supplied name %q still reached the superset (verbs %v)", tenantChosen, got[0].Verbs)
		}
	}
}

// No evidence is not evidence of a lookup. If the manifest could not be read — a parse failure, or
// a chart whose every Kind is unresolvable — nothing may be narrowed, or a chart loses permissions
// it needs to manage its own children. This is the direction that must fail safe.
func TestNarrowLookupVerbs_NoRenderedSetNarrowsNothing(t *testing.T) {
	observed := []resources.Resource{named("", "configmaps", "demo", "anything", "*")}

	got, narrowed := narrowLookupVerbs(observed, renderedSet{owned: map[renderedKey]struct{}{}})
	if narrowed != 0 {
		t.Fatalf("narrowed %d with an empty rendered set, want 0", narrowed)
	}
	if strings.Join(got[0].Verbs, ",") != "*" {
		t.Errorf("verbs were narrowed on no evidence: %v", got[0].Verbs)
	}
}

// A collection read is already read-only and must be left exactly as the tracer set it; an entry
// with some other explicit verb set must be left alone too. This can only ever remove permission
// from an ambiguous entry, never add it.
func TestNarrowLookupVerbs_OnlyTouchesAmbiguousNamedEntries(t *testing.T) {
	rendered, _ := renderedObjects(manifest, "demo", fakePluralizer{})

	observed := []resources.Resource{
		named("", "configmaps", "demo", "", "get", "list", "watch"), // collection read
		named("", "pods", "demo", "a-pod", "get", "create"),         // explicit non-superset
	}
	got, narrowed := narrowLookupVerbs(observed, rendered)
	if narrowed != 0 {
		t.Fatalf("narrowed %d, want 0", narrowed)
	}
	if strings.Join(got[0].Verbs, ",") != "get,list,watch" {
		t.Errorf("a collection read was rewritten: %v", got[0].Verbs)
	}
	if strings.Join(got[1].Verbs, ",") != "get,create" {
		t.Errorf("an explicit verb set was rewritten: %v", got[1].Verbs)
	}
}

// A Kind whose CRD the chart itself installs cannot be pluralized at dry-run time. It must be
// SKIPPED from the rendered set rather than failing the parse — and because it is then absent, its
// observations stay on the superset, which is the conservative direction.
func TestRenderedObjects_UnresolvableKindIsSkippedNotFatal(t *testing.T) {
	const withCR = manifest + `---
apiVersion: example.com/v1
kind: Widget
metadata:
  name: a-widget
  namespace: demo
`
	rendered, err := renderedObjects(withCR, "demo", fakePluralizer{unresolvable: map[string]bool{"Widget": true}})
	if err != nil {
		t.Fatalf("an unresolvable Kind must not fail the parse: %v", err)
	}
	// The resolvable objects are still there — otherwise the skip has thrown away the whole set.
	if _, ok := rendered.owned[renderedKey{group: "", resource: "configmaps", namespace: "demo", name: "owned-cm"}]; !ok {
		t.Fatal("skipping one unresolvable Kind discarded the rest of the manifest")
	}
	// And the unresolvable one keeps the superset, because it is absent from the set.
	observed := []resources.Resource{named("example.com", "widgets", "demo", "a-widget", "*")}
	got, narrowed := narrowLookupVerbs(observed, rendered)
	if narrowed != 0 || strings.Join(got[0].Verbs, ",") != "*" {
		t.Errorf("a Kind we could not resolve was narrowed on no evidence: %v", got[0].Verbs)
	}
	// And the blast radius of that uncertainty is the GROUP, not the whole chart: a lookup in a
	// different group is still narrowed, or one unresolvable CRD would switch the feature off.
	other := []resources.Resource{named("", "secrets", "kube-system", "someone-elses", "*")}
	if _, n := narrowLookupVerbs(other, rendered); n != 1 {
		t.Errorf("one unresolvable Kind disabled narrowing outside its own group")
	}
}

// A manifest object with no explicit namespace belongs to the release namespace. Without that
// default it would never match the trace, which always carries the namespace the request went to —
// so every such object would look unowned and be narrowed. That is the failure mode that breaks
// teardown, so it gets its own test.
func TestRenderedObjects_DefaultsTheReleaseNamespace(t *testing.T) {
	rendered, err := renderedObjects(manifest, "demo", fakePluralizer{})
	if err != nil {
		t.Fatalf("renderedObjects: %v", err)
	}
	// owned-secret has no namespace in the manifest; the trace will say "demo".
	observed := []resources.Resource{named("", "secrets", "demo", "owned-secret", "*")}
	got, narrowed := narrowLookupVerbs(observed, rendered)
	if narrowed != 0 {
		t.Fatalf("an owned object with a defaulted namespace was narrowed; teardown would break")
	}
	if strings.Join(got[0].Verbs, ",") != "*" {
		t.Errorf("verbs = %v, want the superset retained", got[0].Verbs)
	}
}

// A cluster-scoped object carries no namespace in the trace. It must still match.
func TestRenderedObjects_ClusterScopedMatchesWithNoNamespace(t *testing.T) {
	rendered, _ := renderedObjects(manifest, "demo", fakePluralizer{})
	observed := []resources.Resource{
		named("admissionregistration.k8s.io", "validatingadmissionpolicys", "", "owned-policy", "*"),
	}
	got, narrowed := narrowLookupVerbs(observed, rendered)
	if narrowed != 0 {
		t.Fatalf("a cluster-scoped object the chart renders was narrowed")
	}
	if strings.Join(got[0].Verbs, ",") != "*" {
		t.Errorf("verbs = %v, want the superset retained", got[0].Verbs)
	}
}

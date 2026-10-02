package restactionrbac

import (
	"sort"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Generated is the RBAC that authorizes a per-composition group to perform the
// reads a RESTAction's apiRef resolves. The subject is always the group
// krateo:cdc:<resource>-<apiVersion> (NOT the CDC ServiceAccount): snowplow
// resolves the RESTAction under the per-user clientconfig bound to that group.
type Generated struct {
	ClusterRole        *rbacv1.ClusterRole        // nil when no cluster-scoped rows
	ClusterRoleBinding *rbacv1.ClusterRoleBinding  // nil when no cluster-scoped rows
	Roles              []rbacv1.Role               // one per namespace with rows
	RoleBindings       []rbacv1.RoleBinding        // paired with Roles
	// Dropped reports read-set rows refused as non-read, so a caller can tell an author why a
	// userAccessFilter check produced no grant. Resource and verb only, never a user identity.
	Dropped DroppedSummary
}

// Generate turns a read-set into group-bound RBAC. name is the metadata name for
// the generated objects (e.g. "<plural>-<version>-restaction"); group is the
// subject group. Cluster-scoped rows (Namespace=="") produce a ClusterRole;
// namespaced rows produce one Role per namespace. Within each role, rows are
// aggregated by (apiGroup, verb) and canonicalised (sorted), so an unchanged
// read-set yields byte-identical objects (digest stability).
//
// Unlike chart rbacgen (one "*" rule per resource bound to the SA), the verb
// comes from the read-set row — precise least privilege.
// readVerbs is the complete set this package will ever grant. A RESTAction's generated RBAC exists
// so the bound group can perform the READS the action performs on its behalf; nothing else belongs
// in it.
var readVerbs = map[string]bool{"get": true, "list": true, "watch": true}

// grantable reports whether a read-set row may become a Role rule.
//
// Two independent gates, deliberately. snowplow marks a row whose verb came from a
// userAccessFilter rather than from a read with NonReadVerb (snowplow#179), and that flag is the
// producer's statement of intent. But a consumer that mints RBAC must not depend on one party
// alone, so the verb is also checked against readVerbs directly: an unflagged "create" is still not
// a read, whatever the producer meant.
//
// The failure this prevents is an inversion, not a leak. A userAccessFilter asks "may this user do
// X?"; copying its verb into a generated Role answers that question by GRANTING X to the group
// being asked about. Three live portal pickers use verb: create (krateo-platformops/core-provider#149).
func grantable(r Resource) bool {
	return !r.NonReadVerb && readVerbs[r.Verb]
}

// partitionGrantable splits a read-set into rows that may be granted and those that may not,
// preserving order so generated objects stay byte-identical for an unchanged read-set.
func partitionGrantable(rows []Resource) (keep []Resource, dropped []Resource) {
	for _, r := range rows {
		if grantable(r) {
			keep = append(keep, r)
			continue
		}
		dropped = append(dropped, r)
	}
	return keep, dropped
}

// NonGrantable reports the rows a Generate call will refuse, so a caller can log why a
// userAccessFilter check produced no grant without duplicating the gate.
func NonGrantable(rows []Resource) DroppedSummary {
	_, dropped := partitionGrantable(rows)
	return summarise(dropped)
}

// DroppedSummary describes the non-read rows a Generate call refused to grant, so a caller can tell
// an author why a picker's check produced no grant. Deliberately carries the resource and verb only
// — never a user identity, which is what a userAccessFilter row is about.
type DroppedSummary struct {
	Count     int
	Resources []string
}

func summarise(dropped []Resource) DroppedSummary {
	seen := map[string]bool{}
	out := DroppedSummary{Count: len(dropped)}
	for _, r := range dropped {
		id := r.Resource + ":" + r.Verb
		if seen[id] {
			continue
		}
		seen[id] = true
		out.Resources = append(out.Resources, id)
	}
	sort.Strings(out.Resources)
	return out
}

func Generate(readSet []Resource, group, name string) Generated {
	readSet, dropped := partitionGrantable(readSet)

	var clusterRows []Resource
	byNamespace := map[string][]Resource{}
	for _, r := range readSet {
		if r.Namespace == "" {
			clusterRows = append(clusterRows, r)
			continue
		}
		byNamespace[r.Namespace] = append(byNamespace[r.Namespace], r)
	}

	groupSubject := rbacv1.Subject{
		Kind:     rbacv1.GroupKind,
		Name:     group,
		APIGroup: rbacv1.GroupName,
	}

	var out Generated
	out.Dropped = summarise(dropped)

	if len(clusterRows) > 0 {
		out.ClusterRole = &rbacv1.ClusterRole{
			// TypeMeta is required for the dynamic-client (unstructured) apply — see GenerateClusterScoped.
			TypeMeta:   metav1.TypeMeta{Kind: "ClusterRole", APIVersion: rbacv1.SchemeGroupVersion.String()},
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Rules:      buildRules(clusterRows),
		}
		out.ClusterRoleBinding = &rbacv1.ClusterRoleBinding{
			TypeMeta:   metav1.TypeMeta{Kind: "ClusterRoleBinding", APIVersion: rbacv1.SchemeGroupVersion.String()},
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Subjects:   []rbacv1.Subject{groupSubject},
			RoleRef: rbacv1.RoleRef{
				APIGroup: rbacv1.GroupName,
				Kind:     "ClusterRole",
				Name:     name,
			},
		}
	}

	for _, ns := range sortedKeys(byNamespace) {
		out.Roles = append(out.Roles, rbacv1.Role{
			TypeMeta:   metav1.TypeMeta{Kind: "Role", APIVersion: rbacv1.SchemeGroupVersion.String()},
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Rules:      buildRules(byNamespace[ns]),
		})
		out.RoleBindings = append(out.RoleBindings, rbacv1.RoleBinding{
			TypeMeta:   metav1.TypeMeta{Kind: "RoleBinding", APIVersion: rbacv1.SchemeGroupVersion.String()},
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Subjects:   []rbacv1.Subject{groupSubject},
			RoleRef: rbacv1.RoleRef{
				APIGroup: rbacv1.GroupName,
				Kind:     "Role",
				Name:     name,
			},
		})
	}

	return out
}

// GenerateClusterScoped collapses the whole read-set into a SINGLE ClusterRole + binding for the
// group, ignoring per-row namespace (a Phase 1 simplification — per-namespace Roles via Generate
// are a Phase 3 refinement). It always returns non-nil objects (Rules may be empty) so the deploy
// lifecycle — apply, digest-hash, lookup, delete — keys off one fixed-name object pair, exactly
// like the authn ServiceAccount mapping. Verbs are per-row (least privilege, never "*").
func GenerateClusterScoped(readSet []Resource, group, name string) (*rbacv1.ClusterRole, *rbacv1.ClusterRoleBinding) {
	// Same gate as Generate. This is a separate entry point used by the deploy path, and fixing one
	// without the other is exactly how a filter like this comes back.
	readSet, _ = partitionGrantable(readSet)
	if len(readSet) == 0 {
		// A binding to a role granting nothing reads as an intended grant someone broke. Emit
		// neither.
		return nil, nil
	}

	cr := &rbacv1.ClusterRole{
		// TypeMeta must be set: these objects are applied via the dynamic client
		// (converted to unstructured), which requires an explicit GVK — a struct
		// literal leaves TypeMeta empty and the apply fails "no kind".
		TypeMeta:   metav1.TypeMeta{Kind: "ClusterRole", APIVersion: rbacv1.SchemeGroupVersion.String()},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Rules:      buildRules(readSet),
	}
	crb := &rbacv1.ClusterRoleBinding{
		TypeMeta:   metav1.TypeMeta{Kind: "ClusterRoleBinding", APIVersion: rbacv1.SchemeGroupVersion.String()},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Subjects: []rbacv1.Subject{{
			Kind:     rbacv1.GroupKind,
			Name:     group,
			APIGroup: rbacv1.GroupName,
		}},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     name,
		},
	}
	return cr, crb
}

// buildRules aggregates rows by (apiGroup, verb) into PolicyRules, sorted for
// determinism. Each row contributes its resource under its own verb.
func buildRules(rows []Resource) []rbacv1.PolicyRule {
	// key = apiGroup + "\x00" + verb → set of resources
	type key struct{ group, verb string }
	agg := map[key]map[string]struct{}{}
	for _, r := range rows {
		k := key{group: r.Group, verb: r.Verb}
		if agg[k] == nil {
			agg[k] = map[string]struct{}{}
		}
		agg[k][r.Resource] = struct{}{}
	}

	keys := make([]key, 0, len(agg))
	for k := range agg {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].group != keys[j].group {
			return keys[i].group < keys[j].group
		}
		return keys[i].verb < keys[j].verb
	})

	rules := make([]rbacv1.PolicyRule, 0, len(keys))
	for _, k := range keys {
		resources := make([]string, 0, len(agg[k]))
		for res := range agg[k] {
			resources = append(resources, res)
		}
		sort.Strings(resources)
		rules = append(rules, rbacv1.PolicyRule{
			APIGroups: []string{k.group},
			Resources: resources,
			Verbs:     []string{k.verb},
		})
	}
	return rules
}

func sortedKeys(m map[string][]Resource) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

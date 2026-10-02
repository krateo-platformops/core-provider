package restactionrbac

import (
	"testing"
)

// verbsOf collects every verb that ended up in a generated rule, across cluster and namespaced
// roles, so a test can assert on what was actually GRANTED rather than on which rows were seen.
func verbsOf(g Generated) []string {
	var out []string
	if g.ClusterRole != nil {
		for _, r := range g.ClusterRole.Rules {
			out = append(out, r.Verbs...)
		}
	}
	for _, role := range g.Roles {
		for _, r := range role.Rules {
			out = append(out, r.Verbs...)
		}
	}
	return out
}

func containsVerb(vs []string, want string) bool {
	for _, v := range vs {
		if v == want {
			return true
		}
	}
	return false
}

// A userAccessFilter asks "can this user do X?". It must never be turned into a grant of X.
//
// core-provider mints this RBAC from snowplow's read-set and copied each row's verb verbatim, so a
// RESTAction whose UAF used `verb: create` produced a Role GRANTING create to the bound group.
// snowplow flags those rows with nonReadVerb: true (snowplow#179); the flag was dropped at decode
// because Resource had no field for it.
func TestGenerate_NonReadVerbRowGrantsNothing(t *testing.T) {
	readSet := []Resource{
		{Group: "", Version: "v1", Resource: "secrets", Verb: "create", NonReadVerb: true},
	}

	g := Generate(readSet, "group-x", "thing-v1-restaction")

	if v := verbsOf(g); containsVerb(v, "create") {
		t.Fatalf("a nonReadVerb row minted a write grant: verbs=%v — a UAF check must never become "+
			"a grant of the thing it checks", v)
	}
	if g.ClusterRole != nil && len(g.ClusterRole.Rules) > 0 {
		t.Errorf("expected no rules at all from a single non-read row, got %v", g.ClusterRole.Rules)
	}
}

// Defence in depth: the producer contract says the flag is always set on a non-read row, but this
// side must not depend on one party alone. An unflagged `create` is still not a read.
func TestGenerate_UnflaggedWriteVerbStillGrantsNothing(t *testing.T) {
	readSet := []Resource{
		{Group: "apps", Version: "v1", Resource: "deployments", Verb: "create"}, // flag absent
		{Group: "", Version: "v1", Resource: "pods", Verb: "delete"},
		{Group: "", Version: "v1", Resource: "configmaps", Verb: "patch"},
	}

	g := Generate(readSet, "group-x", "thing-v1-restaction")

	for _, bad := range []string{"create", "delete", "patch"} {
		if v := verbsOf(g); containsVerb(v, bad) {
			t.Errorf("unflagged %q verb was granted: verbs=%v — the flag is a hint, not the only gate", bad, v)
		}
	}
}

// The same rule applies to namespaced rows, which take a different code path (one Role per
// namespace) and could easily be fixed on one side only.
func TestGenerate_NonReadVerbFilteredInNamespacedRowsToo(t *testing.T) {
	readSet := []Resource{
		{Group: "", Version: "v1", Resource: "secrets", Namespace: "ns-a", Verb: "create", NonReadVerb: true},
		{Group: "", Version: "v1", Resource: "configmaps", Namespace: "ns-a", Verb: "get"},
	}

	g := Generate(readSet, "group-x", "thing-v1-restaction")

	if v := verbsOf(g); containsVerb(v, "create") {
		t.Fatalf("namespaced non-read row minted a write grant: verbs=%v", v)
	}
	if len(g.Roles) != 1 {
		t.Fatalf("expected the read row to still produce one Role, got %d", len(g.Roles))
	}
}

// Read rows are untouched — the fix must not cost legitimate least-privilege grants.
func TestGenerate_ReadVerbsUnchanged(t *testing.T) {
	readSet := []Resource{
		{Group: "", Version: "v1", Resource: "configmaps", Verb: "get"},
		{Group: "", Version: "v1", Resource: "secrets", Verb: "list"},
		{Group: "apps", Version: "v1", Resource: "deployments", Verb: "watch"},
	}

	g := Generate(readSet, "group-x", "thing-v1-restaction")

	if g.ClusterRole == nil {
		t.Fatal("read-only rows must still produce a ClusterRole")
	}
	for _, want := range []string{"get", "list", "watch"} {
		if !containsVerb(verbsOf(g), want) {
			t.Errorf("read verb %q was dropped; the filter must only remove non-read verbs", want)
		}
	}
}

// A read-set that is entirely non-read must produce nothing at all, rather than an empty-ruled
// ClusterRole plus a binding. A binding to a role granting nothing is confusing rather than
// harmless: it looks like an intended grant that someone broke.
func TestGenerate_AllNonReadProducesNoObjects(t *testing.T) {
	readSet := []Resource{
		{Group: "", Version: "v1", Resource: "secrets", Verb: "create", NonReadVerb: true},
		{Group: "", Version: "v1", Resource: "secrets", Namespace: "ns-a", Verb: "update"},
	}

	g := Generate(readSet, "group-x", "thing-v1-restaction")

	if g.ClusterRole != nil || g.ClusterRoleBinding != nil {
		t.Errorf("no cluster-scoped objects expected when every row is non-read")
	}
	if len(g.Roles) != 0 || len(g.RoleBindings) != 0 {
		t.Errorf("no namespaced objects expected when every row is non-read, got %d roles", len(g.Roles))
	}
}

// GenerateClusterScoped is a separate entry point used by the deploy path; it must filter too.
func TestGenerateClusterScoped_FiltersNonReadVerbs(t *testing.T) {
	readSet := []Resource{
		{Group: "", Version: "v1", Resource: "secrets", Verb: "create", NonReadVerb: true},
		{Group: "", Version: "v1", Resource: "configmaps", Verb: "get"},
	}

	cr, _ := GenerateClusterScoped(readSet, "group-x", "thing-v1-restaction")

	if cr == nil {
		t.Fatal("the read row must still produce a ClusterRole")
	}
	for _, r := range cr.Rules {
		if containsVerb(r.Verbs, "create") {
			t.Errorf("GenerateClusterScoped granted create: %v", r)
		}
	}
}

// The caller dereferences the returned ClusterRole. When every row is non-read the generator
// returns nil, so this pins that contract — a nil return that callers do not expect is a panic in
// the deploy path, which is a worse outcome than the bug being fixed.
func TestGenerateClusterScoped_ReturnsNilWhenNothingGrantable(t *testing.T) {
	readSet := []Resource{
		{Group: "", Version: "v1", Resource: "secrets", Verb: "create", NonReadVerb: true},
	}

	cr, crb := GenerateClusterScoped(readSet, "group-x", "thing-v1-restaction")

	if cr != nil || crb != nil {
		t.Errorf("expected nil,nil when no row is grantable; a binding to an empty role reads as an "+
			"intended grant someone broke. got cr=%v crb=%v", cr, crb)
	}
}

// NonGrantable reports what was refused, for the caller's log. It must carry resource and verb and
// nothing else: a userAccessFilter row is about a USER, and that identity must not reach a log.
func TestNonGrantable_SummarisesWithoutIdentities(t *testing.T) {
	readSet := []Resource{
		{Group: "", Version: "v1", Resource: "secrets", Verb: "create", NonReadVerb: true},
		{Group: "", Version: "v1", Resource: "secrets", Verb: "create", NonReadVerb: true}, // duplicate
		{Group: "apps", Version: "v1", Resource: "deployments", Verb: "delete"},
		{Group: "", Version: "v1", Resource: "configmaps", Verb: "get"}, // grantable, not reported
	}

	d := NonGrantable(readSet)

	if d.Count != 3 {
		t.Errorf("expected 3 refused rows (duplicates still count), got %d", d.Count)
	}
	// Deduplicated and sorted, so the log line is stable and bounded.
	if len(d.Resources) != 2 {
		t.Errorf("expected 2 distinct resource:verb entries, got %v", d.Resources)
	}
	for _, r := range d.Resources {
		if r == "configmaps:get" {
			t.Errorf("a grantable row must not be reported as refused: %v", d.Resources)
		}
	}
}

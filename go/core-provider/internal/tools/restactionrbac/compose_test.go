package restactionrbac

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
)

func podsIn(ns string) Resource {
	return Resource{Version: "v1", Resource: "pods", Namespace: ns, Verb: "list"}
}

func rulesMention(rules []rbacv1.PolicyRule, resource string) bool {
	for _, r := range rules {
		for _, x := range r.Resources {
			if x == resource {
				return true
			}
		}
	}
	return false
}

func roleIn(g Composed, ns string) *rbacv1.Role {
	for i := range g.Roles {
		if g.Roles[i].Namespace == ns {
			return &g.Roles[i]
		}
	}
	return nil
}

var nodes = Resource{Version: "v1", Resource: "nodes", Verb: "get"}

// A concrete namespaced row is a rule of the Role in its namespace and of nothing cluster-wide.
func TestCompose_ConcreteNamespacedRowIsARoleNeverAClusterRule(t *testing.T) {
	c := Compose([]Resource{nodes}, [][]Resource{{podsIn("team-a"), nodes}}, "krateo:cdc:x", "x-restaction")
	if c.ClusterRole == nil || rulesMention(c.ClusterRole.Rules, "pods") {
		t.Fatalf("ClusterRole = %+v, must exist and carry no pods rule", c.ClusterRole)
	}
	if !rulesMention(c.ClusterRole.Rules, "nodes") {
		t.Errorf("a cluster-scoped row must stay in the ClusterRole: %+v", c.ClusterRole.Rules)
	}
	r := roleIn(c, "team-a")
	if r == nil || !rulesMention(r.Rules, "pods") || len(c.Roles) != 1 {
		t.Fatalf("Roles = %+v, want exactly one, in team-a, with pods", c.Roles)
	}
	if len(c.RoleBindings) != 1 || c.RoleBindings[0].Subjects[0].Name != "krateo:cdc:x" || c.RoleBindings[0].RoleRef.Kind != "Role" {
		t.Errorf("RoleBindings = %+v", c.RoleBindings)
	}
	if len(c.ClusterWide) != 0 {
		t.Errorf("nothing is cluster-wide here, got %+v", c.ClusterWide)
	}
}

// The same row in two namespaces (two compositions) is two Roles; the Roles are the union of what
// lands in each namespace.
func TestCompose_SameRowInTwoNamespacesAndUnionPerNamespace(t *testing.T) {
	cm := Resource{Version: "v1", Resource: "configmaps", Namespace: "team-a", Verb: "get"}
	c := Compose(nil, [][]Resource{{podsIn("team-a")}, {podsIn("team-b")}, {cm}}, "g", "n")
	if got := c.Namespaces(); !reflect.DeepEqual(got, []string{"team-a", "team-b"}) {
		t.Fatalf("namespaces = %v, want [team-a team-b]", got)
	}
	a := roleIn(c, "team-a")
	if !rulesMention(a.Rules, "pods") || !rulesMention(a.Rules, "configmaps") {
		t.Errorf("team-a's Role must be the union of both compositions' rows landing there: %+v", a.Rules)
	}
	if b := roleIn(c, "team-b"); rulesMention(b.Rules, "configmaps") {
		t.Errorf("team-b must not get team-a's row: %+v", b.Rules)
	}
}

// A row whose namespace comes from a prior stage is the residual: cluster-wide, and named.
func TestCompose_PriorStageRowStaysClusterWideAndIsNamed(t *testing.T) {
	prior := Resource{Resource: "secrets", Verb: "get", NamespaceUnknown: true}
	c := Compose(nil, [][]Resource{{prior, podsIn("team-a")}, {prior}}, "g", "n")
	if c.ClusterRole == nil || !rulesMention(c.ClusterRole.Rules, "secrets") || rulesMention(c.ClusterRole.Rules, "pods") {
		t.Fatalf("ClusterRole = %+v, want secrets and not pods", c.ClusterRole)
	}
	want := []ClusterWide{{Resource: "secrets", Verb: "get"}}
	if !reflect.DeepEqual(c.ClusterWide, want) {
		t.Errorf("ClusterWide = %+v, want %+v (de-duplicated across compositions)", c.ClusterWide, want)
	}
	if c.Deferred.Count != 0 {
		t.Errorf("a residual row is granted, not deferred: %+v", c.Deferred)
	}
}

// What the definition's own resolution (no composition) leaves unknown is not widened: only a
// composition's resolution can make it residual. With no composition, nothing is.
func TestCompose_BaseNamespacedAndUnknownRowsAreNotGranted(t *testing.T) {
	base := []Resource{nodes, podsIn("fixed"), {Resource: "secrets", Verb: "get", NamespaceUnknown: true}}
	c := Compose(base, nil, "g", "n")
	if c.ClusterRole == nil || !rulesMention(c.ClusterRole.Rules, "nodes") || rulesMention(c.ClusterRole.Rules, "pods") || rulesMention(c.ClusterRole.Rules, "secrets") {
		t.Errorf("ClusterRole = %+v: only the cluster-scoped row of the base read-set belongs", c.ClusterRole)
	}
	if len(c.Roles) != 0 || len(c.ClusterWide) != 0 {
		t.Errorf("Roles=%v ClusterWide=%v, want none without a composition", c.Roles, c.ClusterWide)
	}
}

// A userAccessFilter row (non-read verb) is refused even when its namespace is unknown: it never
// becomes a cluster-wide grant by being residual.
func TestCompose_NonReadUnknownRowIsNotResidual(t *testing.T) {
	uaf := Resource{Resource: "secrets", Verb: "delete", NonReadVerb: true, NamespaceUnknown: true}
	c := Compose([]Resource{nodes}, [][]Resource{{uaf}}, "g", "n")
	if rulesMention(c.ClusterRole.Rules, "secrets") || len(c.ClusterWide) != 0 || c.Dropped.Count != 1 {
		t.Errorf("ClusterRole=%+v ClusterWide=%+v Dropped=%+v", c.ClusterRole.Rules, c.ClusterWide, c.Dropped)
	}
}

// The same read-sets give byte-identical objects, whatever order the compositions came in.
func TestCompose_IsDigestStable(t *testing.T) {
	a := Compose([]Resource{nodes}, [][]Resource{{podsIn("a"), podsIn("b")}, {podsIn("c")}}, "g", "n")
	b := Compose([]Resource{nodes}, [][]Resource{{podsIn("c")}, {podsIn("b"), podsIn("a")}}, "g", "n")
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Errorf("not byte-identical:\n%s\n%s", ja, jb)
	}
}

// The extras parameter reaches snowplow, and a 431 is named, not folded into a generic error.
func TestInspect_SendsExtrasAndNamesA431(t *testing.T) {
	var got string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query().Get("extras")
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = w.Write([]byte(`{"readSet":[]}`))
		}
	}))
	defer srv.Close()
	p := Params{APIRefName: "ra", APIRefNamespace: "ns", Extras: map[string]any{"compositionNamespace": "team-a"}}
	if _, err := New(srv.URL).Inspect(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	if got != `{"compositionNamespace":"team-a"}` {
		t.Errorf("extras = %q", got)
	}
	for st, want := range map[int]Cause{
		http.StatusRequestHeaderFieldsTooLarge: CauseExtrasTooLarge,
		// 422 keeps the handling it has today: StageUnresolvable, the stage-unresolvable error, no read-set.
		http.StatusUnprocessableEntity: CauseStageUnresolvable,
	} {
		status = st
		rows, err := New(srv.URL).Inspect(t.Context(), p)
		e, ok := AsError(err)
		if !ok || e.Cause != want || rows != nil {
			t.Errorf("status %d: err=%v rows=%v, want cause %s and no rows", st, err, rows, want)
		}
	}
}

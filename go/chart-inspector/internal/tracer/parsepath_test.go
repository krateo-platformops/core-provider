package tracer

import (
	"reflect"
	"testing"

	"github.com/krateo-platformops/chart-inspector/internal/handlers/resources"
)

// Collection reads — what a Helm `lookup` of a set issues (no trailing name) — must now be captured
// with read-only verbs. Previously they were dropped or mis-parsed (krateo-core-provider#73).
func TestParsePath_CollectionReads(t *testing.T) {
	read := []string{"get", "list", "watch"}
	cases := []struct {
		name string
		path string
		want resources.Resource
	}{
		{"core-ns-collection", "/api/v1/namespaces/ns/secrets",
			resources.Resource{Version: "v1", Resource: "secrets", Namespace: "ns", Verbs: read}},
		{"group-ns-collection", "/apis/apps/v1/namespaces/ns/deployments",
			resources.Resource{Group: "apps", Version: "v1", Resource: "deployments", Namespace: "ns", Verbs: read}},
		{"group-cluster-collection", "/apis/apps/v1/deployments",
			resources.Resource{Group: "apps", Version: "v1", Resource: "deployments", Verbs: read}},
		{"core-cluster-collection", "/api/v1/nodes",
			resources.Resource{Version: "v1", Resource: "nodes", Verbs: read}},
	}
	for _, c := range cases {
		got, ok := parsePath(c.path)
		if !ok {
			t.Errorf("%s: expected a capture for %q", c.name, c.path)
			continue
		}
		if !reflect.DeepEqual(*got, c.want) {
			t.Errorf("%s: got %+v want %+v", c.name, *got, c.want)
		}
	}
}

// A named observation stays the management superset ["*"]; a subresource path maps to its parent.
func TestParsePath_NamedAndSubresource(t *testing.T) {
	got, ok := parsePath("/api/v1/namespaces/ns/pods/p")
	if !ok || got.Name != "p" || !reflect.DeepEqual(got.Verbs, []string{"*"}) {
		t.Fatalf("named pod: got %+v ok=%v", got, ok)
	}
	got, ok = parsePath("/apis/apps/v1/namespaces/ns/deployments/d/status")
	if !ok || got.Resource != "deployments" || got.Name != "d" || !reflect.DeepEqual(got.Verbs, []string{"*"}) {
		t.Fatalf("subresource -> parent: got %+v ok=%v", got, ok)
	}
}

// Discovery / malformed paths are not captured.
func TestParsePath_NonResource(t *testing.T) {
	// note: "/api/v1/namespaces/ns" IS a valid named-namespace GET (resource=namespaces name=ns),
	// so it is intentionally NOT in this non-resource list.
	for _, p := range []string{"/health", "/", "/apis/v1", "/notapis/v1/pods", ""} {
		if got, ok := parsePath(p); ok {
			t.Errorf("%q: expected no capture, got %+v", p, got)
		}
	}
}

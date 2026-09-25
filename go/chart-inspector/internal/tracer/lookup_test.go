package tracer

import "testing"

// The regression for core-provider#73.
//
// RBAC is derived from the API paths a `helm --dry-run=server` install issues, so a chart's
// `lookup` calls must be captured or the composition's ServiceAccount 403s on them at reconcile
// time — and because the read happens inside template evaluation, it surfaces as a confusing
// Go-template error rather than an RBAC one.
//
// The defect was confined to `lookup` of a SET. A length-keyed parser read the COLLECTION form
// `/api/v1/namespaces/{ns}/secrets` as `resource="{ns}"` — the namespace mistaken for the resource
// — so the rule generated was for a resource that does not exist and the real read went ungranted.
// Named lookups were always fine, which is why a lookup-heavy chart guarded on named objects failed
// to reproduce it.
//
// Fixed in c12d2af by keying on segment ROLE rather than path length. These cases pin that; there
// was no test for the collection form when the fix landed.
func TestLookupOfASetIsCaptured(t *testing.T) {
	cases := []struct {
		name              string
		path              string
		group, resource   string
		namespace, object string
	}{
		// The exact mis-parse from the issue: namespace read as the resource.
		{"core-group namespaced collection", "/api/v1/namespaces/krateo-system/secrets",
			"", "secrets", "krateo-system", ""},
		{"grouped namespaced collection", "/apis/composition.krateo.io/v1alpha1/namespaces/krateo-system/openstacks",
			"composition.krateo.io", "openstacks", "krateo-system", ""},
		{"cluster-scoped collection", "/apis/apiextensions.k8s.io/v1/customresourcedefinitions",
			"apiextensions.k8s.io", "customresourcedefinitions", "", ""},

		// Named lookups were never broken. Kept so a future reshuffle cannot fix the collection
		// form by breaking these — the two shapes are the same length in some combinations, which
		// is what the length-keyed parser got wrong in the first place.
		{"cluster-scoped named", "/apis/apiextensions.k8s.io/v1/customresourcedefinitions/compositions.krateo.io",
			"apiextensions.k8s.io", "customresourcedefinitions", "", "compositions.krateo.io"},
		{"core-group namespaced named", "/api/v1/namespaces/krateo-system/secrets/git-provider-credentials",
			"", "secrets", "krateo-system", "git-provider-credentials"},
		{"grouped namespaced named", "/apis/oidc.authn.krateo.io/v1alpha1/namespaces/krateo-system/oidcconfigs/default",
			"oidc.authn.krateo.io", "oidcconfigs", "krateo-system", "default"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parsePath(tc.path)
			if !ok {
				t.Fatalf("path was not parsed at all: %s", tc.path)
			}
			if got.Resource != tc.resource {
				t.Errorf("resource: want %q, got %q — a wrong resource here means the grant is written "+
					"for something that does not exist and the real read stays ungranted", tc.resource, got.Resource)
			}
			if got.Group != tc.group {
				t.Errorf("group: want %q, got %q", tc.group, got.Group)
			}
			if got.Namespace != tc.namespace {
				t.Errorf("namespace: want %q, got %q", tc.namespace, got.Namespace)
			}
			if got.Name != tc.object {
				t.Errorf("name: want %q, got %q", tc.object, got.Name)
			}
		})
	}
}

// Verbs come from the request SHAPE, never from the HTTP method, and that is deliberate.
//
// Under DryRunServer every request is a GET, including the existence checks helm makes before
// creating. A method→verb map would therefore strip create/update/delete from the resources a
// chart actually manages — the very resources it most needs them for. Shape is the only safe
// discriminator: a chart never creates a COLLECTION, so a collection read can only be a lookup and
// is safely read-only; a NAMED request is ambiguous between a create-check and a named lookup, so
// it stays the management superset.
//
// This is the non-obvious constraint a later refactor would otherwise "simplify" away.
func TestVerbsComeFromShapeNotMethod(t *testing.T) {
	collection, _ := parsePath("/api/v1/namespaces/krateo-system/secrets")
	if want := []string{"get", "list", "watch"}; !equal(collection.Verbs, want) {
		t.Errorf("a collection read can only be a lookup, so it must be read-only: want %v, got %v", want, collection.Verbs)
	}

	named, _ := parsePath("/api/v1/namespaces/krateo-system/secrets/some-secret")
	if want := []string{"*"}; !equal(named.Verbs, want) {
		t.Errorf("a named request is ambiguous between a create-check and a lookup under DryRunServer, "+
			"so it must stay the management superset: want %v, got %v", want, named.Verbs)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

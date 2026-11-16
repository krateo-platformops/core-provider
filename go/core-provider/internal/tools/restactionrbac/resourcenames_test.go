package restactionrbac

import (
	"reflect"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
)

// authn, measured on kind at 2.13.105: internal-ca-bundle's RESTAction reads ONE Bundle by name
// (GET /apis/trust.cert-manager.io/v1alpha1/bundles/krateo-internal-ca), and the generated
// ClusterRole granted `get` on EVERY Bundle, because the row's Name was decoded and dropped. A row
// naming an object is granted on that object only.
func TestANamedReadIsGrantedOnThatObjectOnly(t *testing.T) {
	bundle := Resource{Group: "trust.cert-manager.io", Version: "v1alpha1", Resource: "bundles", Name: "krateo-internal-ca", Verb: "get"}
	cr, _ := GenerateClusterScoped([]Resource{bundle}, "krateo:cdc:x", "x-restaction")
	want := []rbacv1.PolicyRule{{APIGroups: []string{"trust.cert-manager.io"}, Resources: []string{"bundles"}, ResourceNames: []string{"krateo-internal-ca"}, Verbs: []string{"get"}}}
	if !reflect.DeepEqual(cr.Rules, want) {
		t.Fatalf("rules = %+v, want %+v", cr.Rules, want)
	}
}

func TestBuildRulesNamedAndUnnamed(t *testing.T) {
	g := "trust.cert-manager.io"
	cases := []struct {
		name string
		rows []Resource
		want []rbacv1.PolicyRule
	}{
		{"control: an unnamed read stays unscoped",
			[]Resource{{Group: g, Resource: "bundles", Verb: "list"}},
			[]rbacv1.PolicyRule{{APIGroups: []string{g}, Resources: []string{"bundles"}, Verbs: []string{"list"}}}},
		{"two names on one resource merge into one rule",
			[]Resource{{Group: g, Resource: "bundles", Name: "b", Verb: "get"}, {Group: g, Resource: "bundles", Name: "a", Verb: "get"}},
			[]rbacv1.PolicyRule{{APIGroups: []string{g}, Resources: []string{"bundles"}, ResourceNames: []string{"a", "b"}, Verbs: []string{"get"}}}},
		{"an unnamed read of the same resource and verb covers the named one",
			[]Resource{{Group: g, Resource: "bundles", Name: "a", Verb: "get"}, {Group: g, Resource: "bundles", Verb: "get"}},
			[]rbacv1.PolicyRule{{APIGroups: []string{g}, Resources: []string{"bundles"}, Verbs: []string{"get"}}}},
		{"a named get and an unnamed list of the same resource stay separate",
			[]Resource{{Group: g, Resource: "bundles", Name: "a", Verb: "get"}, {Group: g, Resource: "bundles", Verb: "list"}},
			[]rbacv1.PolicyRule{
				{APIGroups: []string{g}, Resources: []string{"bundles"}, Verbs: []string{"list"}},
				{APIGroups: []string{g}, Resources: []string{"bundles"}, ResourceNames: []string{"a"}, Verbs: []string{"get"}}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := buildRules(c.rows); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("rules = %+v, want %+v", got, c.want)
			}
		})
	}
}

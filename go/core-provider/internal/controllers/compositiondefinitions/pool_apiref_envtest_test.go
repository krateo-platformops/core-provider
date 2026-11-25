//go:build envtest

package compositiondefinitions

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Two members of one pool, each with an apiRef whose RESTAction reads something else. They share the
// pool's identity, and so its group, but not their grants: both reads are granted at once, and neither
// member's pass changes the other's.
func TestEnvtest_PoolMembersWithAnApiRefEach(t *testing.T) {
	e := start652(t)
	e.c.installAuthnCRD(t, true)
	rbac := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, `{"readSet":[{"group":"","version":"v1","resource":%q,"namespace":%q,"verb":"get"}]}`, r.URL.Query().Get("apiRefName"), self642CDNS)
	}))
	t.Cleanup(rbac.Close)
	was := SnowplowURL
	SnowplowURL = rbac.URL
	t.Cleanup(func() { SnowplowURL = was })
	reads := func(resource string) func(*compositiondefinitionsv1alpha1.CompositionDefinition) {
		return func(cd *compositiondefinitionsv1alpha1.CompositionDefinition) {
			cd.Spec.ApiRef = &compositiondefinitionsv1alpha1.ApiReference{Name: resource, Namespace: self642CDNS}
		}
	}
	a, b := e.member("alpha", "web", reads("configmaps")), e.member("beta", "web", reads("services"))
	e.settle(1, 3)

	granted := func() map[string]bool {
		t.Helper()
		var crbs rbacv1.ClusterRoleBindingList
		if err := e.c.cl.List(context.Background(), &crbs); err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, crb := range crbs.Items {
			if len(crb.Subjects) != 1 || crb.Subjects[0].Kind != "Group" || crb.Subjects[0].Name != "krateo:cdc:cdc-pool-web" {
				continue
			}
			var cr rbacv1.ClusterRole
			if err := e.c.cl.Get(context.Background(), types.NamespacedName{Name: crb.RoleRef.Name}, &cr); err != nil {
				t.Fatal(err)
			}
			for _, r := range cr.Rules {
				for _, res := range r.Resources {
					out[res] = true
				}
			}
		}
		return out
	}
	for range 2 {
		for _, d := range []*cd644{a, b} {
			d.rest(1) // at rest: the other member's grants are not its drift
			if got := granted(); !got["configmaps"] || !got["services"] {
				t.Errorf("after %s's pass the pool's group may read %v, want both members' reads", d.key.Name, got)
			}
		}
	}
}

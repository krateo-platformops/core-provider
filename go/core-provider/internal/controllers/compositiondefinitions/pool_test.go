package compositiondefinitions

import (
	"reflect"
	"testing"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func poolCD(ns, name string, age int, version, pool string) compositiondefinitionsv1alpha1.CompositionDefinition {
	cd := ownerCD(ns, name, age)
	cd.Spec.Chart = &compositiondefinitionsv1alpha1.ChartInfo{Url: "oci://example/" + name, Version: version}
	cd.Status.ApiVersion = "composition.krateo.io/" + version
	cd.Status.Kind = "Page"
	cd.Status.Resource = "pages"
	if pool != "" {
		p := pool
		cd.Spec.Controller = &compositiondefinitionsv1alpha1.ControllerConfig{Pool: &p}
	}
	return cd
}

func gvrsOf(t *testing.T, g []schema.GroupVersionResource) []string {
	t.Helper()
	out := []string{}
	for _, x := range g {
		out = append(out, x.Version+"/"+x.Resource)
	}
	return out
}

// A member is a Kind x version CONTROLLER whose OWNER names the pool: a follower's pool field changes
// nothing, because a follower runs no controller (electControllerOwner).
func TestPoolMembers(t *testing.T) {
	owner := poolCD("team-a", "pages", 0, "v1-0-0", "web")
	followerOther := poolCD("team-b", "pages-copy", 1, "v1-0-0", "other") // same Kind x version, younger
	ownerNoPool := poolCD("team-a", "pages", 0, "v1-0-0", "")
	followerWeb := poolCD("team-b", "pages-copy", 1, "v1-0-0", "web")
	v2 := poolCD("team-c", "pages-v2", 2, "v2-0-0", "web")
	deleting := poolCD("team-d", "going", 3, "v3-0-0", "web")
	now := metav1.Now()
	deleting.SetDeletionTimestamp(&now)
	remote := remoteCD(poolCD("team-e", "far", 4, "v4-0-0", "web"))
	unreconciled := poolCD("team-f", "new", 5, "v5-0-0", "web")
	unreconciled.Status = compositiondefinitionsv1alpha1.CompositionDefinitionStatus{}

	cases := []struct {
		name string
		cds  []compositiondefinitionsv1alpha1.CompositionDefinition
		want []string
	}{
		{"an owner naming the pool is a member", []compositiondefinitionsv1alpha1.CompositionDefinition{owner}, []string{"v1-0-0/pages"}},
		{"the owner decides, not a follower naming another pool", []compositiondefinitionsv1alpha1.CompositionDefinition{owner, followerOther}, []string{"v1-0-0/pages"}},
		{"a follower naming the pool whose owner does not: not pooled", []compositiondefinitionsv1alpha1.CompositionDefinition{ownerNoPool, followerWeb}, []string{}},
		{"two versions are two members, sorted", []compositiondefinitionsv1alpha1.CompositionDefinition{v2, owner}, []string{"v1-0-0/pages", "v2-0-0/pages"}},
		{"deleting, remote and unreconciled definitions are not members", []compositiondefinitionsv1alpha1.CompositionDefinition{deleting, remote, unreconciled}, []string{}},
		{"another pool's member is not this pool's", []compositiondefinitionsv1alpha1.CompositionDefinition{followerOther}, []string{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := gvrsOf(t, poolMembers("web", c.cds)); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("poolMembers(web) = %v, want %v", got, c.want)
			}
		})
	}
}

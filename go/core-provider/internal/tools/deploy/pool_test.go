package deploy

import (
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestPoolNames(t *testing.T) {
	n := PoolNames("pages")
	if n.Workload != "cdc-pool-pages" || n.EnvConfigMap != "cdc-pool-pages-configmap" || n.Index != "cdc-pool-pages-members" {
		t.Fatalf("PoolNames(pages) = %+v", n)
	}
	// The API caps a pool name at 54 so the workload name stays a 63-character label.
	if w := PoolNames(strings.Repeat("a", 54)).Workload; len(w) != 63 {
		t.Fatalf("a 54-character pool gives a %d-character workload name", len(w))
	}
}

func TestValidatePoolName(t *testing.T) {
	for _, ok := range []string{"pages", "a", "0", strings.Repeat("a", 54)} {
		if err := ValidatePoolName(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "Pages", "-a", "a-", "a_b", "a.b", strings.Repeat("a", 55)} {
		if err := ValidatePoolName(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// The index names each member by its dedicated object stem and points at its env ConfigMap, the
// names a dedicated controller already uses, so the pool reads exactly what core-provider renders.
func TestRenderPoolIndex(t *testing.T) {
	portals := schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1-8-129", Resource: "portals"}
	pages := schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1-8-129", Resource: "searchpages"}
	cm := RenderPoolIndex("pages", "krateo-system", []schema.GroupVersionResource{portals, pages})
	if cm.Name != "cdc-pool-pages-members" || cm.Namespace != "krateo-system" || cm.Labels[PoolLabel] != "pages" {
		t.Fatalf("index metadata = %s/%s %v", cm.Namespace, cm.Name, cm.Labels)
	}
	want := map[string]string{
		"portals-v1-8-129":     "portals-v1-8-129-configmap",
		"searchpages-v1-8-129": "searchpages-v1-8-129-configmap",
	}
	if !reflect.DeepEqual(cm.Data, want) {
		t.Fatalf("index data = %v, want %v", cm.Data, want)
	}
	if got := PoolIndexMembers(cm); !reflect.DeepEqual(got, []string{"portals-v1-8-129", "searchpages-v1-8-129"}) {
		t.Fatalf("PoolIndexMembers = %v", got)
	}
	if empty := RenderPoolIndex("pages", "krateo-system", nil); len(empty.Data) != 0 {
		t.Fatalf("an empty pool renders %v", empty.Data)
	}
}

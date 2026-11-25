package deploy

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// poolSizingFrom writes the pool-sizing.json the shipped chart renders with overrides (the file
// core-provider reads at PoolSizingPath) and returns its path.
func poolSizingFrom(t *testing.T, overrides map[string]any) string {
	t.Helper()
	return mountedTemplate(t, renderShippedChart(t, overrides), "cdc-deployment.yaml", "pool-sizing.json")
}

// The sizing core-provider resolves for a pool is cdc.pool.default deep-merged with
// cdc.pool.overrides.<pool>, read from what the chart RENDERS, so a values key the template drops or
// renames fails here rather than in a cluster.
func TestLoadPoolSizing_DefaultDeepMergedWithTheOverride(t *testing.T) {
	t.Run("chart defaults: 10 workers, the documented resources", func(t *testing.T) {
		got, err := LoadPoolSizing(poolSizingFrom(t, nil), "general")
		if err != nil {
			t.Fatal(err)
		}
		want := PoolSizing{Workers: 10, Resources: map[string]any{
			"requests": map[string]any{"cpu": "100m", "memory": "660Mi"},
			"limits":   map[string]any{"memory": "1Gi"},
		}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("sizing = %+v, want %+v", got, want)
		}
	})

	t.Run("an override replaces only the leaves it names", func(t *testing.T) {
		path := poolSizingFrom(t, map[string]any{"cdc": map[string]any{"pool": map[string]any{"overrides": map[string]any{
			"heavy": map[string]any{"workers": 4, "resources": map[string]any{"limits": map[string]any{"memory": "3Gi"}}},
		}}}})
		heavy, err := LoadPoolSizing(path, "heavy")
		if err != nil {
			t.Fatal(err)
		}
		want := PoolSizing{Workers: 4, Resources: map[string]any{
			"requests": map[string]any{"cpu": "100m", "memory": "660Mi"}, // kept from the default
			"limits":   map[string]any{"memory": "3Gi"},
		}}
		if !reflect.DeepEqual(heavy, want) {
			t.Errorf("heavy = %+v, want %+v", heavy, want)
		}
		// CONTROL: another pool in the same file gets the default, untouched by heavy's override.
		general, err := LoadPoolSizing(path, "general")
		if err != nil {
			t.Fatal(err)
		}
		if general.Workers != 10 || general.Resources["limits"].(map[string]any)["memory"] != "1Gi" {
			t.Errorf("general = %+v, want the default", general)
		}
	})

	t.Run("refusals: no file, no default, workers < 1", func(t *testing.T) {
		if _, err := LoadPoolSizing(filepath.Join(t.TempDir(), "absent.json"), "general"); err == nil {
			t.Error("a missing sizing file was accepted")
		}
		noDefault := filepath.Join(t.TempDir(), "s.json")
		_ = os.WriteFile(noDefault, []byte(`{"overrides":{}}`), 0o644)
		if _, err := LoadPoolSizing(noDefault, "general"); err == nil || !strings.Contains(err.Error(), "no default") {
			t.Errorf("no default: err = %v", err)
		}
		// The chart's schema refuses workers 0 at install; LoadPoolSizing refuses it again for a
		// file that did not come through the schema.
		if _, err := renderShippedChartE(t, map[string]any{"cdc": map[string]any{"pool": map[string]any{"overrides": map[string]any{
			"idle": map[string]any{"workers": 0},
		}}}}); err == nil || !strings.Contains(err.Error(), "workers") {
			t.Errorf("cdc.pool.overrides.idle.workers=0: render err = %v, want the schema naming workers", err)
		}
		zero := filepath.Join(t.TempDir(), "s.json")
		_ = os.WriteFile(zero, []byte(`{"default":{"workers":10},"overrides":{"idle":{"workers":0}}}`), 0o644)
		if _, err := LoadPoolSizing(zero, "idle"); err == nil || !strings.Contains(err.Error(), "at least 1") {
			t.Errorf("workers 0: err = %v", err)
		}
	})
}

// The pool's ServiceAccount may read the pool's index (get/list/watch: the cdc lists and watches it
// through a metadata.name field selector, manager/pool.go) and get its members' env ConfigMaps (read
// by name), and nothing else: no other ConfigMap, no write.
func TestPoolRBAC_OnlyTheIndexAndTheMembersEnv(t *testing.T) {
	members := []PoolMember{
		{GVR: schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1-2-0", Resource: "portals"}},
		{GVR: schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v0-3-28", Resource: "mariadbs"}},
	}
	role, binding := PoolRBAC("general", "krateo-system", poolView{members: members}.envNames())
	names := PoolNames("general")

	want := []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"configmaps"}, ResourceNames: []string{names.Index}, Verbs: []string{"get", "list", "watch"}},
		{APIGroups: []string{""}, Resources: []string{"configmaps"}, ResourceNames: []string{
			MemberEnvConfigMapName(members[1].GVR), MemberEnvConfigMapName(members[0].GVR), // sorted
		}, Verbs: []string{"get"}},
	}
	if !reflect.DeepEqual(role.Rules, want) {
		t.Errorf("rules = %+v\nwant    %+v", role.Rules, want)
	}
	sa := PoolServiceAccount("general", "krateo-system")
	if len(binding.Subjects) != 1 || binding.Subjects[0].Name != sa.Name || binding.Subjects[0].Namespace != sa.Namespace ||
		binding.RoleRef.Name != role.Name || role.Namespace != "krateo-system" {
		t.Errorf("binding %+v does not bind role %s to the pool's ServiceAccount %s", binding, role.Name, sa)
	}
	// No member: no env rule at all. One with no resourceNames would grant every ConfigMap here.
	if role, _ := PoolRBAC("general", "krateo-system", nil); len(role.Rules) != 1 {
		t.Errorf("an empty pool's rules = %+v, want only the index's", role.Rules)
	}
}

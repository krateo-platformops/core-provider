package compositiondefinitions

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/krateo-platformops/core-provider/internal/tools/objects/templates"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// The member keys a pool member's env ConfigMap carries (docs/design/cdc-pooling.md §5c), read by the
// cdc's manager.MemberFromConfig.
var poolMemberKeys = []string{
	"COMPOSITION_CONTROLLER_GROUP", "COMPOSITION_CONTROLLER_VERSION", "COMPOSITION_CONTROLLER_RESOURCE",
	"KRATEO_POOL_MEMBER_WORKERS", "KRATEO_POOL_MEMBER_RESYNC_INTERVAL",
}

// renderMemberConfigMap renders the SHIPPED chart's cdc ConfigMap asset the way core-provider does at
// runtime: helm renders the chart into the asset, then the asset is rendered with templates.Values
// plus extra.
func renderMemberConfigMap(t *testing.T, extra map[string]any) map[string]string {
	t.Helper()
	root := t.TempDir()
	if _, err := renderChartAssets(root, "krateo-system", "composition-dynamic-controller:unit"); err != nil {
		t.Fatal(err)
	}
	asset, err := os.ReadFile(filepath.Join(root, filepath.Base(filepath.Dir(CDCtemplateConfigmapPath)), filepath.Base(CDCtemplateConfigmapPath)))
	if err != nil {
		t.Fatal(err)
	}
	values := templates.Values(templates.Renderoptions{Group: "composition.krateo.io", Version: "v1-2-4", Resource: "fireworksapps", Namespace: "krateo-system"})
	for k, v := range extra {
		values[k] = v
	}
	out, err := templates.Template(asset).Render(values)
	if err != nil {
		t.Fatalf("rendering the member ConfigMap: %v", err)
	}
	var cm corev1.ConfigMap
	if err := yaml.Unmarshal(out, &cm); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(cm.Data) == 0 {
		t.Fatalf("the rendered ConfigMap has no data, so the absence checks below would pass on nothing:\n%s", out)
	}
	return cm.Data
}

// A dedicated controller's ConfigMap is unchanged by pool step 5: the member keys are rendered only
// with `pool` set, so its contents, and the checksum its Deployment's pod template carries, do not
// move. A pool member's carries its Kind x version, and its own workers and resync only when the
// definition sets them. The cdc reads these keys by these names (manager.MemberFromConfig).
func TestPoolMemberConfigMapKeys(t *testing.T) {
	dedicated := renderMemberConfigMap(t, nil)
	for _, k := range poolMemberKeys {
		if _, ok := dedicated[k]; ok {
			t.Errorf("a dedicated controller's ConfigMap carries %s", k)
		}
	}
	t.Logf("dedicated ConfigMap: %d keys", len(dedicated))

	bare := renderMemberConfigMap(t, map[string]any{"pool": "small"})
	for k, want := range map[string]string{
		"COMPOSITION_CONTROLLER_GROUP": "composition.krateo.io", "COMPOSITION_CONTROLLER_VERSION": "v1-2-4", "COMPOSITION_CONTROLLER_RESOURCE": "fireworksapps",
	} {
		if bare[k] != want {
			t.Errorf("pool member %s = %q, want %q", k, bare[k], want)
		}
	}
	for _, k := range []string{"KRATEO_POOL_MEMBER_WORKERS", "KRATEO_POOL_MEMBER_RESYNC_INTERVAL"} {
		if _, ok := bare[k]; ok {
			t.Errorf("a member that sets no %s carries it", k)
		}
	}
	if len(bare) != len(dedicated)+3 {
		t.Errorf("pool member has %d keys, dedicated %d: want exactly the 3 Kind x version keys more", len(bare), len(dedicated))
	}

	tuned := renderMemberConfigMap(t, map[string]any{"pool": "small", "workers": 3, "resyncInterval": "5m"})
	if tuned["KRATEO_POOL_MEMBER_WORKERS"] != "3" || tuned["KRATEO_POOL_MEMBER_RESYNC_INTERVAL"] != "5m" {
		t.Errorf("member overrides = %q, %q; want 3, 5m", tuned["KRATEO_POOL_MEMBER_WORKERS"], tuned["KRATEO_POOL_MEMBER_RESYNC_INTERVAL"])
	}
}

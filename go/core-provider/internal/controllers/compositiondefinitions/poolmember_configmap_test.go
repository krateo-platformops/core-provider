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
	unset := envConfigMapTemplateValues()
	for i := 0; i < len(unset); i += 2 {
		values[unset[i].(string)] = unset[i+1]
	}
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

// renderPoolConfigMap renders the shipped chart's pool process ConfigMap asset as core-provider would.
func renderPoolConfigMap(t *testing.T, values map[string]any) map[string]string {
	t.Helper()
	root := t.TempDir()
	if _, err := renderChartAssets(root, "krateo-system", "composition-dynamic-controller:unit"); err != nil {
		t.Fatal(err)
	}
	asset, err := os.ReadFile(filepath.Join(root, filepath.Base(filepath.Dir(CDCtemplateConfigmapPath)), "pool-configmap.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := templates.Template(asset).Render(values)
	if err != nil {
		t.Fatalf("rendering the pool ConfigMap: %v", err)
	}
	var cm corev1.ConfigMap
	if err := yaml.Unmarshal(out, &cm); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return cm.Data
}

// A pool's process ConfigMap carries the pool's process keys and none of a member's (a member's are in
// its own env ConfigMap, read through the index). Its chart-level keys are the dedicated controller's,
// value for value: both come from the same helm templates (core-provider.cdcProcessEnvHead/Tail), and
// this is the test that they stay the same.
func TestPoolProcessConfigMap(t *testing.T) {
	pool := renderPoolConfigMap(t, map[string]any{
		"poolConfigMap": "cdc-pool-small-configmap", "namespace": "krateo-system", "pool": "small",
		"poolDeployment": "cdc-pool-small", "poolIndex": "cdc-pool-small-members", "poolWorkers": 10,
		"composition_controller_sa_name": "cdc-pool-small", "composition_controller_sa_namespace": "krateo-system",
		"authn_url": "http://authn.krateo-system.svc:8082",
	})
	for k, want := range map[string]string{
		"COMPOSITION_CONTROLLER_POOL_INDEX": "cdc-pool-small-members", "COMPOSITION_CONTROLLER_POOL_WORKERS": "10",
		"COMPOSITION_CONTROLLER_SA_NAME": "cdc-pool-small", "COMPOSITION_CONTROLLER_SA_NAMESPACE": "krateo-system",
		"URL_AUTHN": "http://authn.krateo-system.svc:8082", "DEPLOYMENT_NAME": "cdc-pool-small",
	} {
		if pool[k] != want {
			t.Errorf("pool %s = %q, want %q", k, pool[k], want)
		}
	}
	for _, k := range append(poolMemberKeys, "COMPOSITION_CONTROLLER_LEASE_NAME", "COMPOSITION_CONTROLLER_API_REF_NAME", "COMPOSITION_CONTROLLER_STATUS_DATA_TEMPLATE") {
		if _, ok := pool[k]; ok {
			t.Errorf("the pool's process ConfigMap carries the member key %s", k)
		}
	}

	dedicated := renderMemberConfigMap(t, nil)
	shared := 0
	// COMPOSITION_CONTROLLER_WORKERS comes from cdc.env (the fleet default, values.yaml): in a pool it is
	// the default a member without its own spec.controller.workers takes, never the pool's total.
	for _, k := range []string{"URL_CHART_INSPECTOR", "KRATEO_NAMESPACE", "COMPOSITION_CONTROLLER_HEALTH_PROBE_PORT",
		"COMPOSITION_CONTROLLER_HELM_TIMEOUT", "COMPOSITION_CONTROLLER_METRICS_SERVER_PORT", "COMPOSITION_CONTROLLER_WORKERS", "HOME"} {
		dv, ok := dedicated[k]
		if !ok {
			t.Fatalf("control: the dedicated ConfigMap has no %s, so this comparison checks nothing", k)
		}
		if pool[k] != dv {
			t.Errorf("%s: pool %q, dedicated %q; they come from one template and must agree", k, pool[k], dv)
		}
		shared++
	}
	t.Logf("%d chart-level keys compared; pool ConfigMap has %d keys", shared, len(pool))
}

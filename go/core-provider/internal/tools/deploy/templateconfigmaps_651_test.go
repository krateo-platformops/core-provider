package deploy

import (
	"context"
	"slices"
	"testing"

	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	contexttools "github.com/krateo-platformops/core-provider/internal/tools/context"
	kubecli "github.com/krateo-platformops/core-provider/internal/tools/kube"
)

// #651. The chart's own template ConfigMaps live in the namespace core-provider writes, reads and deletes
// its controllers' ConfigMaps in. Each path that touches ConfigMaps there is run with them present, under
// the names this chart renders AND the fixed names earlier charts rendered (helm deletes those last, so
// both can exist): none may be a candidate, and the path must still do its own job.
//
// The paths: every ConfigMap operation in this module is by one exact derived name, except the pool index
// listing, which is by label (compositiondefinitions, templateconfigmaps_651_test.go there).

const templatesNS = "krateo-system"

// chartTemplateNames are the template ConfigMap names of one rendering: this chart's and the earlier ones.
func chartTemplateNames(t *testing.T, values map[string]any) []string {
	t.Helper()
	var names []string
	for asset, cm := range templateConfigMaps(t, renderShippedChart(t, installerNamed(values))) {
		names = append(names, cm.Name, "installer-core-provider-"+asset)
	}
	if len(names) != 2*len(templateAssets) {
		t.Fatalf("%d template names, want %d", len(names), 2*len(templateAssets))
	}
	slices.Sort(names)
	return names
}

// seedTemplates adds the template ConfigMaps to a cluster, and returns the check that each survived
// untouched: still there, and no request of any kind named one.
func seedTemplates(t *testing.T, dyn *dynamicfake.FakeDynamicClient, names []string) func(path string) {
	t.Helper()
	for _, n := range names {
		if err := dyn.Tracker().Add(cmObj(templatesNS, n)); err != nil {
			t.Fatal(err)
		}
	}
	dyn.ClearActions()
	return func(path string) {
		t.Helper()
		for _, a := range dyn.Actions() {
			if a.GetResource() != configMapGVR {
				continue
			}
			switch n, ok := a.(interface{ GetName() string }); {
			case ok && slices.Contains(names, n.GetName()):
				t.Errorf("%s: %s of the chart's template ConfigMap %s", path, a.GetVerb(), n.GetName())
			case a.GetVerb() == "list" || a.GetVerb() == "deletecollection":
				t.Errorf("%s: %s of ConfigMaps: this path is by name", path, a.GetVerb())
			}
		}
		for _, n := range names {
			if _, err := dyn.Tracker().Get(configMapGVR, templatesNS, n); err != nil {
				t.Errorf("%s: the chart's template ConfigMap %s is gone: %v", path, n, err)
			}
		}
	}
}

func forEachRendering(t *testing.T, f func(t *testing.T, names []string)) {
	for _, r := range chartRenderings {
		t.Run(r.name, func(t *testing.T) { f(t, chartTemplateNames(t, r.values)) })
	}
}

// No name core-provider derives can equal a template ConfigMap's: derived ConfigMap names end in
// "-configmap" or "-members", a template's in "-" and 10 hex digits. (The fixed names of earlier charts
// did end in "-configmap".)
func TestDerivedConfigMapNamesAreNeverATemplates(t *testing.T) {
	// The closest a definition can come: a Kind whose plural is the chart's own prefix, and a version
	// that ends like a digest.
	var derived []string
	for _, gvr := range []schema.GroupVersionResource{
		portalsGVR,
		{Group: "composition.krateo.io", Version: "v1-0-0", Resource: "installer-core-provider"},
		{Group: "composition.krateo.io", Version: "v1-0-0-0123456789", Resource: "installer-core-provider-cdc"},
	} {
		nn := types.NamespacedName{Namespace: templatesNS, Name: resourceNamer(gvr.Resource, gvr.Version)}
		derived = append(derived, getCDCConfigmapNN(nn).Name, getJsonSchemaConfigmapNN(nn).Name,
			DefinitionSchemaConfigMapNN("tenant-a", gvr).Name, MemberEnvConfigMapName(gvr))
	}
	for _, pool := range []string{"general", "0123456789"} {
		derived = append(derived, PoolNames(pool).EnvConfigMap, PoolNames(pool).Index)
	}
	for _, d := range derived {
		if digestName.MatchString(d) {
			t.Errorf("%q has a template ConfigMap's form", d)
		}
	}
	for _, r := range chartRenderings {
		for _, cm := range templateConfigMaps(t, renderShippedChart(t, installerNamed(r.values))) {
			if !digestName.MatchString(cm.Name) || slices.Contains(derived, cm.Name) {
				t.Errorf("%s: %s is not a digest name apart from the derived ones", r.name, cm.Name)
			}
		}
	}
}

// Path: removing the legacy schema copy from the controller namespace (a hand-over, or a definition's
// move), and Deploy's schema write, which does it.
func TestTemplatesSurviveTheLegacySchemaRemoval(t *testing.T) {
	ctx := context.Background()
	legacy := getJsonSchemaConfigmapNN(types.NamespacedName{Namespace: templatesNS, Name: "portals-v1-8-127"})
	forEachRendering(t, func(t *testing.T, names []string) {
		hub := hubWith(cmObj(templatesNS, legacy.Name))
		survived := seedTemplates(t, hub, names)
		if err := removeLegacySchemaConfigMap(ctx, hub, legacy); err != nil {
			t.Fatal(err)
		}
		if keepSharedExists(t, hub, configMapGVR, templatesNS, legacy.Name) {
			t.Error("the legacy ConfigMap was not removed")
		}
		survived("removeLegacySchemaConfigMap")
	})
	forEachRendering(t, func(t *testing.T, names []string) {
		hub := hubWith(cmObj(templatesNS, legacy.Name))
		survived := seedTemplates(t, hub, names)
		opts := DeployOptions{GVR: portalsGVR, Namespace: "tenant-a", ControllerNamespace: templatesNS,
			HubDynClient: hub, DynClient: hubWith(),
			JsonSchemaTemplatePath: "testdata/configmap_jsonschema_production.yaml", JsonSchemaBytes: []byte(`{"type":"object"}`)}
		if _, err := applySchemaConfigMap(ctx, opts, types.NamespacedName{Namespace: templatesNS, Name: "portals-v1-8-127"}, kubecli.ApplyOptions{}, logging.NewNopLogger()); err != nil {
			t.Fatal(err)
		}
		if keepSharedExists(t, hub, configMapGVR, templatesNS, legacy.Name) {
			t.Error("the legacy ConfigMap was not removed")
		}
		survived("applySchemaConfigMap")
	})
}

// Path: a follower's own schema copy, written, read and removed by its exact name.
func TestTemplatesSurviveAFollowersSchemaCopy(t *testing.T) {
	ctx := context.Background()
	const tmpl = "testdata/configmap_jsonschema_production.yaml"
	body := []byte(`{"type":"object"}`)
	forEachRendering(t, func(t *testing.T, names []string) {
		hub := hubWith()
		survived := seedTemplates(t, hub, names)
		if err := ApplyDefinitionSchemaConfigMap(ctx, hub, portalsGVR, templatesNS, tmpl, body); err != nil {
			t.Fatal(err)
		}
		if ok, why, err := DefinitionSchemaConfigMapCurrent(ctx, hub, portalsGVR, templatesNS, tmpl, body); err != nil || !ok {
			t.Fatalf("current = %v (%s), %v", ok, why, err)
		}
		if err := RemoveDefinitionSchemaConfigMap(ctx, hub, portalsGVR, templatesNS); err != nil {
			t.Fatal(err)
		}
		if ok, err := DefinitionSchemaConfigMapExists(ctx, hub, portalsGVR, templatesNS); err != nil || ok {
			t.Fatalf("exists after removal = %v, %v", ok, err)
		}
		survived("the definition's own schema ConfigMap")
	})
}

// Path: Undeploy, which retirement (retire.go), the leaked-controller reclaim (reclaimleaked.go), the
// controller relocation and the definition's deletion all end in.
func TestTemplatesSurviveUndeploy(t *testing.T) {
	for name, opts := range map[string]UndeployOptions{
		"the last definition":     {},
		"a hand-over, KeepShared": {KeepShared: true},
		"a relocation":            {Relocating: true},
	} {
		t.Run(name, func(t *testing.T) {
			forEachRendering(t, func(t *testing.T, names []string) {
				dyn := relocatingCluster()
				survived := seedTemplates(t, dyn, names)
				relocatingUndeploy(t, dyn, opts)
				if keepSharedExists(t, dyn, configMapGVR, keepSharedOldNS, keepSharedName+"-configmap") {
					t.Error("the controller's own env ConfigMap was left behind")
				}
				survived("Undeploy")
			})
		})
	}
}

// Path: a pool's teardown, by its exact names.
func TestTemplatesSurvivePoolDeletion(t *testing.T) {
	forEachRendering(t, func(t *testing.T, names []string) {
		p := PoolNames("general")
		dyn := hubWith(cmObj(templatesNS, p.Index), cmObj(templatesNS, p.EnvConfigMap))
		survived := seedTemplates(t, dyn, names)
		if err := DeletePool(context.Background(), dyn, "general", templatesNS); err != nil {
			t.Fatal(err)
		}
		for _, n := range []string{p.Index, p.EnvConfigMap} {
			if keepSharedExists(t, dyn, configMapGVR, templatesNS, n) {
				t.Errorf("the pool's %s was left behind", n)
			}
		}
		survived("DeletePool")
	})
}

// Path: joining a pool, and the sweep of dangling controller bindings: neither touches a ConfigMap.
func TestPathsThatNeverTouchConfigMaps(t *testing.T) {
	ctx := contexttools.CtxWithLogger(context.Background(), logging.NewNopLogger())
	none := func(t *testing.T, path string, actions []clienttesting.Action) {
		t.Helper()
		if len(actions) == 0 {
			t.Errorf("%s made no request: nothing was shown", path)
		}
		for _, a := range actions {
			if a.GetResource() == configMapGVR {
				t.Errorf("%s: %s of ConfigMaps", path, a.GetVerb())
			}
		}
	}
	t.Run("cutoverToPool", func(t *testing.T) {
		dyn, controller, sa := cutoverCluster(t)
		opts := DeployOptions{GVR: oldPortalGVR, Pool: "general", DynClient: dyn, KubeClient: clientfake.NewClientBuilder().Build()}
		if err := cutoverToPool(ctx, opts, controller, sa); err != nil {
			t.Fatal(err)
		}
		none(t, "cutoverToPool", dyn.Actions())
	})
	t.Run("dangling bindings", func(t *testing.T) {
		dyn := danglingClient(dBinding("tenant-a", "b", "Role", "r", nil, mgmtNS+"/portals-v1-0-0"), dRole("tenant-a", "r", nil))
		found, _, err := FindDanglingControllerBindings(ctx, portalsOpts(dyn))
		if err != nil || len(found) != 1 {
			t.Fatalf("found %v, %v; want the one dangling binding", found, err)
		}
		if _, err := RemoveDanglingControllerBindings(ctx, dyn, found); err != nil {
			t.Fatal(err)
		}
		none(t, "the dangling-binding sweep", dyn.Actions())
	})
}

// Path: the chart-inspector projection copies to a spoke the ConfigMaps its Deployment names. The
// controller templates are core-provider's, never chart-inspector's, so none is projected.
func TestChartInspectorReferencesNoTemplateConfigMap(t *testing.T) {
	for _, r := range chartRenderings {
		manifests := renderShippedChart(t, installerNamed(r.values))
		var dep struct {
			Spec struct {
				Template struct{ Spec corev1.PodSpec }
			}
		}
		if err := yaml.Unmarshal([]byte(manifests["core-provider/templates/chart-inspector/deployment.yaml"]), &dep); err != nil {
			t.Fatal(err)
		}
		cms, _ := referencedConfig(&dep.Spec.Template.Spec)
		if !slices.Equal(cms, []string{"installer-core-provider-chart-inspector"}) {
			t.Errorf("%s: chart-inspector references %v, want only its own env ConfigMap", r.name, cms)
		}
	}
}

// Path: the pool index listing selects by label. No template ConfigMap may carry it.
func TestTemplatesCarryNoPoolLabel(t *testing.T) {
	for _, r := range chartRenderings {
		for asset, cm := range templateConfigMaps(t, renderShippedChart(t, installerNamed(r.values))) {
			if v, ok := cm.Labels[PoolLabel]; ok {
				t.Errorf("%s: the %s ConfigMap carries %s=%s: the pool index listing would select it", r.name, asset, PoolLabel, v)
			}
			if len(cm.Labels) == 0 {
				t.Errorf("%s: the %s ConfigMap carries no labels: nothing was shown", r.name, asset)
			}
		}
	}
}

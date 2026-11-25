package deploy

import (
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/krateo-platformops/core-provider/internal/tools/objects"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/yaml"
)

// #651. The ConfigMaps core-provider reads its controller templates from are immutable and named by
// the digest of their own data, and the Deployment's volumes name them, so a pod can only ever read
// the templates its own spec names. The kubelet half (a running pod keeps its files when the ConfigMap
// is gone) is exercised on kind: hack/upgrade-templates-e2e.

// digestName is what separates a template ConfigMap's name from every name core-provider derives.
var digestName = regexp.MustCompile(`-[0-9a-f]{10}$`)

// templateAssets are the chart's template ConfigMaps: the asset part of the name, and the template
// file that renders it.
var templateAssets = map[string]string{
	"cdc-deployment":       "cdc-deployment.yaml",
	"cdc-configmap":        "cdc-configmap.yaml",
	"rbac-configmap":       "cdc-rbac.yaml",
	"jsonschema-configmap": "jsonschema-configmap.yaml",
	"service-configmap":    "cdc-service.yaml",
}

// chartRenderings are the two shapes an install has: krateo-057 has no controller metrics, so it mounts
// no Service template.
var chartRenderings = []struct {
	name    string
	values  map[string]any
	mounted int
}{
	{"cdc.metrics off (the default, krateo-057)", nil, 4},
	{"cdc.metrics on", map[string]any{"cdc": map[string]any{"metrics": map[string]any{"enabled": true}}}, 5},
}

// installerNamed renders with the fullname the installer umbrella gives core-provider.
func installerNamed(values map[string]any) map[string]any {
	out := map[string]any{"fullnameOverride": "installer-core-provider"}
	maps.Copy(out, values)
	return out
}

// templateConfigMaps decodes the chart's template ConfigMaps from rendered manifests, keyed by asset.
func templateConfigMaps(t *testing.T, manifests map[string]string) map[string]corev1.ConfigMap {
	t.Helper()
	out := map[string]corev1.ConfigMap{}
	for asset, file := range templateAssets {
		body, ok := manifests["core-provider/templates/"+file]
		if !ok {
			t.Fatalf("the chart rendered no %s", file)
		}
		var cm corev1.ConfigMap
		if err := yaml.UnmarshalStrict([]byte(body), &cm); err != nil {
			t.Fatalf("decoding %s: %v", file, err)
		}
		if len(cm.Data) == 0 {
			t.Fatalf("%s carries no data", file)
		}
		out[asset] = cm
	}
	return out
}

func coreProviderDeployment(t *testing.T, manifests map[string]string) appsv1.Deployment {
	t.Helper()
	var dep appsv1.Deployment
	if err := yaml.Unmarshal([]byte(manifests["core-provider/templates/deployment.yaml"]), &dep); err != nil {
		t.Fatal(err)
	}
	if len(dep.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("core-provider's Deployment has %d containers, want 1", len(dep.Spec.Template.Spec.Containers))
	}
	return dep
}

// chartCopy copies the shipped chart into a temp directory, with edit applied to each file.
func chartCopy(t *testing.T, edit func(rel string, b []byte) []byte) string {
	t.Helper()
	dir := t.TempDir()
	err := filepath.WalkDir(shippedChart, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(shippedChart, p)
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dir, rel), 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, rel), edit(filepath.ToSlash(rel), b), 0o644)
	})
	if err != nil {
		t.Fatalf("copying the chart: %v", err)
	}
	return dir
}

// Every template volume names a ConfigMap the chart renders, which is immutable and digest-named, and
// nothing about the mount lets the kubelet swap the files under a running process: `optional` would make
// it BLANK them once helm deletes the old name, and a subPath would pin them for another reason.
func TestTemplateVolumesNameImmutableContentAddressedConfigMaps(t *testing.T) {
	for _, r := range chartRenderings {
		t.Run(r.name, func(t *testing.T) {
			manifests := renderShippedChart(t, installerNamed(r.values))
			byName := map[string]string{}
			for asset, cm := range templateConfigMaps(t, manifests) {
				if cm.Immutable == nil || !*cm.Immutable {
					t.Errorf("%s is not immutable", cm.Name)
				}
				if !digestName.MatchString(cm.Name) || !strings.HasPrefix(cm.Name, "installer-core-provider-"+asset+"-") {
					t.Errorf("%s: want installer-core-provider-%s-<10 hex>", cm.Name, asset)
				}
				byName[cm.Name] = asset
			}

			pod := coreProviderDeployment(t, manifests).Spec.Template.Spec
			mounts := map[string]corev1.VolumeMount{}
			for _, m := range pod.Containers[0].VolumeMounts {
				mounts[m.Name] = m
			}
			seen := map[string]bool{}
			for _, v := range pod.Volumes {
				m, ok := mounts[v.Name]
				if !ok || !strings.HasPrefix(m.MountPath, "/tmp/assets/") {
					if v.ConfigMap != nil && byName[v.ConfigMap.Name] != "" {
						t.Errorf("volume %s names template ConfigMap %s but is not mounted under /tmp/assets", v.Name, v.ConfigMap.Name)
					}
					continue
				}
				if v.ConfigMap == nil {
					t.Fatalf("volume %s under %s is not a ConfigMap", v.Name, m.MountPath)
				}
				if byName[v.ConfigMap.Name] == "" {
					t.Errorf("volume %s names ConfigMap %q, which the chart does not render as a template ConfigMap (%v)", v.Name, v.ConfigMap.Name, byName)
				}
				if v.ConfigMap.Optional != nil {
					t.Errorf("volume %s sets optional: the kubelet would blank the files once the ConfigMap is gone", v.Name)
				}
				if m.SubPath != "" || m.SubPathExpr != "" || len(v.ConfigMap.Items) != 0 {
					t.Errorf("volume %s is mounted with a subPath or items", v.Name)
				}
				seen[v.ConfigMap.Name] = true
			}
			if len(seen) != r.mounted {
				t.Errorf("%d template ConfigMaps mounted (%v), want %d", len(seen), seen, r.mounted)
			}
		})
	}
}

// The name changes exactly when the data does, for each ConfigMap on its own: over user values, over
// every shipped asset file, and over the template that assembles the data. Same name with different data
// is an upgrade the apiserver refuses (the ConfigMap is immutable); a different name with the same data
// would only roll core-provider for nothing.
func TestTemplateConfigMapNameFollowsItsData(t *testing.T) {
	type rendering struct {
		name string
		cms  map[string]corev1.ConfigMap
	}
	var all []rendering
	add := func(name, dir string, values map[string]any) {
		all = append(all, rendering{name, templateConfigMaps(t, renderChartAt(t, dir, values))})
	}
	cdc := func(kv ...any) map[string]any {
		m := map[string]any{}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return map[string]any{"cdc": m}
	}
	add("base", shippedChart, nil)
	add("base again", shippedChart, nil)
	for name, v := range map[string]map[string]any{
		"replicaCount of core-provider": {"replicaCount": 2},
		"cdc.metrics on":                cdc("metrics", map[string]any{"enabled": true}),
		"cdc.metrics.port":              cdc("metrics", map[string]any{"enabled": true, "port": 9191}),
		"cdc.workers":                   cdc("workers", 7),
		"cdc.resyncInterval":            cdc("resyncInterval", "2m"),
		"cdc.image.tag":                 cdc("image", map[string]any{"tag": "1.2.3"}),
		"cdc.env":                       cdc("env", map[string]any{"SOMETHING": "else"}),
		"cdc.pool workers":              cdc("pool", map[string]any{"default": map[string]any{"workers": 77}}),
		"cdc.helmTimeout":               cdc("helmTimeout", "9m"),
		"cdc.nodeSelector":              cdc("nodeSelector", map[string]any{"a": "b"}),
		"otel":                          {"otel": map[string]any{"enabled": true}},
		"global.imageRegistry":          {"global": map[string]any{"imageRegistry": "mirror.example"}},
	} {
		add(name, shippedChart, v)
	}

	var assets []string
	if err := filepath.WalkDir(filepath.Join(shippedChart, "assets"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(shippedChart, p)
			assets = append(assets, filepath.ToSlash(rel))
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	touched := map[string]bool{} // "asset/key" whose data a perturbed asset file changed
	base := all[0].cms
	for _, f := range assets {
		dir := chartCopy(t, func(rel string, b []byte) []byte {
			if rel == f {
				return append(b, []byte("\n# perturbed\n")...)
			}
			return b
		})
		add("asset "+f, dir, nil)
		changed := 0
		for asset, cm := range all[len(all)-1].cms {
			for k, v := range cm.Data {
				if v != base[asset].Data[k] {
					touched[asset+"/"+k] = true
					changed++
				}
			}
		}
		if changed != 1 {
			t.Errorf("editing %s changed %d data keys, want exactly 1", f, changed)
		}
	}
	// Every data key is a shipped file, except the pool sizing, which is a value (perturbed above).
	for asset, cm := range base {
		for _, k := range slices.Sorted(maps.Keys(cm.Data)) {
			if !touched[asset+"/"+k] && k != "pool-sizing.json" {
				t.Errorf("no asset file reaches %s/%s", asset, k)
			}
		}
	}
	// A key added by the template that assembles the data, with no asset file behind it.
	add("a data key added in _assets.tpl", chartCopy(t, func(rel string, b []byte) []byte {
		if rel != "templates/_assets.tpl" {
			return b
		}
		const at = `{{- define "core-provider.assetData.cdc-deployment" -}}`
		if strings.Count(string(b), at) != 1 {
			t.Fatalf("_assets.tpl has no single %s", at)
		}
		return []byte(strings.Replace(string(b), at, at+"\nextra.txt: added\n", 1))
	}), nil)
	// A release: only the chart's version moves, which the labels carry and the data (with the image tag
	// pinned) does not.
	for _, v := range []string{"0.0.1", "0.0.2"} {
		add("version "+v, chartCopy(t, func(rel string, b []byte) []byte {
			if rel == "Chart.yaml" {
				b = []byte(strings.NewReplacer("CHART_VERSION", v, "APP_VERSION", v).Replace(string(b)))
			}
			return b
		}), cdc("image", map[string]any{"tag": "pinned"}))
	}

	pairs, distinct := 0, map[string]map[string]bool{}
	for i := range all {
		for j := i + 1; j < len(all); j++ {
			for asset := range templateAssets {
				a, b := all[i].cms[asset], all[j].cms[asset]
				sameData, sameName := reflect.DeepEqual(a.Data, b.Data), a.Name == b.Name
				if sameData != sameName {
					t.Errorf("%s, %q against %q: same data %v, same name %v (%s, %s)", asset, all[i].name, all[j].name, sameData, sameName, a.Name, b.Name)
				}
				pairs++
			}
		}
		for asset := range templateAssets {
			if distinct[asset] == nil {
				distinct[asset] = map[string]bool{}
			}
			distinct[asset][all[i].cms[asset].Name] = true
		}
	}
	for asset, names := range distinct {
		if len(names) < 2 {
			t.Errorf("%s: no rendering changed it, so nothing was shown about its name", asset)
		}
	}
	t.Logf("%d renderings, %d pairs compared", len(all), pairs)
}

// The checksum annotation covers the one ConfigMap the pod reads that is NOT named by its content: the
// env ConfigMap, which envFrom reads at container start only.
func TestEnvConfigMapStillRollsCoreProvider(t *testing.T) {
	sum := func(values map[string]any) string {
		dep := coreProviderDeployment(t, renderShippedChart(t, values))
		s := dep.Spec.Template.Annotations["checksum/configmap"]
		if s == "" {
			t.Fatal("no checksum/configmap annotation")
		}
		return s
	}
	base := sum(nil)
	if got := sum(map[string]any{"env": map[string]any{"CORE_PROVIDER_POLL_INTERVAL": "30s"}}); got == base {
		t.Error("an env change did not change the pod template")
	}
	if got := sum(nil); got != base {
		t.Error("the checksum is not stable")
	}
}

// strategy and priorityClassName: absent by default, so Kubernetes' defaults apply; rendered when set.
func TestDeploymentStrategyAndPriorityClassAreOptIn(t *testing.T) {
	dep := coreProviderDeployment(t, renderShippedChart(t, nil))
	if !reflect.DeepEqual(dep.Spec.Strategy, appsv1.DeploymentStrategy{}) || dep.Spec.Template.Spec.PriorityClassName != "" {
		t.Errorf("by default: strategy %+v, priorityClassName %q; want neither rendered", dep.Spec.Strategy, dep.Spec.Template.Spec.PriorityClassName)
	}
	dep = coreProviderDeployment(t, renderShippedChart(t, map[string]any{
		"priorityClassName": "krateo-critical",
		"strategy":          map[string]any{"type": "RollingUpdate", "rollingUpdate": map[string]any{"maxSurge": 0, "maxUnavailable": 1}},
	}))
	ru := dep.Spec.Strategy.RollingUpdate
	if dep.Spec.Strategy.Type != appsv1.RollingUpdateDeploymentStrategyType || ru == nil || ru.MaxSurge.IntValue() != 0 || ru.MaxUnavailable.IntValue() != 1 {
		t.Errorf("strategy = %+v", dep.Spec.Strategy)
	}
	if got := dep.Spec.Template.Spec.PriorityClassName; got != "krateo-critical" {
		t.Errorf("priorityClassName = %q", got)
	}
}

// THE PER-DEFINITION ConfigMap is another object, and its name is a contract: installer's chart builds
// `<plural>-<version>-jsonschema-configmap` for its read grants, and the portal reads it by that name.
// Content addressing is for the chart's own ConfigMaps only.
func TestDefinitionSchemaConfigMapNameIsUnchanged(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v0-2-61", Resource: "selfservicekrateoes"}
	const want = "selfservicekrateoes-v0-2-61-jsonschema-configmap"
	for _, r := range chartRenderings {
		tmpl := mountedTemplate(t, renderShippedChart(t, r.values), "jsonschema-configmap.yaml", "configmap.yaml")
		var cm corev1.ConfigMap
		if err := objects.CreateK8sObject(&cm, gvr, types.NamespacedName{Namespace: "tenant-a", Name: "ignored"}, tmpl, "schema", `{"type":"object"}`); err != nil {
			t.Fatal(err)
		}
		if cm.Name != want || cm.Namespace != "tenant-a" {
			t.Errorf("%s: rendered %s/%s, want tenant-a/%s", r.name, cm.Namespace, cm.Name, want)
		}
		if got := fmt.Sprint(slices.Sorted(maps.Keys(cm.Data))); got != "[values.schema.json]" {
			t.Errorf("%s: data keys %s, want [values.schema.json]", r.name, got)
		}
	}
	// The lookups and deletions use this derivation, so it must give the rendered name.
	if nn := DefinitionSchemaConfigMapNN("tenant-a", gvr); nn.Name != want || nn.Namespace != "tenant-a" {
		t.Errorf("DefinitionSchemaConfigMapNN = %s, want tenant-a/%s", nn, want)
	}
}

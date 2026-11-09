package compositiondefinitions

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	"helm.sh/helm/v3/pkg/releaseutil"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// cdcImageEnv names the composition-dynamic-controller image the suite runs. It is REQUIRED: the
// integration workflow builds it from the commit under test and loads it into kind.
//
// Before #404 the suite rendered cdcs from its own fixture (testdata/manifests), pinned to
// composition-dynamic-controller:2.13.31 and drifted from the chart: it still passed
// -namespace={{ .namespace }}, which the shipped template dropped. Once #313 put the controller in the
// management namespace, that flag made it watch ONLY the management namespace, so it never saw a
// composition. No earlier test needed a cdc to work, so nothing noticed. Rendering the chart's own
// templates with the commit's own image means the suite runs what ships.
const cdcImageEnv = "CDC_IMAGE"

// chartDir is helm/core-provider, from this package's directory.
var chartDir = filepath.Join("..", "..", "..", "..", "..", "helm", "core-provider")

const assetsMountPrefix = "/tmp/assets/"

// loadChartForTest loads the chart directory with Chart.yaml's release-time placeholders
// (CHART_VERSION, APP_VERSION) substituted by 0.0.0, exactly as lint.yaml does before `helm lint`.
// The loader rejects the placeholders as invalid semver. Nothing on disk is changed.
func loadChartForTest(dir string) (*chart.Chart, error) {
	var files []*loader.BufferedFile
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "Chart.yaml" {
			data = []byte(strings.NewReplacer("CHART_VERSION", "0.0.0", "APP_VERSION", "0.0.0").Replace(string(data)))
		}
		files = append(files, &loader.BufferedFile{Name: rel, Data: data})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return loader.LoadFiles(files)
}

// renderChartAssets renders helm/core-provider with the cdc image overridden to image, and writes
// every ConfigMap the core-provider Deployment mounts under /tmp/assets/ to the same place under
// root. The suite passes os.TempDir()/assets, which is where the controller reads them
// (CDCtemplateDeploymentPath and friends).
// The mapping is read from the rendered Deployment's own volumes, so a new asset mount is picked up
// rather than silently missing. It returns the files written, and fails if none were, if a mount
// has no ConfigMap, or if the rendered cdc Deployment does not name image.
func renderChartAssets(root, releaseNamespace, image string) ([]string, error) {
	i := strings.LastIndex(image, ":")
	if i <= 0 || strings.Contains(image[i:], "/") {
		return nil, fmt.Errorf("%s=%q: want repository:tag", cdcImageEnv, image)
	}
	repo, tag := image[:i], image[i+1:]

	chrt, err := loadChartForTest(chartDir)
	if err != nil {
		return nil, fmt.Errorf("load chart %s: %w", chartDir, err)
	}
	vals := map[string]any{"cdc": map[string]any{
		// registry "" so krateo.image renders "repository:tag", the name kind loaded.
		"image": map[string]any{"registry": "", "repository": repo, "tag": tag, "pullPolicy": "IfNotPresent"},
		// The fixture always supplied the Service template, and deploy only creates the Service when
		// that file exists. Keep that path exercised.
		"metrics": map[string]any{"enabled": true},
	}}
	rv, err := chartutil.ToRenderValues(chrt, vals, chartutil.ReleaseOptions{
		Name: "core-provider", Namespace: releaseNamespace, IsInstall: true,
	}, chartutil.DefaultCapabilities)
	if err != nil {
		return nil, fmt.Errorf("chart values: %w", err)
	}
	rendered, err := engine.Render(chrt, rv)
	if err != nil {
		return nil, fmt.Errorf("render chart: %w", err)
	}

	configMaps := map[string]corev1.ConfigMap{}
	var provider *appsv1.Deployment
	for name, body := range rendered {
		if !strings.HasSuffix(name, ".yaml") {
			continue
		}
		for _, doc := range releaseutil.SplitManifests(body) {
			var head struct{ Kind string }
			if err := yaml.Unmarshal([]byte(doc), &head); err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			switch head.Kind {
			case "ConfigMap":
				var cm corev1.ConfigMap
				if err := yaml.Unmarshal([]byte(doc), &cm); err != nil {
					return nil, fmt.Errorf("%s: %w", name, err)
				}
				configMaps[cm.Name] = cm
			case "Deployment":
				var d appsv1.Deployment
				if err := yaml.Unmarshal([]byte(doc), &d); err != nil {
					return nil, fmt.Errorf("%s: %w", name, err)
				}
				for _, c := range d.Spec.Template.Spec.Containers {
					for _, m := range c.VolumeMounts {
						if strings.HasPrefix(m.MountPath, assetsMountPrefix) {
							provider = d.DeepCopy()
						}
					}
				}
			}
		}
	}
	if provider == nil {
		return nil, fmt.Errorf("the rendered chart has no Deployment mounting %s*", assetsMountPrefix)
	}
	volumeCM := map[string]string{}
	for _, v := range provider.Spec.Template.Spec.Volumes {
		if v.ConfigMap != nil {
			volumeCM[v.Name] = v.ConfigMap.Name
		}
	}

	var written []string
	for _, c := range provider.Spec.Template.Spec.Containers {
		for _, m := range c.VolumeMounts {
			if !strings.HasPrefix(m.MountPath, assetsMountPrefix) {
				continue
			}
			cmName, ok := volumeCM[m.Name]
			if !ok {
				return nil, fmt.Errorf("mount %s (volume %s) is not a ConfigMap", m.MountPath, m.Name)
			}
			cm, ok := configMaps[cmName]
			if !ok {
				return nil, fmt.Errorf("mount %s names ConfigMap %s, which the chart does not render", m.MountPath, cmName)
			}
			dir := filepath.Join(root, strings.TrimPrefix(m.MountPath, assetsMountPrefix))
			if err := os.MkdirAll(dir, os.ModePerm); err != nil {
				return nil, err
			}
			for key, data := range cm.Data {
				p := filepath.Join(dir, key)
				if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
					return nil, err
				}
				written = append(written, p)
			}
		}
	}
	sort.Strings(written)
	if len(written) == 0 {
		return nil, fmt.Errorf("rendered %d ConfigMaps but wrote no asset file", len(configMaps))
	}

	// The control: the template core-provider will deploy cdcs from must name the image under test.
	depPath := filepath.Join(root, "cdc-deployment", filepath.Base(CDCtemplateDeploymentPath))
	dep, err := os.ReadFile(depPath)
	if err != nil {
		return nil, fmt.Errorf("the rendered assets have no %s: %w", depPath, err)
	}
	if !strings.Contains(string(dep), `"`+image+`"`) {
		return nil, fmt.Errorf("%s does not name the image under test %q", depPath, image)
	}
	return written, nil
}

// TestRenderChartAssets renders the real chart, as the integration suite does, and pins the two
// properties #404 lost two CI runs to: the cdc template names the image under test, and it does not
// restrict the controller's watch with -namespace (the fixture's flag that, once #313 moved the
// controller, made it watch only the management namespace).
func TestRenderChartAssets(t *testing.T) {
	root := t.TempDir()
	const image = "composition-dynamic-controller:unit"
	written, err := renderChartAssets(root, "krateo-mgmt-test", image)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %d asset files", len(written))
	for _, want := range []string{
		filepath.Base(filepath.Dir(CDCtemplateDeploymentPath)) + "/" + filepath.Base(CDCtemplateDeploymentPath),
		filepath.Base(filepath.Dir(CDCtemplateConfigmapPath)) + "/" + filepath.Base(CDCtemplateConfigmapPath),
		filepath.Base(filepath.Dir(JSONSchemaTemplateConfigmapPath)) + "/" + filepath.Base(JSONSchemaTemplateConfigmapPath),
		filepath.Base(filepath.Dir(ServiceTemplatePath)) + "/" + filepath.Base(ServiceTemplatePath),
		filepath.Base(CDCrbacConfigFolder) + "/serviceaccount.yaml",
	} {
		if _, err := os.Stat(filepath.Join(root, want)); err != nil {
			t.Errorf("asset %s not written: %v", want, err)
		}
	}
	dep, err := os.ReadFile(filepath.Join(root, "cdc-deployment", filepath.Base(CDCtemplateDeploymentPath)))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(dep), "-namespace=") {
		t.Errorf("the cdc template restricts the watch with -namespace=; a controller in the management namespace would see no composition")
	}
	// Control, a differential: a render with another image names THAT image and not this one, so the
	// image is taken from the input rather than from anything fixed in the chart.
	root2 := t.TempDir()
	if _, err := renderChartAssets(root2, "krateo-mgmt-test", "other-cdc:tag"); err != nil {
		t.Fatalf("control render: %v", err)
	}
	dep2, err := os.ReadFile(filepath.Join(root2, "cdc-deployment", filepath.Base(CDCtemplateDeploymentPath)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(dep2), `"other-cdc:tag"`) || strings.Contains(string(dep2), image) {
		t.Fatalf("control: the render with other-cdc:tag does not name it alone")
	}
}

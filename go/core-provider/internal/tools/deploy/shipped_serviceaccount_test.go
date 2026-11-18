package deploy

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	"sigs.k8s.io/yaml"
)

// renderShippedChartE is renderShippedChart returning the render error instead of failing, so a
// refusal by the chart itself can be asserted.
func renderShippedChartE(t *testing.T, overrides map[string]any) (map[string]string, error) {
	t.Helper()
	dir := t.TempDir()
	if err := filepath.WalkDir(shippedChart, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(shippedChart, p)
		if err != nil {
			return err
		}
		dst := filepath.Join(dir, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if rel == "Chart.yaml" {
			b = []byte(strings.NewReplacer("CHART_VERSION", "0.0.0", "APP_VERSION", "0.0.0").Replace(string(b)))
		}
		return os.WriteFile(dst, b, 0o644)
	}); err != nil {
		t.Fatal(err)
	}
	chrt, err := loader.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	caps := chartutil.DefaultCapabilities.Copy()
	caps.KubeVersion = chartutil.KubeVersion{Version: "v1.36.0", Major: "1", Minor: "36"}
	// The values schema is checked here, so a schema refusal is returned too: it is the chart refusing.
	vals, err := chartutil.ToRenderValues(chrt, overrides,
		chartutil.ReleaseOptions{Name: "core-provider", Namespace: "krateo-system", IsInstall: true}, caps)
	if err != nil {
		return nil, err
	}
	out, err := engine.Render(chrt, vals)
	if err != nil {
		return nil, err
	}
	manifests, _ := splitNotes(chrt.Name(), out)
	return manifests, nil
}

// serviceAccountUse is who the chart's ServiceAccounts, workloads and bindings name.
type serviceAccountUse struct {
	rendered []string // ServiceAccounts the chart creates
	runAs    []string // each Deployment's serviceAccountName
	bound    []string // each ClusterRoleBinding's ServiceAccount subjects, namespace/name
}

func serviceAccountsOf(t *testing.T, manifests map[string]string) serviceAccountUse {
	t.Helper()
	var u serviceAccountUse
	for path, manifest := range manifests {
		for _, doc := range strings.Split(manifest, "\n---") {
			var obj map[string]any
			if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			md, _ := obj["metadata"].(map[string]any)
			switch obj["kind"] {
			case "ServiceAccount":
				u.rendered = append(u.rendered, md["name"].(string))
			case "Deployment":
				spec := obj["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
				name, _ := spec["serviceAccountName"].(string)
				u.runAs = append(u.runAs, md["name"].(string)+"="+name)
			case "ClusterRoleBinding":
				subjects, _ := obj["subjects"].([]any)
				for _, s := range subjects {
					sm := s.(map[string]any)
					if sm["kind"] == "ServiceAccount" {
						u.bound = append(u.bound, md["name"].(string)+"="+sm["namespace"].(string)+"/"+sm["name"].(string))
					}
				}
			}
		}
	}
	return u
}

// serviceAccount.create=false means "use a ServiceAccount created outside the chart", so its name is
// required. With no name the chart used to fall back to the namespace's shared `default` account,
// render a ServiceAccount for chart-inspector anyway, and bind core-provider's ClusterRole to an
// account its Deployment did not run as. Now the combination is refused at render, and a named
// account is used consistently by both Deployments and both bindings.
func TestShippedChart_NeverBindsTheDefaultServiceAccount(t *testing.T) {
	t.Run("create=false with no name: refused by the chart", func(t *testing.T) {
		_, err := renderShippedChartE(t, map[string]any{"serviceAccount": map[string]any{"create": false}})
		if err == nil || !strings.Contains(err.Error(), "serviceAccount.name is required when serviceAccount.create is false") {
			t.Fatalf("render err = %v, want the chart's refusal", err)
		}
	})

	t.Run("create=false with a name: that account everywhere, none created", func(t *testing.T) {
		m, err := renderShippedChartE(t, map[string]any{"serviceAccount": map[string]any{"create": false, "name": "pre-made"}})
		if err != nil {
			t.Fatal(err)
		}
		u := serviceAccountsOf(t, m)
		if len(u.rendered) != 0 {
			t.Errorf("the chart created ServiceAccounts %v with create=false", u.rendered)
		}
		if len(u.runAs) != 2 || len(u.bound) != 2 {
			t.Fatalf("control: want 2 Deployments and 2 ServiceAccount bindings, got %v / %v", u.runAs, u.bound)
		}
		for _, x := range append(append([]string{}, u.runAs...), u.bound...) {
			if !strings.HasSuffix(x, "=pre-made") && !strings.HasSuffix(x, "=krateo-system/pre-made") {
				t.Errorf("%s does not use the pre-made account", x)
			}
		}
	})

	t.Run("control: defaults create and bind the chart's own accounts", func(t *testing.T) {
		m, err := renderShippedChartE(t, nil)
		if err != nil {
			t.Fatal(err)
		}
		u := serviceAccountsOf(t, m)
		t.Logf("rendered %v; run as %v; bound %v", u.rendered, u.runAs, u.bound)
		if len(u.rendered) != 2 || len(u.bound) != 2 {
			t.Fatalf("defaults: want 2 ServiceAccounts and 2 bindings, got %v / %v", u.rendered, u.bound)
		}
		for _, b := range u.bound {
			if strings.HasSuffix(b, "/default") {
				t.Errorf("the default render binds the namespace's default account: %s", b)
			}
		}
	})
}

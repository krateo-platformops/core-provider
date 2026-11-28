package ownership

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	"sigs.k8s.io/yaml"
)

// The chart core-provider ships, in this monorepo. It renders every controller's env ConfigMap template.
var chartDir = filepath.Join("..", "..", "..", "..", "helm", "core-provider")

// renderControllerEnvs renders helm/core-provider with extra values and returns the env each controller
// process gets: the dedicated controller's ConfigMap template and the pool's, as key -> value. The
// templates still hold core-provider's {{ .x }} placeholders, so they are read line by line, not as YAML.
func renderControllerEnvs(t *testing.T, extra map[string]any) map[string]map[string]string {
	t.Helper()
	var files []*loader.BufferedFile
	if err := filepath.WalkDir(chartDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(chartDir, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if rel = filepath.ToSlash(rel); rel == "Chart.yaml" {
			data = []byte(strings.NewReplacer("CHART_VERSION", "0.0.0", "APP_VERSION", "0.0.0").Replace(string(data)))
		}
		files = append(files, &loader.BufferedFile{Name: rel, Data: data})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	chrt, err := loader.LoadFiles(files)
	if err != nil {
		t.Fatal(err)
	}
	rv, err := chartutil.ToRenderValues(chrt, extra, chartutil.ReleaseOptions{Name: "core-provider", Namespace: "krateo-system", IsInstall: true}, chartutil.DefaultCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := engine.Render(chrt, rv)
	if err != nil {
		t.Fatal(err)
	}
	body, ok := rendered["core-provider/templates/cdc-configmap.yaml"]
	if !ok {
		t.Fatal("the render has no templates/cdc-configmap.yaml")
	}
	var cm struct {
		Data map[string]string `json:"data"`
	}
	if err := yaml.Unmarshal([]byte(body), &cm); err != nil {
		t.Fatal(err)
	}
	line := regexp.MustCompile(`(?m)^\s*([A-Z][A-Z0-9_]*): (.*)$`)
	out := map[string]map[string]string{}
	for _, process := range []string{"configmap.yaml", "pool-configmap.yaml"} {
		tpl, ok := cm.Data[process]
		if !ok {
			t.Fatalf("templates/cdc-configmap.yaml carries no %s", process)
		}
		env := map[string]string{}
		for _, m := range line.FindAllStringSubmatch(tpl, -1) {
			v := strings.TrimSpace(m[2])
			if uq, err := strconv.Unquote(v); err == nil {
				v = uq
			}
			env[m[1]] = v
		}
		out[process] = env
	}
	return out
}

// #660: what the chart renders into a controller's env is what this binary reads, through the reader main
// uses (TimingsFromEnv), for a dedicated controller and for a pool. Expected values are literals: the
// chart's default (30s/20s/5s), and a cluster's own set. core-provider's
// TestOwnershipLeaseTimingsRenderOnceForBothBinaries holds core-provider's ConfigMap to the same value.
func TestChartRendersTheTimingsItReads(t *testing.T) {
	for _, c := range []struct {
		name  string
		extra map[string]any
		want  Timings
	}{
		{"default", nil, Timings{LeaseDuration: 30 * time.Second, RenewDeadline: 20 * time.Second, RetryPeriod: 5 * time.Second}},
		{"a cluster's own", map[string]any{"cdc": map[string]any{"ownershipLease": map[string]any{"duration": "41s", "renewDeadline": "27s", "retryPeriod": "7s"}}},
			Timings{LeaseDuration: 41 * time.Second, RenewDeadline: 27 * time.Second, RetryPeriod: 7 * time.Second}},
	} {
		t.Run(c.name, func(t *testing.T) {
			envs := renderControllerEnvs(t, c.extra)
			for process, env := range envs {
				// Control: the reader sees the rendered env at all (a key every controller env has).
				if env["COMPOSITION_CONTROLLER_HEALTH_PROBE_PORT"] != "8081" {
					t.Fatalf("%s: control: COMPOSITION_CONTROLLER_HEALTH_PROBE_PORT = %q, the env was not read", process, env["COMPOSITION_CONTROLLER_HEALTH_PROBE_PORT"])
				}
				// Rendered, not defaulted: with none of the three set, the reader would return the default too.
				for _, k := range []string{"COMPOSITION_CONTROLLER_OWNERSHIP_LEASE_DURATION", "COMPOSITION_CONTROLLER_OWNERSHIP_LEASE_RENEW_DEADLINE", "COMPOSITION_CONTROLLER_OWNERSHIP_LEASE_RETRY_PERIOD"} {
					if _, ok := env[k]; !ok {
						t.Errorf("%s: %s is not rendered", process, k)
					}
				}
				got, err := TimingsFromEnv(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
				if err != nil || got != c.want {
					t.Errorf("%s: the controller reads %+v, %v; want %+v", process, got, err, c.want)
				}
			}
			if len(envs) != 2 {
				t.Errorf("read %d process envs, want 2 (dedicated and pool)", len(envs))
			}
		})
	}
}

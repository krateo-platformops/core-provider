package deploy

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// shippedImages maps each image the chart runs to the Dockerfile that builds it, relative to the
// deploy package (the go/ directory holds one module per image).
var shippedImages = map[string]string{
	"krateo-platformops/core-provider":                  "../../../../core-provider/Dockerfile",
	"krateo-platformops/chart-inspector":                "../../../../chart-inspector/Dockerfile",
	"krateo-platformops/composition-dynamic-controller": "../../../../composition-dynamic-controller/Dockerfile",
}

// finalBase is the base image of a Dockerfile's last stage: the one the shipped image is made of.
func finalBase(t *testing.T, dockerfile string) string {
	t.Helper()
	b, err := os.ReadFile(dockerfile)
	if err != nil {
		t.Fatal(err)
	}
	base := ""
	for _, line := range strings.Split(string(b), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && strings.EqualFold(f[0], "FROM") {
			base = f[1]
		}
	}
	if base == "" {
		t.Fatalf("%s: no FROM", dockerfile)
	}
	return base
}

// execHandlers names every handler of c that runs a command inside the container.
func execHandlers(c corev1.Container) []string {
	var out []string
	if l := c.Lifecycle; l != nil {
		if l.PreStop != nil && l.PreStop.Exec != nil {
			out = append(out, "lifecycle.preStop.exec")
		}
		if l.PostStart != nil && l.PostStart.Exec != nil {
			out = append(out, "lifecycle.postStart.exec")
		}
	}
	for name, p := range map[string]*corev1.Probe{"livenessProbe": c.LivenessProbe, "readinessProbe": c.ReadinessProbe, "startupProbe": c.StartupProbe} {
		if p != nil && p.Exec != nil {
			out = append(out, name+".exec")
		}
	}
	sort.Strings(out)
	return out
}

// shippedContainers returns every container the chart runs with its defaults: core-provider's and
// chart-inspector's Deployments as helm renders them, and, after both render stages, the per-version
// controller (shipped_probes_test.go) and a pool's controller (renderPoolDeployment, the function
// EnsurePool applies).
func shippedContainers(t *testing.T) map[string]corev1.Container {
	t.Helper()
	out := map[string]corev1.Container{}
	manifests := renderShippedChart(t, nil)
	for path, manifest := range manifests {
		for _, doc := range strings.Split(manifest, "\n---") {
			var head struct{ Kind string }
			if err := yaml.Unmarshal([]byte(doc), &head); err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			if head.Kind != "Deployment" {
				continue
			}
			var dep appsv1.Deployment
			if err := yaml.Unmarshal([]byte(doc), &dep); err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			for _, c := range dep.Spec.Template.Spec.Containers {
				out[dep.Name+"/"+c.Name] = c
			}
		}
	}
	dep, _ := renderControllerObjects(t, nil)
	for _, c := range dep.Spec.Template.Spec.Containers {
		out["per-version controller/"+c.Name] = c
	}
	sizing, err := LoadPoolSizing(mountedTemplate(t, manifests, "cdc-deployment.yaml", "pool-sizing.json"), "general")
	if err != nil {
		t.Fatal(err)
	}
	pool, err := renderPoolDeployment(PoolOptions{Pool: "general", Namespace: "krateo-system", Sizing: sizing,
		DeploymentTemplatePath: mountedTemplate(t, manifests, "cdc-deployment.yaml", "pool-deployment.yaml")}, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range pool.Spec.Template.Spec.Containers {
		out["pool controller/"+c.Name] = c
	}
	return out
}

// #577: chart-inspector's preStop was `exec: /bin/sh -c sleep 10` on a distroless image, which has
// no shell. The kubelet reported "PreStopHook failed" and sent SIGTERM at once, so the hook #148
// added never slept, and every core-provider roll refused connections to chart-inspector, which
// flipped the compositions reconciling in that window to Ready=False/Unavailable (52 of 90 on 057).
// Nothing in a render shows that: the hook is well-formed YAML, and only the image decides whether
// its command exists. So this joins the two: no container whose image's final stage is distroless
// may carry an exec handler.
func TestShippedChart_NoExecHandlersOnDistrolessImages(t *testing.T) {
	containers := shippedContainers(t)
	distroless := 0
	for name, c := range containers {
		repo := c.Image
		if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
			repo = repo[:i]
		}
		var dockerfile string
		for r, df := range shippedImages {
			if strings.HasSuffix(repo, r) {
				dockerfile = df
			}
		}
		if dockerfile == "" {
			t.Fatalf("%s runs %q, whose Dockerfile this test does not know: add it to shippedImages", name, c.Image)
		}
		base := finalBase(t, filepath.Clean(dockerfile))
		t.Logf("%s: image %s, final base %s, exec handlers %v", name, repo, base, execHandlers(c))
		if !strings.Contains(base, "distroless") {
			continue
		}
		distroless++
		if h := execHandlers(c); len(h) > 0 {
			t.Errorf("%s runs on %s (no shell, no coreutils) but carries %v: the kubelet cannot run it", name, base, h)
		}
	}
	// Denominator: chart-inspector, the per-version controller and the pool controller are the
	// distroless ones today. A zero here would make the loop above vacuous.
	if len(containers) != 4 || distroless != 3 {
		t.Fatalf("control: want 4 shipped containers, 3 on distroless; got %d, %d", len(containers), distroless)
	}
}

// The fix half of #577: the preStop still delays SIGTERM by preStopSleepSeconds, now through the
// kubelet's own sleep action, and 0 still omits the hook.
func TestShippedChartInspector_PreStopSleepsNatively(t *testing.T) {
	find := func(overrides map[string]any) corev1.Container {
		t.Helper()
		for path, manifest := range renderShippedChart(t, overrides) {
			if !strings.HasSuffix(path, "chart-inspector/deployment.yaml") {
				continue
			}
			var dep appsv1.Deployment
			if err := yaml.Unmarshal([]byte(manifest), &dep); err != nil {
				t.Fatal(err)
			}
			return dep.Spec.Template.Spec.Containers[0]
		}
		t.Fatal("no chart-inspector Deployment rendered")
		return corev1.Container{}
	}
	c := find(nil)
	if c.Lifecycle == nil || c.Lifecycle.PreStop == nil || c.Lifecycle.PreStop.Sleep == nil || c.Lifecycle.PreStop.Sleep.Seconds != 10 {
		t.Fatalf("default preStop = %+v, want sleep 10s", c.Lifecycle)
	}
	if c := find(map[string]any{"chartInspector": map[string]any{"preStopSleepSeconds": 25}}); c.Lifecycle.PreStop.Sleep.Seconds != 25 {
		t.Errorf("preStopSleepSeconds=25 renders sleep %d", c.Lifecycle.PreStop.Sleep.Seconds)
	}
	if c := find(map[string]any{"chartInspector": map[string]any{"preStopSleepSeconds": 0}}); c.Lifecycle != nil {
		t.Errorf("preStopSleepSeconds=0 renders a lifecycle %+v, want none", c.Lifecycle)
	}
}

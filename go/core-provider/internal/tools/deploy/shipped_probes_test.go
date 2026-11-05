package deploy

import (
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/krateo-platformops/core-provider/internal/tools/objects"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/yaml"
)

// The per-version controller Deployment is rendered in TWO stages, and only the second produces the
// object the cluster runs (#333 could not tell which file that was; this is the chain):
//
//  1. helm renders helm/core-provider/assets/cdc/deployment.yaml into the ConfigMap
//     <fullname>-cdc-deployment (templates/cdc-deployment.yaml), which templates/deployment.yaml
//     mounts into core-provider at /tmp/assets/cdc-deployment.
//  2. core-provider reads that file (compositiondefinitions.CDCtemplateDeploymentPath) and renders it
//     per Kind+version with objects.CreateK8sObject (Deploy, below), then applies the result.
//
// So the probes are asserted on the decoded appsv1.Deployment after BOTH stages, not by reading the
// template: a probe that stage 1 drops (an unset value under `with`) or stage 2 mangles (an escaped
// "{{" in the wrong place) is caught here and would be invisible to a grep.
const shippedChart = "../../../../../helm/core-provider"

// renderShippedChart renders the shipped chart with helm's own engine, as `helm install` would, and
// returns the rendered manifests keyed by template path. overrides are user values.
func renderShippedChart(t *testing.T, overrides map[string]any) map[string]string {
	t.Helper()
	// The chart carries the release workflow's placeholders (CHART_VERSION / APP_VERSION), which are
	// not valid semver. Substitute them in a copy, exactly as the lint workflow does.
	dir := t.TempDir()
	err := filepath.WalkDir(shippedChart, func(p string, d fs.DirEntry, err error) error {
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
	})
	if err != nil {
		t.Fatalf("copying the shipped chart: %v", err)
	}
	chrt, err := loader.Load(dir)
	if err != nil {
		t.Fatalf("loading the shipped chart: %v", err)
	}
	caps := chartutil.DefaultCapabilities.Copy()
	// The chart's own floor (Chart.yaml kubeVersion).
	caps.KubeVersion = chartutil.KubeVersion{Version: "v1.36.0", Major: "1", Minor: "36"}
	// ToRenderValues also validates the values against values.schema.json.
	vals, err := chartutil.ToRenderValues(chrt, overrides,
		chartutil.ReleaseOptions{Name: "core-provider", Namespace: "krateo-system", IsInstall: true}, caps)
	if err != nil {
		t.Fatalf("values for the shipped chart: %v", err)
	}
	out, err := engine.Render(chrt, vals)
	if err != nil {
		t.Fatalf("rendering the shipped chart: %v", err)
	}
	return out
}

// mountedTemplate returns the file core-provider would read from the mounted ConfigMap rendered by
// template, i.e. the ConfigMap's single data key.
func mountedTemplate(t *testing.T, manifests map[string]string, template, key string) string {
	t.Helper()
	m, ok := manifests["core-provider/templates/"+template]
	if !ok {
		t.Fatalf("the chart rendered no %s", template)
	}
	var cm corev1.ConfigMap
	if err := yaml.Unmarshal([]byte(m), &cm); err != nil {
		t.Fatalf("decoding %s: %v", template, err)
	}
	body, ok := cm.Data[key]
	if !ok {
		t.Fatalf("%s carries no %q (keys: %v)", template, key, cm.Data)
	}
	p := filepath.Join(t.TempDir(), key)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// renderControllerObjects runs both stages and returns the per-version Deployment and ConfigMap
// core-provider would apply, rendered with the same values Deploy passes.
func renderControllerObjects(t *testing.T, overrides map[string]any) (appsv1.Deployment, corev1.ConfigMap) {
	t.Helper()
	manifests := renderShippedChart(t, overrides)
	gvr := schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v0-13-23", Resource: "kubernetesclusters"}
	nn := types.NamespacedName{Namespace: "krateo-system", Name: resourceNamer(gvr.Resource, gvr.Version)}

	var dep appsv1.Deployment
	if err := objects.CreateK8sObject(&dep, gvr, getCDCDeploymentNN(nn),
		mountedTemplate(t, manifests, "cdc-deployment.yaml", "deployment.yaml"),
		"serviceAccountName", "kubernetesclusters-v0-13-23", "api_ref_name", ""); err != nil {
		t.Fatalf("stage 2, Deployment: %v", err)
	}
	var cm corev1.ConfigMap
	if err := objects.CreateK8sObject(&cm, gvr, getCDCConfigmapNN(nn),
		mountedTemplate(t, manifests, "cdc-configmap.yaml", "configmap.yaml"),
		"composition_controller_sa_name", "kubernetesclusters-v0-13-23",
		"composition_controller_sa_namespace", "krateo-system",
		"status_data_template", "",
		"api_ref_name", "", "api_ref_namespace", "", "api_ref_extras", ""); err != nil {
		t.Fatalf("stage 2, ConfigMap: %v", err)
	}
	return dep, cm
}

func controllerContainer(t *testing.T, dep appsv1.Deployment) corev1.Container {
	t.Helper()
	if n := len(dep.Spec.Template.Spec.Containers); n != 1 {
		t.Fatalf("the per-version Deployment has %d containers, want 1", n)
	}
	return dep.Spec.Template.Spec.Containers[0]
}

func containerPort(c corev1.Container, name string) (int32, bool) {
	for _, p := range c.Ports {
		if p.Name == name {
			return p.ContainerPort, true
		}
	}
	return 0, false
}

// TestShippedControllerDeployment_CarriesTheProbes is #333's template half. With the chart's
// defaults, the per-version Deployment core-provider applies must probe READINESS on /readyz through
// the container port named "health", and carry NO liveness probe (#57: a kill mid-helm-operation
// leaves the release pending; decided again for #333 on 2026-11-05). The port must be the one the
// controller is told to serve on (COMPOSITION_CONTROLLER_HEALTH_PROBE_PORT in its envFrom ConfigMap);
// otherwise readiness fails against a port nothing listens on.
func TestShippedControllerDeployment_CarriesTheProbes(t *testing.T) {
	dep, cm := renderControllerObjects(t, nil)
	c := controllerContainer(t, dep)

	if c.ReadinessProbe == nil {
		t.Fatal("the per-version Deployment has no readinessProbe: a controller that cannot list its resource reports Ready (#333)")
	}
	if c.LivenessProbe != nil {
		t.Fatalf("the per-version Deployment carries a livenessProbe by default (%+v). #57: a kill in the "+
			"middle of a helm operation leaves the release pending, and a probe timeout under CPU "+
			"pressure kills a busy, healthy controller. Liveness is opt-in", c.LivenessProbe)
	}
	if c.ReadinessProbe.HTTPGet == nil {
		t.Fatalf("readiness probe is not httpGet: %+v", c.ReadinessProbe.ProbeHandler)
	}
	if c.ReadinessProbe.HTTPGet.Port != intstr.FromString("health") {
		t.Errorf("readiness probe targets port %s, want the named port \"health\"", c.ReadinessProbe.HTTPGet.Port.String())
	}
	if got := c.ReadinessProbe.HTTPGet.Path; got != "/readyz" {
		t.Errorf("readiness path = %q, want /readyz", got)
	}

	port, ok := containerPort(c, "health")
	if !ok {
		t.Fatalf("no container port named \"health\" for the probes to resolve (ports: %+v)", c.Ports)
	}
	if port != 8081 {
		t.Errorf("health container port = %d, want the chart default 8081", port)
	}
	if got := cm.Data["COMPOSITION_CONTROLLER_HEALTH_PROBE_PORT"]; got != "8081" {
		t.Errorf("the controller is told to serve probes on %q, but the probes reach port %d", got, port)
	}
}

// TestShippedControllerDeployment_ProbePortMovesAsOne: overriding cdc.healthProbe.port moves the
// container port AND the port the controller serves on, together, while the probes keep resolving
// by name. A probe that hard-coded the number would pass the default-values test and fail here.
func TestShippedControllerDeployment_ProbePortMovesAsOne(t *testing.T) {
	dep, cm := renderControllerObjects(t, map[string]any{
		"cdc": map[string]any{"healthProbe": map[string]any{"port": 9440}},
	})
	c := controllerContainer(t, dep)
	if port, _ := containerPort(c, "health"); port != 9440 {
		t.Errorf("health container port = %d, want 9440", port)
	}
	if got := cm.Data["COMPOSITION_CONTROLLER_HEALTH_PROBE_PORT"]; got != "9440" {
		t.Errorf("COMPOSITION_CONTROLLER_HEALTH_PROBE_PORT = %q, want 9440", got)
	}
	if c.ReadinessProbe == nil || c.ReadinessProbe.HTTPGet == nil || c.ReadinessProbe.HTTPGet.Port != intstr.FromString("health") {
		t.Errorf("readiness probe no longer resolves through the named port: %+v", c.ReadinessProbe)
	}
}

// TestShippedControllerDeployment_LivenessIsOptIn: the documented opt-in (values.yaml) renders exactly
// the probe supplied, on /healthz through the named port, and leaves readiness alone. The control for
// the default test above: without it, "no liveness" passes against a template that can never render
// one. The supplied probe must also be the more tolerant of the two, since it restarts the pod and
// readiness only reports.
func TestShippedControllerDeployment_LivenessIsOptIn(t *testing.T) {
	dep, _ := renderControllerObjects(t, map[string]any{
		"cdc": map[string]any{"livenessProbe": map[string]any{
			"httpGet":             map[string]any{"path": "/healthz", "port": "health"},
			"initialDelaySeconds": 15, "periodSeconds": 20, "timeoutSeconds": 5, "failureThreshold": 6,
		}},
	})
	c := controllerContainer(t, dep)
	if c.LivenessProbe == nil || c.LivenessProbe.HTTPGet == nil {
		t.Fatalf("an opted-in cdc.livenessProbe did not render: %+v", c.LivenessProbe)
	}
	if got := c.LivenessProbe.HTTPGet.Path; got != "/healthz" {
		t.Errorf("liveness path = %q, want /healthz: /readyz there would restart-loop a controller "+
			"through a briefly unserved version", got)
	}
	if c.LivenessProbe.HTTPGet.Port != intstr.FromString("health") {
		t.Errorf("liveness targets port %s, want the named port \"health\"", c.LivenessProbe.HTTPGet.Port.String())
	}
	if c.ReadinessProbe == nil || c.ReadinessProbe.HTTPGet.Path != "/readyz" {
		t.Errorf("opting in to liveness changed readiness: %+v", c.ReadinessProbe)
	}
	window := func(p *corev1.Probe) int32 { return p.InitialDelaySeconds + p.PeriodSeconds*p.FailureThreshold }
	if lw, rw := window(c.LivenessProbe), window(c.ReadinessProbe); lw <= rw {
		t.Errorf("the documented liveness example gives up after %ds, readiness after %ds: liveness must be the more tolerant", lw, rw)
	}
}

// TestShippedCoreProviderDeployment_ProbesTheHealthEndpoints covers core-provider's OWN Deployment,
// whose probes were interim TCP checks on the always-bound metrics port (8080), satisfied by a
// process that had never listed anything. They must now be httpGet /readyz and /healthz through the
// container port named "health", and that port must be the one the binary is told to serve on
// (CORE_PROVIDER_HEALTH_PROBE_PORT in its envFrom ConfigMap).
func TestShippedCoreProviderDeployment_ProbesTheHealthEndpoints(t *testing.T) {
	manifests := renderShippedChart(t, nil)
	var dep appsv1.Deployment
	if err := yaml.Unmarshal([]byte(manifests["core-provider/templates/deployment.yaml"]), &dep); err != nil {
		t.Fatalf("decoding core-provider's Deployment: %v", err)
	}
	var cm corev1.ConfigMap
	if err := yaml.Unmarshal([]byte(manifests["core-provider/templates/configmap.yaml"]), &cm); err != nil {
		t.Fatalf("decoding core-provider's ConfigMap: %v", err)
	}
	c := controllerContainer(t, dep)

	for name, tc := range map[string]struct {
		probe *corev1.Probe
		path  string
	}{
		"readiness": {c.ReadinessProbe, "/readyz"},
		"liveness":  {c.LivenessProbe, "/healthz"},
	} {
		if tc.probe == nil {
			t.Fatalf("core-provider has no %s probe", name)
		}
		if tc.probe.TCPSocket != nil {
			t.Errorf("%s probe is still a TCP check, which a blind process passes", name)
		}
		if tc.probe.HTTPGet == nil {
			t.Fatalf("%s probe is not httpGet: %+v", name, tc.probe.ProbeHandler)
		}
		if tc.probe.HTTPGet.Path != tc.path {
			t.Errorf("%s path = %q, want %q", name, tc.probe.HTTPGet.Path, tc.path)
		}
		if tc.probe.HTTPGet.Port != intstr.FromString("health") {
			t.Errorf("%s probe targets port %s, want the named port \"health\"", name, tc.probe.HTTPGet.Port.String())
		}
	}
	port, ok := containerPort(c, "health")
	if !ok {
		t.Fatalf("no container port named \"health\" (ports: %+v)", c.Ports)
	}
	if got := cm.Data["CORE_PROVIDER_HEALTH_PROBE_PORT"]; got != strconv.Itoa(int(port)) {
		t.Errorf("core-provider is told to serve probes on %q, but the probes reach port %d", got, port)
	}
}

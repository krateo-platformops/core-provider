package deploy

import (
	"testing"

	"github.com/krateo-platformops/core-provider/internal/tools/objects"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// #391. The env ConfigMap's checksum must change exactly when its data changes.
func TestConfigChecksum(t *testing.T) {
	base := map[string]string{"A": "1", "B": "2"}
	if configChecksum(base) != configChecksum(map[string]string{"B": "2", "A": "1"}) {
		t.Error("the checksum depends on map order")
	}
	for name, other := range map[string]map[string]string{
		"a value changed":          {"A": "1", "B": "3"},
		"a key renamed":            {"A": "1", "C": "2"},
		"a key added":              {"A": "1", "B": "2", "C": ""},
		"key/value boundary moved": {"A": "12", "B": ""},
	} {
		if configChecksum(base) == configChecksum(other) {
			t.Errorf("%s: same checksum as the original", name)
		}
	}
	// The case length-prefixing exists for: without it both concatenate to "ABC".
	if configChecksum(map[string]string{"AB": "C"}) == configChecksum(map[string]string{"A": "BC"}) {
		t.Error("a key/value boundary shift within one entry produced the same checksum")
	}
}

// The wiring Deploy relies on: the values it renders the Deployment with carry the checksum of the
// env ConfigMap data it was given.
func TestCDCDeploymentValuesCarryTheChecksum(t *testing.T) {
	data := map[string]string{"COMPOSITION_CONTROLLER_RESYNC_INTERVAL": "3m"}
	values, err := cdcDeploymentValues("sa", DeployOptions{}, data)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]any{}
	for i := 0; i+1 < len(values); i += 2 {
		got[values[i].(string)] = values[i+1]
	}
	if got["config_checksum"] != configChecksum(data) {
		t.Errorf("config_checksum = %v, want the env ConfigMap's checksum %s", got["config_checksum"], configChecksum(data))
	}
}

// Both render paths through the SHIPPED template: with a checksum the pod template carries it; without
// one (Lookup, Undeploy) it carries no annotation and does not render "<no value>".
func TestShippedControllerDeployment_CarriesTheConfigChecksum(t *testing.T) {
	manifests := renderShippedChart(t, nil)
	gvr := schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v0-13-23", Resource: "kubernetesclusters"}
	nn := types.NamespacedName{Namespace: "krateo-system", Name: resourceNamer(gvr.Resource, gvr.Version)}
	tmpl := mountedTemplate(t, manifests, "cdc-deployment.yaml", "deployment.yaml")

	var with appsv1.Deployment
	if err := objects.CreateK8sObject(&with, gvr, getCDCDeploymentNN(nn), tmpl,
		"serviceAccountName", "sa", "api_ref_name", "", "config_checksum", "abc123"); err != nil {
		t.Fatalf("stage 2 with a checksum: %v", err)
	}
	if got := with.Spec.Template.Annotations["krateo.io/config-checksum"]; got != "abc123" {
		t.Errorf("pod template annotation = %q, want abc123", got)
	}

	var without appsv1.Deployment
	if err := objects.CreateK8sObject(&without, gvr, getCDCDeploymentNN(nn), tmpl,
		"serviceAccountName", "sa", "api_ref_name", ""); err != nil {
		t.Fatalf("stage 2 without a checksum: %v", err)
	}
	if v, ok := without.Spec.Template.Annotations["krateo.io/config-checksum"]; ok {
		t.Errorf("without a checksum the annotation must be absent, got %q", v)
	}
}

// #393. A scale-to-zero definition renders the helper's replicas, including 0; every other one renders
// the chart value, exactly as before.
func TestShippedControllerDeployment_RendersReplicasOnlyWhenGiven(t *testing.T) {
	manifests := renderShippedChart(t, nil)
	gvr := schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v0-13-23", Resource: "kubernetesclusters"}
	nn := types.NamespacedName{Namespace: "krateo-system", Name: resourceNamer(gvr.Resource, gvr.Version)}
	tmpl := mountedTemplate(t, manifests, "cdc-deployment.yaml", "deployment.yaml")
	render := func(replicas *int32) int32 {
		t.Helper()
		values, err := cdcDeploymentValues("sa", DeployOptions{Replicas: replicas}, nil)
		if err != nil {
			t.Fatal(err)
		}
		var dep appsv1.Deployment
		if err := objects.CreateK8sObject(&dep, gvr, getCDCDeploymentNN(nn), tmpl, values...); err != nil {
			t.Fatalf("stage 2: %v", err)
		}
		if dep.Spec.Replicas == nil {
			t.Fatal("spec.replicas not rendered")
		}
		return *dep.Spec.Replicas
	}
	zero, two := int32(0), int32(2)
	if got := render(&zero); got != 0 {
		t.Errorf("Replicas=0 rendered %d; 0 must not fall back to the chart value", got)
	}
	if got := render(&two); got != 2 {
		t.Errorf("Replicas=2 rendered %d", got)
	}
	if got := render(nil); got != 1 {
		t.Errorf("no Replicas rendered %d, want the chart default 1", got)
	}
}

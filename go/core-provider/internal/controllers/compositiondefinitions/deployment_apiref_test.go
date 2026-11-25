package compositiondefinitions

import (
	"testing"

	"github.com/krateo-platformops/core-provider/internal/tools/objects"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

func renderCDCDeployment(t *testing.T, apiRefName string) appsv1.Deployment {
	t.Helper()
	dep := appsv1.Deployment{}
	err := objects.CreateK8sObject(&dep,
		schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1-0-0", Resource: "fireworksapps"},
		types.NamespacedName{Name: "fireworksapps-v1-0-0-controller", Namespace: "demo"},
		"testdata/manifests/deployment.yaml",
		deploymentTemplateValues("serviceAccountName", "fireworksapps-v1-0-0", "api_ref_name", apiRefName)...,
	)
	if err != nil {
		t.Fatalf("rendering deployment (apiRef=%q): %v", apiRefName, err)
	}
	return dep
}

// When an apiRef is declared, the CDC Deployment must project an authn-audience
// ServiceAccount token at the path the token client reads.
func TestDeploymentProjectsTokenWhenApiRef(t *testing.T) {
	dep := renderCDCDeployment(t, "status-sources")

	var vol *struct {
		name      string
		audience  string
		tokenPath string
	}
	for _, v := range dep.Spec.Template.Spec.Volumes {
		if v.Projected == nil {
			continue
		}
		for _, s := range v.Projected.Sources {
			if s.ServiceAccountToken != nil {
				vol = &struct {
					name      string
					audience  string
					tokenPath string
				}{v.Name, s.ServiceAccountToken.Audience, s.ServiceAccountToken.Path}
			}
		}
	}
	if vol == nil {
		t.Fatalf("no projected serviceAccountToken volume found; volumes=%+v", dep.Spec.Template.Spec.Volumes)
	}
	if vol.audience != "authn" {
		t.Errorf("token audience = %q, want authn", vol.audience)
	}
	if vol.tokenPath != "token" {
		t.Errorf("token path = %q, want token", vol.tokenPath)
	}

	// the token must be mounted at the directory the CDC reads (.../serviceaccount/token)
	var mounted bool
	for _, c := range dep.Spec.Template.Spec.Containers {
		for _, m := range c.VolumeMounts {
			if m.Name == vol.name {
				mounted = true
				if m.MountPath != "/var/run/secrets/krateo.io/serviceaccount" {
					t.Errorf("token mountPath = %q", m.MountPath)
				}
				if !m.ReadOnly {
					t.Errorf("token mount should be read-only")
				}
			}
		}
	}
	if !mounted {
		t.Errorf("projected token volume %q is not mounted into the container", vol.name)
	}
}

// Without an apiRef, no authn token is projected (the feature is opt-in per CompositionDefinition).
func TestDeploymentNoTokenWithoutApiRef(t *testing.T) {
	dep := renderCDCDeployment(t, "")
	for _, v := range dep.Spec.Template.Spec.Volumes {
		if v.Projected != nil {
			for _, s := range v.Projected.Sources {
				if s.ServiceAccountToken != nil {
					t.Errorf("projected serviceAccountToken volume present without apiRef: %q", v.Name)
				}
			}
		}
	}
}

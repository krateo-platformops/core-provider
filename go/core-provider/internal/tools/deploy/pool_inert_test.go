package deploy

import (
	"reflect"
	"testing"

	definitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/tools/objects"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// docs/design/cdc-pooling.md, step 1: spec.controller.pool is INERT. Whatever it says, the controller
// Deployment core-provider renders is the dedicated one it renders today, byte for byte. Any code that
// starts reading the field before the pool lifecycle (step 5) exists fails here.
func TestPoolFieldIsInert(t *testing.T) {
	manifests := renderShippedChart(t, nil)
	gvr := schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v0-13-23", Resource: "kubernetesclusters"}
	nn := types.NamespacedName{Namespace: "krateo-system", Name: resourceNamer(gvr.Resource, gvr.Version)}
	tmpl := mountedTemplate(t, manifests, "cdc-deployment.yaml", "deployment.yaml")
	envConfig := map[string]string{"COMPOSITION_CONTROLLER_RESYNC_INTERVAL": "3m"}

	render := func(c *definitionsv1alpha1.ControllerConfig) appsv1.Deployment {
		t.Helper()
		values, err := cdcDeploymentValues("sa", DeployOptions{
			ControllerNamespace: "test-ns", Controller: c}, envConfig)
		if err != nil {
			t.Fatal(err)
		}
		var dep appsv1.Deployment
		if err := objects.CreateK8sObject(&dep, gvr, getCDCDeploymentNN(nn), tmpl, values...); err != nil {
			t.Fatalf("stage 2: %v", err)
		}
		return dep
	}
	pool := "frontend-pages"
	baseline := render(nil)
	for name, c := range map[string]*definitionsv1alpha1.ControllerConfig{
		"an empty controller block": {},
		"pool set":                  {Pool: &pool},
	} {
		if got := render(c); !reflect.DeepEqual(got, baseline) {
			t.Errorf("%s renders a different Deployment than no controller block; pool must be inert in step 1", name)
		}
	}
}

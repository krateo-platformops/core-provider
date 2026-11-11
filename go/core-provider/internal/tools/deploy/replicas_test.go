package deploy

import (
	"context"
	"testing"

	kubecli "github.com/krateo-platformops/core-provider/internal/tools/kube"
	"github.com/krateo-platformops/core-provider/internal/tools/objects"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynfake "k8s.io/client-go/dynamic/fake"
)

func depWithReplicas(r any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "c", "namespace": "ns"}, "spec": map[string]any{}}}
	if r != nil {
		u.Object["spec"].(map[string]any)["replicas"] = r
	}
	return u
}

func TestKeepLiveReplicas(t *testing.T) {
	t.Run("the live value replaces the rendered one", func(t *testing.T) {
		desired := depWithReplicas(int64(1))
		if err := keepLiveReplicas(depWithReplicas(int64(0)), desired); err != nil {
			t.Fatal(err)
		}
		if r, _, _ := unstructured.NestedInt64(desired.Object, "spec", "replicas"); r != 0 {
			t.Errorf("replicas = %d, want the live 0", r)
		}
	})
	t.Run("a live object without replicas keeps the rendered value, never omits it", func(t *testing.T) {
		desired := depWithReplicas(int64(1))
		if err := keepLiveReplicas(depWithReplicas(nil), desired); err != nil {
			t.Fatal(err)
		}
		if r, found, _ := unstructured.NestedInt64(desired.Object, "spec", "replicas"); !found || r != 1 {
			t.Errorf("replicas = %d (found %v), want the rendered 1: an omitted field defaults to 1 server-side", r, found)
		}
	})
	t.Run("a non-integer live value is an error, not a guess", func(t *testing.T) {
		if err := keepLiveReplicas(depWithReplicas("zero"), depWithReplicas(int64(1))); err == nil {
			t.Error("want an error")
		}
	})
}

func TestDeploymentApplyOptions(t *testing.T) {
	base := kubecli.ApplyOptions{DryRun: []string{"All"}}
	if got := deploymentApplyOptions(base, DeployOptions{}); got.FromCurrent != nil || len(got.DryRun) != 1 {
		t.Errorf("not opted in: FromCurrent set=%v DryRun=%v, want no hook and the base options", got.FromCurrent != nil, got.DryRun)
	}
	if got := deploymentApplyOptions(base, DeployOptions{KeepLiveReplicas: true}); got.FromCurrent == nil || len(got.DryRun) != 1 {
		t.Errorf("opted in: FromCurrent set=%v DryRun=%v, want the hook AND the base options (Observe's dry-run must keep replicas too)", got.FromCurrent != nil, got.DryRun)
	}
}

// The whole path, minus the apiserver: the SHIPPED Deployment template rendered as Deploy renders it
// (chart replicaCount 1), applied through the real kube.Apply / objectclient over a live controller
// Deployment the helper has scaled to 0. Opted in, it stays at 0 while the rest of the spec is
// applied; not opted in, the chart value is written, which is also how opting out wakes it.
func TestControllerDeploymentApplyKeepsAZeroedControllerOnlyWhenOptedIn(t *testing.T) {
	manifests := renderShippedChart(t, nil)
	gvr := schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v0-13-23", Resource: "kubernetesclusters"}
	nn := getCDCDeploymentNN(types.NamespacedName{Namespace: "krateo-system", Name: resourceNamer(gvr.Resource, gvr.Version)})
	tmpl := mountedTemplate(t, manifests, "cdc-deployment.yaml", "deployment.yaml")

	render := func() *appsv1.Deployment {
		values, err := cdcDeploymentValues("sa", DeployOptions{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		var dep appsv1.Deployment
		if err := objects.CreateK8sObject(&dep, gvr, nn, tmpl, values...); err != nil {
			t.Fatal(err)
		}
		dep.SetGroupVersionKind(appsv1.SchemeGroupVersion.WithKind("Deployment"))
		return &dep
	}
	if r := render().Spec.Replicas; r == nil || *r != 1 {
		t.Fatalf("premise: the shipped template must render the chart replicaCount 1, got %v", r)
	}

	for _, c := range []struct {
		keep bool
		want int32
	}{{true, 0}, {false, 1}} {
		ctx := context.Background()
		live := render()
		zero := int32(0)
		live.Spec.Replicas = &zero
		live.Annotations = map[string]string{"marker": "old"}
		u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(live)
		if err != nil {
			t.Fatal(err)
		}
		dyn := dynfake.NewSimpleDynamicClient(runtime.NewScheme(), &unstructured.Unstructured{Object: u})

		desired := render()
		desired.Annotations = map[string]string{"marker": "new"}
		if err := kubecli.Apply(ctx, dyn, deploymentGVR, desired, deploymentApplyOptions(kubecli.ApplyOptions{}, DeployOptions{KeepLiveReplicas: c.keep})); err != nil {
			t.Fatalf("keep=%v: %v", c.keep, err)
		}
		got, err := dyn.Resource(deploymentGVR).Namespace(nn.Namespace).Get(ctx, nn.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		r, _, _ := unstructured.NestedInt64(got.Object, "spec", "replicas")
		if int32(r) != c.want {
			t.Errorf("KeepLiveReplicas=%v: replicas = %d, want %d", c.keep, r, c.want)
		}
		if got.GetAnnotations()["marker"] != "new" {
			t.Errorf("KeepLiveReplicas=%v: the rest of the Deployment was not applied (marker=%q)", c.keep, got.GetAnnotations()["marker"])
		}
	}
}

func TestChartControllerReplicasReadsTheShippedTemplate(t *testing.T) {
	manifests := renderShippedChart(t, nil)
	tmpl := mountedTemplate(t, manifests, "cdc-deployment.yaml", "deployment.yaml")
	gvr := schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1-0-0", Resource: "fireworksapps"}
	if n, err := ChartControllerReplicas(tmpl, gvr); err != nil || n != 1 {
		t.Errorf("default chart: %d, %v; want 1", n, err)
	}
	// Control: the value must come from the chart, not a constant.
	manifests3 := renderShippedChart(t, map[string]any{"cdc": map[string]any{"replicaCount": 3}})
	tmpl3 := mountedTemplate(t, manifests3, "cdc-deployment.yaml", "deployment.yaml")
	if n, err := ChartControllerReplicas(tmpl3, gvr); err != nil || n != 3 {
		t.Errorf("cdc.replicaCount=3: %d, %v; want 3", n, err)
	}
}

//go:build envtest

// The keep-live-replicas apply against a REAL apiserver (#393): the fake-client test proves the
// hook's logic, this proves the round trip a production redeploy makes. A controller the
// scale-to-zero helper scaled to 0 through the SCALE subresource, then the definition's full
// re-apply of the shipped template, must stay at 0 when opted in, and be restored to the chart's
// replicas when not.
//
//	Run: KUBEBUILDER_ASSETS=$(setup-envtest use 1.36.0 -p path) \
//	go test -tags envtest ./internal/tools/deploy/ -run KeepLiveReplicasEnvtest -v
package deploy

import (
	"context"
	"fmt"
	"os"
	"testing"

	kubecli "github.com/krateo-platformops/core-provider/internal/tools/kube"
	"github.com/krateo-platformops/core-provider/internal/tools/objects"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestKeepLiveReplicasEnvtest(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Fatal("KUBEBUILDER_ASSETS is not set: this test exists to run against a real apiserver, and a skip would read as a pass")
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	kube, err := client.New(cfg, client.Options{Scheme: clientgoscheme.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	dyn := dynamic.NewForConfigOrDie(cfg)
	ctx := context.Background()
	if err := kube.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "krateo-system"}}); err != nil {
		t.Fatal(err)
	}

	manifests := renderShippedChart(t, nil)
	tmpl := mountedTemplate(t, manifests, "cdc-deployment.yaml", "deployment.yaml")

	for i, c := range []struct {
		keep bool
		want int32
	}{{true, 0}, {false, 1}} {
		gvr := schema.GroupVersionResource{Group: "composition.krateo.io", Version: fmt.Sprintf("v1-0-%d", i), Resource: "fireworksapps"}
		nn := getCDCDeploymentNN(types.NamespacedName{Namespace: "krateo-system", Name: resourceNamer(gvr.Resource, gvr.Version)})
		render := func(marker string) *appsv1.Deployment {
			values, err := cdcDeploymentValues("sa", DeployOptions{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			var dep appsv1.Deployment
			if err := objects.CreateK8sObject(&dep, gvr, nn, tmpl, values...); err != nil {
				t.Fatal(err)
			}
			dep.SetGroupVersionKind(appsv1.SchemeGroupVersion.WithKind("Deployment"))
			dep.Annotations = map[string]string{"marker": marker}
			return &dep
		}
		// First apply: the controller as Deploy creates it (chart replicas 1).
		if err := kubecli.Apply(ctx, dyn, deploymentGVR, render("v1"), deploymentApplyOptions(kubecli.ApplyOptions{}, DeployOptions{KeepLiveReplicas: c.keep})); err != nil {
			t.Fatal(err)
		}
		// The helper scales it to 0 through the scale subresource, exactly as scaletozero.Reconcile does.
		obj := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: nn.Namespace, Name: nn.Name}}
		if err := kube.SubResource("scale").Patch(ctx, obj, client.RawPatch(types.MergePatchType, []byte(`{"spec":{"replicas":0}}`)),
			client.WithSubResourceBody(&autoscalingv1.Scale{})); err != nil {
			t.Fatal(err)
		}
		// The definition's full re-apply (an Update: the template changed).
		if err := kubecli.Apply(ctx, dyn, deploymentGVR, render("v2"), deploymentApplyOptions(kubecli.ApplyOptions{}, DeployOptions{KeepLiveReplicas: c.keep})); err != nil {
			t.Fatal(err)
		}
		got := &appsv1.Deployment{}
		if err := kube.Get(ctx, nn, got); err != nil {
			t.Fatal(err)
		}
		if got.Spec.Replicas == nil || *got.Spec.Replicas != c.want {
			t.Errorf("KeepLiveReplicas=%v: replicas after the re-apply = %v, want %d", c.keep, got.Spec.Replicas, c.want)
		}
		if got.Annotations["marker"] != "v2" {
			t.Errorf("KeepLiveReplicas=%v: the re-apply did not land (marker=%q)", c.keep, got.Annotations["marker"])
		}
	}
}

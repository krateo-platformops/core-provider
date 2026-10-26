//go:build integration
// +build integration

package compositiondefinitions

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	rtv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/e2e-framework/klient/decoder"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/klient/wait/conditions"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
)

// Moving a live CompositionDefinition to another namespace: create the same definition (same
// chart, so the same generated Kind+version) in the new namespace, then delete the original.
// "Test Change Version" leaves fireworks-app-2 in test-system on 2.0.4; these two steps bracket
// "Test Delete", whose deletion of that definition is the hand-over.
const (
	rehomeNamespace = "rehome-system"
	rehomeCDName    = "fireworks-app-2"
	rehomeInstance  = "rehome-probe"
	rehomeVersion   = "v2-0-4"
	rehomeShared    = "fireworksapps-v2-0-4" // ClusterRole/ClusterRoleBinding/ServiceAccount
	rehomeCRD       = "fireworksapps.composition.krateo.io"
)

var rehomeGVR = schema.GroupVersionResource{Group: "composition.krateo.io", Version: rehomeVersion, Resource: "fireworksapps"}

func rehomeControllerIn(ctx context.Context, t *testing.T, r *resources.Resources, ns string) bool {
	t.Helper()
	var dep appsv1.Deployment
	err := r.Get(ctx, rehomeShared+"-controller", ns, &dep)
	if apierrors.IsNotFound(err) {
		return false
	}
	if err != nil {
		t.Fatalf("get controller deployment in %s: %v", ns, err)
	}
	return true
}

func rehomeBindingNamespace(ctx context.Context, t *testing.T, r *resources.Resources) string {
	t.Helper()
	var crb rbacv1.ClusterRoleBinding
	if err := r.Get(ctx, rehomeShared, "", &crb); err != nil {
		t.Fatalf("shared ClusterRoleBinding %s: %v", rehomeShared, err)
	}
	if len(crb.Subjects) != 1 {
		t.Fatalf("shared ClusterRoleBinding %s has %d subjects, want 1", rehomeShared, len(crb.Subjects))
	}
	return crb.Subjects[0].Namespace
}

func rehomeEventually(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for: %s", timeout, what)
		}
		time.Sleep(5 * time.Second)
	}
}

func assessRehomeFollower(ctx context.Context, t *testing.T, cfg *envconf.Config, r *resources.Resources) context.Context {
	if err := r.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: rehomeNamespace}}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}

	var moved v1alpha1.CompositionDefinition
	if err := decoder.DecodeFile(os.DirFS(filepath.Join(testdataPath)), "fireworksapp.yaml", &moved, decoder.MutateNamespace(rehomeNamespace)); err != nil {
		t.Fatal(err)
	}
	moved.Spec.Chart.Version = "2.0.4"
	if err := r.Create(ctx, &moved); err != nil {
		t.Fatal(err)
	}

	// A live instance of the Kind, labelled the way the composition-version policy would label it.
	dyn := dynamic.NewForConfigOrDie(cfg.Client().RESTConfig())
	inst := &unstructured.Unstructured{}
	inst.SetAPIVersion(rehomeGVR.GroupVersion().String())
	inst.SetKind("FireworksApp")
	inst.SetNamespace(namespace)
	inst.SetName(rehomeInstance)
	inst.SetLabels(map[string]string{"krateo.io/composition-version": rehomeVersion})
	_ = unstructured.SetNestedField(inst.Object, rehomeInstance, "spec", "git", "toRepo", "name")
	if _, err := dyn.Resource(rehomeGVR).Namespace(namespace).Create(ctx, inst, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create instance: %v", err)
	}

	// The new definition elects the original (older) as owner: it becomes Available as a follower
	// and deploys no controller of its own.
	rehomeEventually(t, 3*time.Minute, "the moved definition to settle as a follower of test-system/"+rehomeCDName, func() bool {
		var cd v1alpha1.CompositionDefinition
		if err := r.Get(ctx, rehomeCDName, rehomeNamespace, &cd); err != nil {
			return false
		}
		c := cd.GetCondition(rtv1.TypeReady)
		return c.Reason == rtv1.ReasonAvailable && strings.Contains(c.Message, "run by CompositionDefinition "+namespace+"/"+rehomeCDName)
	})
	if rehomeControllerIn(ctx, t, r, rehomeNamespace) {
		t.Fatalf("follower deployed its own controller in %s: two controllers on one Kind+version", rehomeNamespace)
	}
	if !rehomeControllerIn(ctx, t, r, namespace) {
		t.Fatalf("owner's controller in %s is gone", namespace)
	}

	// No ping-pong over the shared ClusterRoleBinding: across two poll intervals it keeps naming
	// the owner's ServiceAccount.
	for i := 0; i < 9; i++ {
		if got := rehomeBindingNamespace(ctx, t, r); got != namespace {
			t.Fatalf("shared ClusterRoleBinding subject moved to %q while %s still owns the controller", got, namespace)
		}
		time.Sleep(5 * time.Second)
	}
	return ctx
}

func assessRehomeHandover(ctx context.Context, t *testing.T, cfg *envconf.Config, r *resources.Resources) context.Context {
	dyn := dynamic.NewForConfigOrDie(cfg.Client().RESTConfig())

	// "Test Delete" removed the original: the controller now runs in the new namespace only.
	rehomeEventually(t, 3*time.Minute, "the controller to move to "+rehomeNamespace, func() bool {
		return rehomeControllerIn(ctx, t, r, rehomeNamespace) && !rehomeControllerIn(ctx, t, r, namespace)
	})
	rehomeEventually(t, time.Minute, "the shared ClusterRoleBinding to name the new controller", func() bool {
		return rehomeBindingNamespace(ctx, t, r) == rehomeNamespace
	})
	var cr rbacv1.ClusterRole
	if err := r.Get(ctx, rehomeShared, "", &cr); err != nil {
		t.Fatalf("shared ClusterRole after the hand-over: %v", err)
	}

	// Nothing the original's delete would have destroyed as the last definition is gone.
	var crd apiextensionsv1.CustomResourceDefinition
	if err := r.Get(ctx, rehomeCRD, "", &crd); err != nil {
		t.Fatalf("CRD after the hand-over: %v", err)
	}
	if _, err := dyn.Resource(rehomeGVR).Namespace(namespace).Get(ctx, rehomeInstance, metav1.GetOptions{}); err != nil {
		t.Fatalf("instance after the hand-over: %v", err)
	}
	var moved v1alpha1.CompositionDefinition
	if err := r.Get(ctx, rehomeCDName, rehomeNamespace, &moved); err != nil {
		t.Fatal(err)
	}
	if c := moved.GetCondition(rtv1.TypeReady); strings.Contains(c.Message, "run by CompositionDefinition") {
		t.Fatalf("moved definition still reports itself a follower after the owner went: %q", c.Message)
	}

	// Cleanup: the instance first (strip a finalizer the test controller image may have added),
	// then the now-last definition, which takes the CRD with it.
	_ = dyn.Resource(rehomeGVR).Namespace(namespace).Delete(ctx, rehomeInstance, metav1.DeleteOptions{})
	rehomeEventually(t, 2*time.Minute, "the probe instance to go", func() bool {
		_, err := dyn.Resource(rehomeGVR).Namespace(namespace).Get(ctx, rehomeInstance, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true
		}
		_, _ = dyn.Resource(rehomeGVR).Namespace(namespace).Patch(ctx, rehomeInstance, types.MergePatchType,
			[]byte(`{"metadata":{"finalizers":null}}`), metav1.PatchOptions{})
		return false
	})
	if err := r.Delete(ctx, &moved); err != nil {
		t.Fatal(err)
	}
	if err := wait.For(conditions.New(r).ResourceDeleted(&moved), wait.WithTimeout(5*time.Minute), wait.WithInterval(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	rehomeEventually(t, 2*time.Minute, "the last definition's delete to remove the CRD", func() bool {
		return apierrors.IsNotFound(r.Get(ctx, rehomeCRD, "", &apiextensionsv1.CustomResourceDefinition{}))
	})
	return ctx
}

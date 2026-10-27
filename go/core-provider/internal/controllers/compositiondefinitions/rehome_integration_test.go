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
	rehomeNamespace  = "rehome-system"
	rehome2Namespace = "rehome2-system"
	rehomeCDName     = "fireworks-app-2"
	rehomeInstance   = "rehome-probe"
	rehomeVersion    = "v2-0-4"
	rehomeShared     = "fireworksapps-v2-0-4" // ClusterRole/ClusterRoleBinding/ServiceAccount
	rehomeCRD        = "fireworksapps.composition.krateo.io"
	// After the hand-overs the surviving definition is moved to another chart version (#215).
	rehomeBumpChart   = "2.0.3"
	rehomeBumpVersion = "v2-0-3"
)

var (
	rehomeGVR     = schema.GroupVersionResource{Group: "composition.krateo.io", Version: rehomeVersion, Resource: "fireworksapps"}
	rehomeBumpGVR = schema.GroupVersionResource{Group: "composition.krateo.io", Version: rehomeBumpVersion, Resource: "fireworksapps"}
)

// rehomeProbeLabels reads the probe instance's definition-ref namespace and version label.
func rehomeProbeLabels(ctx context.Context, dyn dynamic.Interface) (refNamespace, version string, ok bool) {
	u, err := dyn.Resource(rehomeGVR).Namespace(namespace).Get(ctx, rehomeInstance, metav1.GetOptions{})
	if err != nil {
		u, err = dyn.Resource(rehomeBumpGVR).Namespace(namespace).Get(ctx, rehomeInstance, metav1.GetOptions{})
		if err != nil {
			return "", "", false
		}
	}
	l := u.GetLabels()
	return l["krateo.io/composition-definition-namespace"], l["krateo.io/composition-version"], true
}

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

	// A live instance of the Kind, labelled the way the composition-version policy and the
	// controller label it: its version, and the definition that owns it (the original, for now).
	dyn := dynamic.NewForConfigOrDie(cfg.Client().RESTConfig())
	inst := &unstructured.Unstructured{}
	inst.SetAPIVersion(rehomeGVR.GroupVersion().String())
	inst.SetKind("FireworksApp")
	inst.SetNamespace(namespace)
	inst.SetName(rehomeInstance)
	inst.SetLabels(map[string]string{
		"krateo.io/composition-version":              rehomeVersion,
		"krateo.io/composition-definition-name":      rehomeCDName,
		"krateo.io/composition-definition-namespace": namespace,
	})
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
	// The instance named the deleted original; the definition that took the Kind over adopts it
	// (#215). Without this the next version bump of that definition leaves it behind.
	rehomeEventually(t, 3*time.Minute, "the instance to be adopted by "+rehomeNamespace, func() bool {
		ns, _, ok := rehomeProbeLabels(ctx, dyn)
		return ok && ns == rehomeNamespace
	})

	// Back-to-back, the way a helm upgrade moves a definition: the next copy exists but core-provider
	// has not reconciled it even once when the current owner's delete runs, so its status is still
	// EMPTY. A real helm upgrade hits this when the old definition's delete is dequeued first, which a
	// test cannot order deterministically, so the next copy is created PAUSED: the reconciler then
	// writes only a Paused condition, never the status GVK, for as long as the test needs (the same
	// state a new definition whose first reconcile errors stays in). The successor must be recognised
	// from its spec; a status-only match takes the last-definition path and destroys the instance and
	// the CRD (kind rehearsal of installer 0.3.457, core-provider#210).
	if err := r.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: rehome2Namespace}}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
	var next v1alpha1.CompositionDefinition
	if err := decoder.DecodeFile(os.DirFS(filepath.Join(testdataPath)), "fireworksapp.yaml", &next, decoder.MutateNamespace(rehome2Namespace)); err != nil {
		t.Fatal(err)
	}
	next.Spec.Chart.Version = "2.0.4"
	next.SetAnnotations(map[string]string{"krateo.io/paused": "true"})
	if err := r.Create(ctx, &next); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(ctx, &moved); err != nil {
		t.Fatal(err)
	}
	if err := wait.For(conditions.New(r).ResourceDeleted(&moved), wait.WithTimeout(5*time.Minute), wait.WithInterval(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, rehomeCRD, "", &apiextensionsv1.CustomResourceDefinition{}); err != nil {
		t.Fatalf("CRD after a back-to-back hand-over: %v", err)
	}
	if _, err := dyn.Resource(rehomeGVR).Namespace(namespace).Get(ctx, rehomeInstance, metav1.GetOptions{}); err != nil {
		t.Fatalf("instance after a back-to-back hand-over: %v", err)
	}
	// Resume the next copy; it now owns the Kind+version and deploys the controller.
	if err := r.Get(ctx, rehomeCDName, rehome2Namespace, &next); err != nil {
		t.Fatal(err)
	}
	next.SetAnnotations(nil)
	if err := r.Update(ctx, &next); err != nil {
		t.Fatalf("unpausing the next copy: %v", err)
	}
	rehomeEventually(t, 3*time.Minute, "the controller to move to "+rehome2Namespace, func() bool {
		return rehomeControllerIn(ctx, t, r, rehome2Namespace) && !rehomeControllerIn(ctx, t, r, rehomeNamespace)
	})
	moved = next
	rehomeEventually(t, 3*time.Minute, "the instance to be adopted by "+rehome2Namespace, func() bool {
		ns, _, ok := rehomeProbeLabels(ctx, dyn)
		return ok && ns == rehome2Namespace
	})

	// #215: bump the surviving definition. Its instances must follow it onto the new version. On
	// the bug, the bump retired the old version's controller, the owner-scoped migration matched
	// nothing (the instance named a deleted definition), and the definition reported up to date
	// with the instance stranded on the old version label.
	if err := r.Get(ctx, rehomeCDName, rehome2Namespace, &moved); err != nil {
		t.Fatal(err)
	}
	moved.Spec.Chart.Version = rehomeBumpChart
	if err := r.Update(ctx, &moved); err != nil {
		t.Fatalf("bumping the surviving definition: %v", err)
	}
	// Judged on what the definition REPORTS, not on the predicate behind it: #215's definition
	// reported Available on the new version while the instance was stranded, because its gate and
	// its migration shared one blind selector. So wait only for the definition to call itself done,
	// then require that it is.
	rehomeEventually(t, 5*time.Minute, "the bumped definition to report Available on "+rehomeBumpVersion, func() bool {
		var cd v1alpha1.CompositionDefinition
		if err := r.Get(ctx, rehomeCDName, rehome2Namespace, &cd); err != nil {
			return false
		}
		return cd.Status.CurrentGVK().Version == rehomeBumpVersion && cd.GetCondition(rtv1.TypeReady).Reason == rtv1.ReasonAvailable
	})
	if ns, v, ok := rehomeProbeLabels(ctx, dyn); !ok || ns != rehome2Namespace || v != rehomeBumpVersion {
		t.Fatalf("definition reports Available on %s, but the instance is owned by %q on version %q", rehomeBumpVersion, ns, v)
	}
	left, err := dyn.Resource(rehomeBumpGVR).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{LabelSelector: "krateo.io/composition-version=" + rehomeVersion})
	if err != nil {
		t.Fatal(err)
	}
	if len(left.Items) != 0 {
		t.Fatalf("%d instance(s) still on %s after the bump", len(left.Items), rehomeVersion)
	}

	// Cleanup: the instance first (strip a finalizer the test controller image may have added),
	// then the now-last definition, which takes the CRD with it.
	_ = dyn.Resource(rehomeBumpGVR).Namespace(namespace).Delete(ctx, rehomeInstance, metav1.DeleteOptions{})
	rehomeEventually(t, 2*time.Minute, "the probe instance to go", func() bool {
		_, err := dyn.Resource(rehomeBumpGVR).Namespace(namespace).Get(ctx, rehomeInstance, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true
		}
		_, _ = dyn.Resource(rehomeBumpGVR).Namespace(namespace).Patch(ctx, rehomeInstance, types.MergePatchType,
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

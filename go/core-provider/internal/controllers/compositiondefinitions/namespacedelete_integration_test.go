//go:build integration
// +build integration

package compositiondefinitions

import (
	"context"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
)

// cdcFinalizer is unstructured-runtime's composition finalizer (v1.12.0, pkg/controller/worker.go:32),
// which only a running composition controller removes.
const cdcFinalizer = "composition.krateo.io/finalizer"

// assessNamespaceDeleteFinishes is #313's incident as a test: on krateo-057 (2026-11-05) deleting a
// namespace that held a definition, its compositions AND their controller wedged until the finalizers
// were cleared by hand, because the controller died with the namespace.
//
// It deletes rehome2Namespace, which by now holds the OWNING definition. That is what makes the test
// able to fail: on main the owner's controller lives in that same namespace and is deleted with it, so
// the composition below keeps its finalizer and the namespace never finishes. With #313 the controller
// lives in the management namespace, outlives the deletion, and finalizes the composition.
//
// THE TEST REMOVES NO FINALIZER. It also refuses to pass vacuously: before deleting anything it
// waits for the composition to carry the controller's finalizer, which only a RUNNING controller
// adds. If no controller runs in this cluster, that wait fails, rather than the namespace deleting
// instantly and the test passing on main and branch alike.
func assessNamespaceDeleteFinishes(ctx context.Context, t *testing.T, cfg *envconf.Config, r *resources.Resources) context.Context {
	const probe = "nsdelete-probe"
	dyn := dynamic.NewForConfigOrDie(cfg.Client().RESTConfig())

	inst := &unstructured.Unstructured{}
	inst.SetAPIVersion(rehomeGVR.GroupVersion().String())
	inst.SetKind("FireworksApp")
	inst.SetNamespace(rehome2Namespace)
	inst.SetName(probe)
	inst.SetLabels(map[string]string{
		"krateo.io/composition-version":              rehomeVersion,
		"krateo.io/composition-definition-name":      rehomeCDName,
		"krateo.io/composition-definition-namespace": rehome2Namespace,
	})
	_ = unstructured.SetNestedField(inst.Object, probe, "spec", "git", "toRepo", "name")
	if _, err := dyn.Resource(rehomeGVR).Namespace(rehome2Namespace).Create(ctx, inst, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create the probe composition in %s: %v", rehome2Namespace, err)
	}

	// The precondition that makes the result mean something: a running controller took the probe.
	rehomeEventually(t, 3*time.Minute, "the controller to put its finalizer on the probe", func() bool {
		u, err := dyn.Resource(rehomeGVR).Namespace(rehome2Namespace).Get(ctx, probe, metav1.GetOptions{})
		return err == nil && slices.Contains(u.GetFinalizers(), cdcFinalizer)
	})
	if !rehomeControllerIn(ctx, t, r, mgmtNamespace) || rehomeControllerIn(ctx, t, r, rehome2Namespace) {
		t.Fatalf("setup: the controller must be in %s and not in %s before the deletion", mgmtNamespace, rehome2Namespace)
	}

	if err := r.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: rehome2Namespace}}); err != nil {
		t.Fatalf("delete namespace %s: %v", rehome2Namespace, err)
	}
	// On 057 this never happened without a human. Bound it generously, but bound it.
	rehomeEventually(t, 6*time.Minute, "namespace "+rehome2Namespace+" to finish deleting with no finalizer removed by hand", func() bool {
		err := r.Get(ctx, rehome2Namespace, "", &corev1.Namespace{})
		if err != nil && !apierrors.IsNotFound(err) {
			t.Fatalf("get namespace %s: %v", rehome2Namespace, err)
		}
		return apierrors.IsNotFound(err)
	})

	// Deliberately NOT asserted: that the controller still exists afterwards. If this was the Kind's
	// last definition, its Delete correctly removes the controller AFTER the compositions are gone
	// (compositions first, then the controller: ErrCompositionStillExist). What #313 guarantees, and
	// what main cannot do, is that the namespace FINISHES.
	return ctx
}

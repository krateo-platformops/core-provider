//go:build integration
// +build integration

package compositiondefinitions

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/e2e-framework/klient/decoder"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
)

// cdcFinalizer is unstructured-runtime's composition finalizer (v1.12.0, pkg/controller/worker.go:32),
// which only a running composition controller removes.
const cdcFinalizer = "composition.krateo.io/finalizer"

// nsdeleteNamespace holds the incident: a definition, ITS composition, and (before #313) its
// controller, all in one namespace that is then deleted.
const nsdeleteNamespace = "nsdelete-system"

// assessNamespaceDeleteFinishes is #313's incident as a test. On krateo-057 (2026-11-05) deleting a
// namespace that held a definition, its compositions AND their controller wedged until the finalizers
// were cleared by hand, because the controller died with the namespace.
//
// SELF-CONTAINED, because the step before it ends by deleting the Kind's last definition and with it
// the CRD (#404's third run). It creates its own namespace, a definition there (the Kind's ONLY
// definition, so its owner), and a composition there.
//
// That is what makes the test able to fail: on main the owner's controller is deployed into that same
// namespace and is deleted with it, so the composition keeps its finalizer and the namespace never
// finishes. With #313 the controller lives in the management namespace, outlives the deletion, and
// finalizes the composition.
//
// THE TEST REMOVES NO FINALIZER. It also refuses to pass vacuously: before deleting anything it
// waits for the composition to carry the controller's finalizer, which only a RUNNING controller
// adds.
func assessNamespaceDeleteFinishes(ctx context.Context, t *testing.T, cfg *envconf.Config, r *resources.Resources) context.Context {
	const probe = "nsdelete-probe"
	dyn := dynamic.NewForConfigOrDie(cfg.Client().RESTConfig())

	// 1. The incident's namespace, with the Kind's only definition in it.
	if err := r.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsdeleteNamespace}}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
	var cd v1alpha1.CompositionDefinition
	if err := decoder.DecodeFile(os.DirFS(filepath.Join(testdataPath)), "fireworksapp.yaml", &cd, decoder.MutateNamespace(nsdeleteNamespace)); err != nil {
		t.Fatal(err)
	}
	cd.Spec.Chart.Version = "2.0.4" // rehomeGVR's version
	if err := r.Create(ctx, &cd); err != nil {
		t.Fatalf("create the definition in %s: %v", nsdeleteNamespace, err)
	}
	rehomeEventually(t, 4*time.Minute, "the definition's controller to run in "+mgmtNamespace, func() bool {
		return rehomeControllerIn(ctx, t, r, mgmtNamespace)
	})
	if rehomeControllerIn(ctx, t, r, nsdeleteNamespace) {
		t.Fatalf("setup: a controller is in %s; #313 places it in %s", nsdeleteNamespace, mgmtNamespace)
	}

	// 2. A composition in the same namespace. Create is retried: the CRD may still be propagating.
	inst := &unstructured.Unstructured{}
	inst.SetAPIVersion(rehomeGVR.GroupVersion().String())
	inst.SetKind("FireworksApp")
	inst.SetNamespace(nsdeleteNamespace)
	inst.SetName(probe)
	inst.SetLabels(map[string]string{
		"krateo.io/composition-version":              rehomeVersion,
		"krateo.io/composition-definition-name":      cd.Name,
		"krateo.io/composition-definition-namespace": nsdeleteNamespace,
	})
	_ = unstructured.SetNestedField(inst.Object, probe, "spec", "git", "toRepo", "name")
	rehomeEventually(t, 2*time.Minute, "the probe composition to be created in "+nsdeleteNamespace, func() bool {
		_, err := dyn.Resource(rehomeGVR).Namespace(nsdeleteNamespace).Create(ctx, inst.DeepCopy(), metav1.CreateOptions{})
		return err == nil || apierrors.IsAlreadyExists(err)
	})

	// 3. The precondition that makes the result mean something: a running controller took the probe.
	// On timeout, say WHY: a missing finalizer means no running controller took the probe, and the
	// next reader needs the pod's own account (pulled? crash-looping? running but not reconciling?)
	// rather than a guess. #404's fourth run timed out here with no explanation.
	deadline := time.Now().Add(3 * time.Minute)
	for {
		u, err := dyn.Resource(rehomeGVR).Namespace(nsdeleteNamespace).Get(ctx, probe, metav1.GetOptions{})
		if err == nil && slices.Contains(u.GetFinalizers(), cdcFinalizer) {
			break
		}
		if time.Now().After(deadline) {
			dumpControllerPods(ctx, t, cfg, mgmtNamespace)
			t.Fatalf("timed out after 3m waiting for the controller to put %s on %s/%s (dump above)", cdcFinalizer, nsdeleteNamespace, probe)
		}
		time.Sleep(5 * time.Second)
	}

	// 4. The incident: delete the namespace holding the definition and its composition.
	if err := r.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsdeleteNamespace}}); err != nil {
		t.Fatalf("delete namespace %s: %v", nsdeleteNamespace, err)
	}
	// On 057 this never happened without a human. Bound it generously, but bound it.
	rehomeEventually(t, 6*time.Minute, "namespace "+nsdeleteNamespace+" to finish deleting with no finalizer removed by hand", func() bool {
		err := r.Get(ctx, nsdeleteNamespace, "", &corev1.Namespace{})
		if err != nil && !apierrors.IsNotFound(err) {
			t.Fatalf("get namespace %s: %v", nsdeleteNamespace, err)
		}
		return apierrors.IsNotFound(err)
	})

	// Deliberately NOT asserted: that the controller still exists afterwards. This was the Kind's only
	// definition, so its Delete correctly removes the controller AFTER the composition is gone
	// (compositions first, then the controller: ErrCompositionStillExist). What #313 guarantees, and
	// what main cannot do, is that the namespace FINISHES.
	return ctx
}

// dumpControllerPods logs every pod of the rehome Kind's controller in ns: its phase, each
// container's state and restarts, and the tail of its log. Diagnostics only.
func dumpControllerPods(ctx context.Context, t *testing.T, cfg *envconf.Config, ns string) {
	t.Helper()
	cs, err := kubernetes.NewForConfig(cfg.Client().RESTConfig())
	if err != nil {
		t.Logf("dump: clientset: %v", err)
		return
	}
	var dep appsv1.Deployment
	if err := cfg.Client().Resources().Get(ctx, rehomeShared+"-controller", ns, &dep); err != nil {
		t.Logf("dump: controller deployment %s/%s-controller: %v", ns, rehomeShared, err)
		return
	}
	t.Logf("dump: deployment image %s, ready %d/%d", dep.Spec.Template.Spec.Containers[0].Image, dep.Status.ReadyReplicas, dep.Status.Replicas)
	sel := labels.SelectorFromSet(dep.Spec.Selector.MatchLabels).String()
	pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		t.Logf("dump: list pods: %v", err)
		return
	}
	t.Logf("dump: %d controller pod(s) in %s", len(pods.Items), ns)
	for _, p := range pods.Items {
		t.Logf("dump: pod %s phase=%s", p.Name, p.Status.Phase)
		for _, c := range p.Status.ContainerStatuses {
			t.Logf("dump:   container %s ready=%v restarts=%d waiting=%+v terminated=%+v lastTerminated=%+v",
				c.Name, c.Ready, c.RestartCount, c.State.Waiting, c.State.Terminated, c.LastTerminationState.Terminated)
		}
		tail := int64(40)
		raw, err := cs.CoreV1().Pods(ns).GetLogs(p.Name, &corev1.PodLogOptions{TailLines: &tail}).DoRaw(ctx)
		if err != nil {
			t.Logf("dump:   logs: %v", err)
			continue
		}
		t.Logf("dump:   last log lines:\n%s", raw)
	}
}

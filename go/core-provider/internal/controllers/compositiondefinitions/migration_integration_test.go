//go:build integration
// +build integration

package compositiondefinitions

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	rtv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
)

// assessMigrationIsStopBeforeStart is #313's migration as a test. It reproduces the PRE-#313 state, a
// running current-version controller in the definition's namespace and none in the management
// namespace, and requires the next Update to move it STOP-BEFORE-START. The old placement's pods must
// all be gone before the new placement has any pod. The cdc has no leader election, so any overlap is
// two controllers reconciling the same compositions.
//
// It can fail: a relocation that deployed first and removed second would show pods in both
// namespaces in at least one sample. It refuses to pass vacuously: the sampler must have run (a
// non-zero sample count), and the old placement must have had a running pod before the Update.
func assessMigrationIsStopBeforeStart(ctx context.Context, t *testing.T, cfg *envconf.Config, r *resources.Resources) context.Context {
	name := rehomeShared + "-controller"

	// The real controller, as #313 placed it: its selector is what the planted copy must carry, so the
	// relocation's drain wait (which lists pods by the RENDERED selector) waits for the planted pods.
	var real appsv1.Deployment
	if err := r.Get(ctx, name, mgmtNamespace, &real); err != nil {
		t.Fatalf("setup: the controller in %s: %v", mgmtNamespace, err)
	}
	sel := real.Spec.Selector.MatchLabels
	if len(sel) == 0 {
		t.Fatalf("setup: %s/%s has no selector labels", mgmtNamespace, name)
	}

	// 0. PAUSE the owning definition first, and wait until the reconciler has acknowledged it. A
	// reconcile already in flight (the previous step restores spec.controller, which is an Update)
	// would otherwise relocate the planted controller before it ever runs: the feature working, and
	// the test racing it. That happened on #404's first CI run (21:20:06Z). Unpausing is then the
	// one deliberate trigger.
	var cd v1alpha1.CompositionDefinition
	if err := r.Get(ctx, rehomeCDName, namespace, &cd); err != nil {
		t.Fatal(err)
	}
	ann := cd.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	ann["krateo.io/paused"] = "true"
	cd.SetAnnotations(ann)
	if err := r.Update(ctx, &cd); err != nil {
		t.Fatalf("pause %s/%s: %v", namespace, rehomeCDName, err)
	}
	rehomeEventually(t, 2*time.Minute, "the reconciler to acknowledge the pause (Synced="+string(rtv1.ReasonReconcilePaused)+")", func() bool {
		var got v1alpha1.CompositionDefinition
		if err := r.Get(ctx, rehomeCDName, namespace, &got); err != nil {
			return false
		}
		return got.GetCondition(rtv1.TypeSynced).Reason == rtv1.ReasonReconcilePaused
	})

	// 1. Plant the pre-#313 placement: same name, same selector, a pod that actually runs.
	one := int32(1)
	legacy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: &one,
			Selector: &metav1.LabelSelector{MatchLabels: sel},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: sel},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name: "legacy", Image: "registry.k8s.io/pause:3.9",
				}}},
			},
		},
	}
	if err := r.Create(ctx, legacy); err != nil {
		t.Fatalf("plant the legacy controller in %s: %v", namespace, err)
	}
	podsIn := func(ns string) int {
		var pods corev1.PodList
		if err := r.List(ctx, &pods, resources.WithLabelSelector(labels.SelectorFromSet(sel).String())); err != nil {
			t.Fatalf("list pods: %v", err)
		}
		n := 0
		for _, p := range pods.Items {
			if p.Namespace == ns {
				n++
			}
		}
		return n
	}
	rehomeEventually(t, 3*time.Minute, "the planted legacy controller to have a running pod", func() bool {
		var pods corev1.PodList
		if err := r.List(ctx, &pods, resources.WithLabelSelector(labels.SelectorFromSet(sel).String())); err != nil {
			return false
		}
		for _, p := range pods.Items {
			if p.Namespace == namespace && p.Status.Phase == corev1.PodRunning {
				return true
			}
		}
		return false
	})

	// 2. Remove the management-namespace placement, and wait until its pods are gone too, so a
	// terminating old pod there can't be read as the NEW controller starting early.
	if err := r.Delete(ctx, &real); err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("delete %s/%s: %v", mgmtNamespace, name, err)
	}
	rehomeEventually(t, 3*time.Minute, "no controller pod in "+mgmtNamespace, func() bool { return podsIn(mgmtNamespace) == 0 })

	// 3. Sample both placements for the whole migration.
	var samples, overlaps atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(time.Second):
			}
			var pods corev1.PodList
			if err := r.List(ctx, &pods, resources.WithLabelSelector(labels.SelectorFromSet(sel).String())); err != nil {
				continue
			}
			oldN, newN := 0, 0
			for _, p := range pods.Items {
				switch p.Namespace {
				case namespace:
					oldN++
				case mgmtNamespace:
					newN++
				}
			}
			samples.Add(1)
			if oldN > 0 && newN > 0 {
				overlaps.Add(1)
			}
		}
	}()

	// 4. The one trigger: unpause, and one more worker, in a single Update.
	if err := r.Get(ctx, rehomeCDName, namespace, &cd); err != nil {
		t.Fatal(err)
	}
	before := cd.Spec.Controller.DeepCopy()
	ann = cd.GetAnnotations()
	delete(ann, "krateo.io/paused")
	cd.SetAnnotations(ann)
	if cd.Spec.Controller == nil {
		cd.Spec.Controller = &v1alpha1.ControllerConfig{}
	}
	w := int32(3)
	cd.Spec.Controller.Workers = &w
	if err := r.Update(ctx, &cd); err != nil {
		t.Fatalf("trigger the Update: %v", err)
	}

	rehomeEventually(t, 5*time.Minute, "the controller to be relocated: gone from "+namespace+", running in "+mgmtNamespace, func() bool {
		return !rehomeControllerIn(ctx, t, r, namespace) && rehomeControllerIn(ctx, t, r, mgmtNamespace) && podsIn(mgmtNamespace) > 0
	})
	close(stop)
	wg.Wait()

	// 5. The verdict, with its denominator.
	t.Logf("sampled both placements %d times during the migration; %d samples had pods in both", samples.Load(), overlaps.Load())
	if samples.Load() == 0 {
		t.Fatal("the sampler took no samples, so the stop-before-start assertion would be vacuous")
	}
	if overlaps.Load() > 0 {
		t.Fatalf("stop-before-start violated: %d of %d samples saw controller pods in %s AND %s at once",
			overlaps.Load(), samples.Load(), namespace, mgmtNamespace)
	}

	// Leave the definition as later steps expect it.
	if err := r.Get(ctx, rehomeCDName, namespace, &cd); err != nil {
		t.Fatal(err)
	}
	cd.Spec.Controller = before
	if err := r.Update(ctx, &cd); err != nil {
		t.Fatalf("restore spec.controller: %v", err)
	}
	return ctx
}

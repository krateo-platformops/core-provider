package deploy

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// #263. Undeploy deletes the controller Deployment and then its ServiceAccount and RBAC. The pod is
// still draining in between — unstructured-runtime keeps it alive for GracefulShutdownTimeout (30s)
// so an in-flight reconcile can finish — so revoking the identity without waiting leaves the
// controller alive and unable to write. That is how status.managed kept a name the release no
// longer rendered (#261).

func drainDep(gracePeriod *int64) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "cdc-v0-5-4", Namespace: "krateo-system"},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app.kubernetes.io/name": "cdc-v0-5-4"},
			},
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{TerminationGracePeriodSeconds: gracePeriod},
			},
		},
	}
}

func drainPod(name string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "krateo-system", Labels: labels}}
}

func TestWaitForControllerDrained_ReturnsWhenThePodIsGone(t *testing.T) {
	kube := fake.NewClientBuilder().WithScheme(scheme.Scheme).Build()

	start := time.Now()
	if err := waitForControllerDrained(context.Background(), kube, drainDep(nil), 2*time.Second); err != nil {
		t.Fatalf("no pods present, so the wait must return immediately: %v", err)
	}
	// The common case is a controller with nothing in flight. If this ever starts taking the full
	// grace period, every version migration pays it.
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("took %s with no pods present; a quiescent retirement must not pay the drain bound", d)
	}
}

// THE SUBJECT. A pod that is still terminating must BLOCK the identity deletion, and the wait must
// return an error rather than falling through — because falling through is what loses the write.
func TestWaitForControllerDrained_FailsWhileThePodIsStillTerminating(t *testing.T) {
	g := int64(1) // 1s + 15s margin; keeps the test honest without making it slow
	dep := drainDep(&g)
	kube := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(drainPod("cdc-v0-5-4-abc", dep.Spec.Selector.MatchLabels)).Build()

	err := waitForControllerDrained(context.Background(), kube, dep, 2*time.Second)
	if err == nil {
		t.Fatal("a pod that never terminates must produce an ERROR: returning nil here revokes the " +
			"ServiceAccount from under a controller that is still reconciling, which is #263")
	}
	if !strings.Contains(err.Error(), "cdc-v0-5-4-abc") {
		t.Errorf("error %q does not name the surviving pod; an operator needs to know which one", err)
	}
}

// A LIST failure must not be read as absence. This is the specific shape that destroys: "I could
// not see the pod" becoming "there is no pod" revokes the identity exactly when we are least
// entitled to.
func TestWaitForControllerDrained_ListFailureIsNotAbsence(t *testing.T) {
	dep := drainDep(nil)
	kube := &drainListFailClient{Client: fake.NewClientBuilder().WithScheme(scheme.Scheme).Build()}

	err := waitForControllerDrained(context.Background(), kube, dep, 2*time.Second)
	if err == nil {
		t.Fatal("a failed LIST must be an error, not a pass: treating an unreadable cluster as " +
			"'no pods' is the list-failure-as-zero mistake and it revokes the identity")
	}
	if !strings.Contains(err.Error(), "listing pods") {
		t.Errorf("error %q should say the LIST failed, not imply the pod was absent", err)
	}
}

// An empty selector must FAIL rather than match every pod in the namespace. client.MatchingLabels{}
// selects everything, so a missing selector would pass as soon as the namespace was empty of pods
// and block forever otherwise — an answer about the wrong question in both directions.
func TestWaitForControllerDrained_EmptySelectorIsRefused(t *testing.T) {
	kube := fake.NewClientBuilder().WithScheme(scheme.Scheme).Build()

	for _, tc := range []struct {
		name string
		dep  *appsv1.Deployment
	}{
		{"nil selector", &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "d", Namespace: "n"}}},
		{"empty matchLabels", &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "d", Namespace: "n"},
			Spec:       appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{}}},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := waitForControllerDrained(context.Background(), kube, tc.dep, 2*time.Second); err == nil {
				t.Error("an unidentifiable pod set must be refused, not treated as drained")
			}
		})
	}
}

func TestWaitForControllerDrained_NilClientIsRefused(t *testing.T) {
	if err := waitForControllerDrained(context.Background(), nil, drainDep(nil), 2*time.Second); err == nil {
		t.Error("with no client there is no evidence either way, which must not read as drained")
	}
}

// The bound comes from the pod's OWN terminationGracePeriodSeconds, not a constant: the chart sets
// cdc.terminationGracePeriodSeconds to exceed the runtime's drain window, and a wait shorter than
// that would time out on a controller that was about to finish legitimately.
func TestWaitForControllerDrained_BoundFollowsThePodsGracePeriod(t *testing.T) {
	g := int64(1)
	dep := drainDep(&g)
	kube := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(drainPod("p", dep.Spec.Selector.MatchLabels)).Build()

	// Deliberately passes 0 so the DERIVED bound is what is under test — this is the arm that
	// covers what production actually computes, and the only one that pays the real wait.
	start := time.Now()
	err := waitForControllerDrained(context.Background(), kube, dep, 0)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected a timeout with a pod that never goes away")
	}
	want := time.Duration(g)*time.Second + drainTimeoutMargin
	// Lower bound only: it must not give up EARLY, which would revoke the identity from a
	// controller still inside its legitimate drain window. The upper bound is not asserted, since
	// scheduling noise on a loaded CI runner is not a defect.
	if elapsed < want-time.Second {
		t.Errorf("gave up after %s but the pod's grace period allows %s; an early give-up revokes "+
			"the identity while the controller is still entitled to be running", elapsed, want)
	}
}

type drainListFailClient struct {
	client.Client
}

func (c *drainListFailClient) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return apierrors.NewServiceUnavailable("apiserver is unavailable")
}

var scheme = struct{ Scheme *runtime.Scheme }{Scheme: func() *runtime.Scheme {
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		panic(fmt.Sprintf("corev1: %v", err))
	}
	if err := appsv1.AddToScheme(s); err != nil {
		panic(fmt.Sprintf("appsv1: %v", err))
	}
	s.AddKnownTypeWithName(schema.GroupVersionKind{Version: "v1", Kind: "PodList"}, &corev1.PodList{})
	return s
}()}

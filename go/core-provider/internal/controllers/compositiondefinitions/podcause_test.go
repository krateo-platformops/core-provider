package compositiondefinitions

import (
	"strings"
	"testing"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/cdreason"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func pod(name string, mut func(*corev1.Pod)) corev1.Pod {
	p := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name}}
	p.Status.Phase = corev1.PodRunning
	p.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
		{Type: corev1.PodReady, Status: corev1.ConditionTrue},
	}
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "cdc", Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
	if mut != nil {
		mut(&p)
	}
	return p
}

func notReady(p *corev1.Pod) {
	p.Status.Conditions[1].Status = corev1.ConditionFalse
	p.Status.ContainerStatuses[0].Ready = false
}

func crashLooping(state corev1.ContainerState) func(*corev1.Pod) {
	return func(p *corev1.Pod) {
		notReady(p)
		cs := &p.Status.ContainerStatuses[0]
		cs.RestartCount = 7
		cs.State = state
		cs.LastTerminationState = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 1}}
	}
}

// #559 (Diego, 2026-11-17): a reader of the definition alone tells an eviction from a controller that
// came up and then failed. Each case names the cause only its arm writes.
func TestPodCause(t *testing.T) {
	for _, c := range []struct {
		name string
		pods []corev1.Pod
		want string
	}{
		{"no information", nil, ""},
		{"no pods", []corev1.Pod{}, "no pods"},
		{"every pod ready: nothing to add", []corev1.Pod{pod("a", nil)}, ""},
		{"evicted, replacement pending", []corev1.Pod{pod("x-2", func(p *corev1.Pod) {
			notReady(p)
			p.Status.Phase = corev1.PodPending
			p.Status.ContainerStatuses[0].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}
		})}, "pod x-2: starting"},
		{"unschedulable", []corev1.Pod{pod("x-2", func(p *corev1.Pod) {
			notReady(p)
			p.Status.Phase = corev1.PodPending
			p.Status.Conditions[0].Status = corev1.ConditionFalse
		})}, "pod x-2: unschedulable"},
		{"crash loop", []corev1.Pod{pod("x-1", crashLooping(corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}))}, "pod x-1: restarting (last exit: Error)"},
		{"OOMKilled", []corev1.Pod{pod("x-1", func(p *corev1.Pod) {
			crashLooping(corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}})(p)
			p.Status.ContainerStatuses[0].LastTerminationState.Terminated.Reason = "OOMKilled"
		})}, "pod x-1: restarting (last exit: OOMKilled)"},
		{"image pull", []corev1.Pod{pod("x-1", func(p *corev1.Pod) {
			notReady(p)
			p.Status.Phase = corev1.PodPending
			p.Status.ContainerStatuses[0].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}
		})}, "pod x-1: image pull failing"},
		{"config error", []corev1.Pod{pod("x-1", func(p *corev1.Pod) {
			notReady(p)
			p.Status.ContainerStatuses[0].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CreateContainerConfigError"}}
		})}, "pod x-1: CreateContainerConfigError"},
		{"running, readiness failing", []corev1.Pod{pod("x-1", notReady)}, "pod x-1: running, not ready"},
		{"terminating", []corev1.Pod{pod("x-1", func(p *corev1.Pod) { now := metav1.Now(); p.DeletionTimestamp = &now })}, "pod x-1: terminating"},
		{"names the first not-ready pod by name", []corev1.Pod{pod("x-b", notReady), pod("x-a", nil), pod("x-c", crashLooping(corev1.ContainerState{}))}, "pod x-b: running, not ready"},
	} {
		if got := podCause(c.pods); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// The message is stable across a crash loop's cycle: the container is momentarily waiting in
// CrashLoopBackOff, terminated with Error, or running, and a cause that followed its current state
// would rewrite the condition (and move lastTransitionTime) on every pass.
func TestPodCauseIsStableAcrossACrashLoopCycle(t *testing.T) {
	phases := []corev1.ContainerState{
		{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 1}},
		{Running: &corev1.ContainerStateRunning{}},
	}
	first := podCause([]corev1.Pod{pod("x-1", crashLooping(phases[0]))})
	for _, st := range phases[1:] {
		if got := podCause([]corev1.Pod{pod("x-1", crashLooping(st))}); got != first {
			t.Errorf("cause moved from %q to %q within one crash loop", first, got)
		}
	}
}

// Wiring: the lost-availability arm appends the cause, and only that arm reads pods.
func TestLostAvailabilityNamesThePodCause(t *testing.T) {
	cd := &compositiondefinitionsv1alpha1.CompositionDefinition{}
	lost := func() *appsv1.Deployment {
		d := rolledOut(1)
		d.Status.AvailableReplicas = 0
		setCond(d, appsv1.DeploymentAvailable, corev1.ConditionFalse, "MinimumReplicasUnavailable", "Deployment does not have minimum availability.")
		return d
	}
	pods := func() []corev1.Pod {
		return []corev1.Pod{pod("x-1", crashLooping(corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}))}
	}
	ready, _ := controllerReadiness(cd, "FireworksApp", lost(), nil, pods)
	if !cdreason.Unavailable.Is(ready) || !strings.HasSuffix(ready.Message, "; pod x-1: restarting (last exit: Error)") ||
		!strings.Contains(ready.Message, "lost availability after a completed rollout: MinimumReplicasUnavailable") {
		t.Fatalf("Ready = %s/%s %q", ready.Status, ready.Reason, ready.Message)
	}
	read := 0
	counting := func() []corev1.Pod { read++; return nil }
	for _, d := range []*appsv1.Deployment{rolledOut(1), func() *appsv1.Deployment { d := rolledOut(1); d.Generation = 9; return d }()} {
		controllerReadiness(cd, "FireworksApp", d, nil, counting)
	}
	if read != 0 {
		t.Errorf("pods read %d times outside the lost-availability arm", read)
	}
}

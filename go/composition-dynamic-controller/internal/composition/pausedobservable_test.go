package composition

import (
	"context"
	"github.com/krateo-platformops/plumbing/kubeutil/event"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/events"
	"strings"
	"testing"
	"time"

	compositionMeta "github.com/krateo-platformops/composition-dynamic-controller/pkg/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	unstructuredtools "github.com/krateo-platformops/unstructured-runtime/pkg/tools/unstructured"
)

// #258. Observe's paused branch returns before any setStatus, so Ready kept whatever it held —
// and a composition paused while Unavailable went on reporting the FAILURE. Two sessions concluded
// the annotation was inert and halted a live migration on exactly that, and a runbook gate of
// "proceed when it reports ReconcileGracefullyPaused" would have waited forever on the one
// composition that had to be paused.
//
// The sibling TestSetStatus_PausedStampsThePausedConditionAndSkipsProjection covers setStatus's
// routing when it is REACHED. This covers the branch that never reached it.
//
// The load-bearing arm is the FAILING one. A paused composition that was already Available would
// satisfy a naive assertion ("reports True") against the unfixed code, because True/Available is
// already True — so the healthy arm is the control and the overwrite is the subject.

func pausedObj(t *testing.T, name string, existing metav1.Condition) *unstructured.Unstructured {
	t.Helper()
	mg := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "composition.krateo.io/v1-0-0",
		"kind":       "Fireworks",
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": "krateo-system",
			"annotations": map[string]interface{}{
				compositionMeta.AnnotationKeyReconciliationGracefullyPaused:     "true",
				compositionMeta.AnnotationKeyReconciliationGracefullyPausedTime: "2026-10-29T23:08:23Z",
			},
		},
	}}
	if existing.LastTransitionTime.IsZero() {
		existing.LastTransitionTime = metav1.Now()
	}
	if err := unstructuredtools.SetConditions(mg, existing); err != nil {
		t.Fatalf("seeding condition: %v", err)
	}
	// PREMISE: both annotations must be read as paused by the runtime's own predicates, or the
	// branch under test is never entered. GetGracefullyPausedTime fails CLOSED on an unparseable
	// time, so a typo here would silently make this test assert nothing.
	if _, ok := compositionMeta.GetGracefullyPausedTime(mg); !ok {
		t.Fatal("premise: the paused-time annotation must parse as RFC3339")
	}
	if !compositionMeta.IsGracefullyPaused(mg) {
		t.Fatal("premise: IsGracefullyPaused must be true")
	}
	return mg
}

func readyOf(t *testing.T, mg *unstructured.Unstructured) metav1.Condition {
	t.Helper()
	// NOTE: unstructuredtools.GetCondition matches Type AND Reason, so GetCondition(type, "")
	// silently returns nil rather than "any reason". Scan by type instead.
	for _, c := range unstructuredtools.GetConditions(mg) {
		if c.Type == "Ready" {
			return c
		}
	}
	t.Fatal("no Ready condition on the object")
	return metav1.Condition{}
}

func TestPausedConditionIsObservable(t *testing.T) {
	for _, tc := range []struct {
		name     string
		existing metav1.Condition
		why      string
	}{
		{
			name: "paused while Unavailable — the case that halted a migration",
			existing: metav1.Condition{
				Type: "Ready", Status: metav1.ConditionFalse, Reason: "Unavailable",
				Message: "managed children not healthy: githubproviderkogs/krateo-system/github-provider-kog",
			},
			why: "the failure reason must be REPLACED, or a gate on ReconcileGracefullyPaused never fires",
		},
		{
			name: "paused while Available — the control",
			existing: metav1.Condition{
				Type: "Ready", Status: metav1.ConditionTrue, Reason: "Available",
				Message: "Composition is up-to-date",
			},
			why: "passes trivially if the assertion is merely 'status is True', so it must check the REASON",
		},
		{
			name: "paused while Creating",
			existing: metav1.Condition{
				Type: "Ready", Status: metav1.ConditionFalse, Reason: "Creating",
				Message: "Composition is being created",
			},
			why: "a pause during creation must still be reported as paused",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mg := pausedObj(t, "fireworks-sample", tc.existing)

			if err := setGracefullyPausedCondition(mg, false); err != nil {
				t.Fatalf("setGracefullyPausedCondition: %v", err)
			}

			got := readyOf(t, mg)
			if got.Reason != "ReconcileGracefullyPaused" {
				t.Errorf("Ready reason = %q, want ReconcileGracefullyPaused — %s", got.Reason, tc.why)
			}
			if got.Status != metav1.ConditionTrue {
				t.Errorf("Ready status = %v, want True (a pause is not a failure)", got.Status)
			}
			if got.Message == tc.existing.Message {
				t.Errorf("message still reads %q — the previous state was not replaced", got.Message)
			}

			// Exactly one Ready condition: Upsert keys on Type, so a second would mean the paused
			// condition was appended beside the old one rather than replacing it, and a consumer
			// reading the first match would see whichever came first.
			n := 0
			for _, c := range unstructuredtools.GetConditions(mg) {
				if c.Type == "Ready" {
					n++
				}
			}
			if n != 1 {
				t.Errorf("found %d Ready conditions, want exactly 1", n)
			}
		})
	}
}

// Idempotence: the paused Observe runs every resync (60s on the fleet), so a second call must not
// rewrite the condition. setReadyCondition's skip compares Type AND Reason AND Message AND Status;
// if any of those drifted between calls it would churn lastTransitionTime and the resourceVersion
// on every tick, for every paused composition, forever.
func TestPausedConditionDoesNotChurnOnRepeat(t *testing.T) {
	mg := pausedObj(t, "fireworks-sample", metav1.Condition{
		Type: "Ready", Status: metav1.ConditionFalse, Reason: "Unavailable", Message: "boom",
	})

	if err := setGracefullyPausedCondition(mg, false); err != nil {
		t.Fatalf("first call: %v", err)
	}
	first := readyOf(t, mg)

	// Force a distinguishable timestamp so a rewrite is detectable rather than assumed.
	first.LastTransitionTime = metav1.NewTime(first.LastTransitionTime.Add(-time.Hour))
	if err := unstructuredtools.SetConditions(mg, first); err != nil {
		t.Fatalf("backdating: %v", err)
	}
	backdated := readyOf(t, mg).LastTransitionTime

	if err := setGracefullyPausedCondition(mg, false); err != nil {
		t.Fatalf("second call: %v", err)
	}
	after := readyOf(t, mg).LastTransitionTime

	if !after.Equal(&backdated) {
		t.Errorf("lastTransitionTime moved on an unchanged repeat (%v -> %v): every paused "+
			"composition would churn its resourceVersion once per resync", backdated, after)
	}
}

// THE WIRING. The two tests above exercise setGracefullyPausedCondition directly, and a mutant
// that removes the call from Observe's paused branch SURVIVES them — which is precisely the gap
// that let #258 ship: the mechanism was covered and the call site was not. The same shape as
// TestProcessItem_DeleteOrphan driving the wrong event type, and as the stale-mapper retry that
// existed only in Install.
//
// So this drives the real h.Observe. The paused branch sits before the helm client is built, and
// dynamic client construction is lazy, so no apiserver is contacted.
func TestObserve_PausedBranchMakesThePauseObservable(t *testing.T) {
	t.Skip("#258 is OPEN and the naive fix is unsafe. Observe opens with mg = mg.DeepCopy(), so a " +
		"condition written in the paused branch is discarded. Making it persist requires a write " +
		"FROM Observe — and the runtime's handleObserve does fetch -> Observe -> SetConditions(el) " +
		"-> UpdateStatus(el), so any write from Observe leaves el's resourceVersion stale and the " +
		"runtime's own UpdateStatus 409s on every paused tick, flipping Synced to ReconcileError. " +
		"Un-skip this when #258 lands: it is the assertion that distinguishes a fix which works " +
		"from one that only looks like it does.")

	mg := pausedObj(t, "fireworks-sample", metav1.Condition{
		Type: "Ready", Status: metav1.ConditionFalse, Reason: "Unavailable",
		Message: "managed children not healthy",
	})

	rec := events.NewFakeRecorder(10)
	h := &handler{
		kubeconfig:    &rest.Config{Host: "https://api.example.invalid:6443"},
		eventRecorder: *event.NewAPIRecorder(rec),
	}

	obs, err := h.Observe(context.Background(), mg)
	if err != nil {
		t.Fatalf("Observe on a paused composition must not error: %v", err)
	}

	// The short-circuit itself: claiming up-to-date is what stops the runtime calling Update, and
	// therefore what stops the helm upgrade and the self-heal. If this regressed, the pause would
	// be reported and ineffective — worse than unreported and effective.
	if !obs.ResourceExists || !obs.ResourceUpToDate {
		t.Errorf("observation = %+v, want {exists:true upToDate:true}: a paused Observe must "+
			"report up-to-date or the runtime calls Update and the pause does nothing", obs)
	}

	if got := readyOf(t, mg).Reason; got != "ReconcileGracefullyPaused" {
		t.Errorf("Ready reason after a paused Observe = %q, want ReconcileGracefullyPaused. The "+
			"pause is in effect but invisible on the object, which is #258 — a runbook gate on "+
			"this reason waits forever.", got)
	}

	select {
	case ev := <-rec.Events:
		if !strings.Contains(ev, "ReconciliationGracefullyPaused") {
			t.Errorf("event = %q, want ReconciliationGracefullyPaused", ev)
		}
	default:
		t.Error("no event emitted: the event is the observable both sessions ended up gating on")
	}
}

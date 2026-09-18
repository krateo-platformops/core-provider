package composition

import (
	"testing"

	compositionCondition "github.com/krateo-platformops/composition-dynamic-controller/internal/condition"
	unstructuredtools "github.com/krateo-platformops/unstructured-runtime/pkg/tools/unstructured"
	"github.com/krateo-platformops/unstructured-runtime/pkg/tools/unstructured/condition"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// seedReady puts a Ready condition on the object verbatim, including a status that may contradict
// the reason — which is exactly the state krateo-core-provider#119 found stuck on the cluster.
func seedReady(status metav1.ConditionStatus, reason, message string) *unstructured.Unstructured {
	mg := &unstructured.Unstructured{Object: map[string]any{}}
	_ = unstructured.SetNestedSlice(mg.Object, []any{
		map[string]any{
			"type":               condition.TypeReady,
			"status":             string(status),
			"reason":             reason,
			"message":            message,
			"lastTransitionTime": "2026-09-17T15:06:15Z",
		},
	}, "status", "conditions")
	return mg
}

func readyCond(t *testing.T, mg *unstructured.Unstructured, reason string) *metav1.Condition {
	t.Helper()
	return unstructuredtools.GetCondition(mg, condition.TypeReady, reason)
}

// The #119 regression, verbatim from the issue: a Ready condition stored as
// {status: False, reason: Available, message: "Composition is up-to-date"} must be REPAIRED to
// status True, not treated as already-current.
//
// It was self-perpetuating because GetCondition matches Type+Reason only, so the lookup returned
// this condition and the old check compared only the Message — a match — and returned without
// writing. Ready froze while Synced kept advancing.
func TestSetAvailableRepairsContradictoryStatus(t *testing.T) {
	const msg = "Composition is up-to-date"
	mg := seedReady(metav1.ConditionFalse, condition.ReasonAvailable, msg)

	if err := setAvaibleStatus(mg, msg, false); err != nil {
		t.Fatalf("setAvaibleStatus: %v", err)
	}

	got := readyCond(t, mg, condition.ReasonAvailable)
	if got == nil {
		t.Fatal("Ready/Available condition disappeared")
	}
	if got.Status != metav1.ConditionTrue {
		t.Errorf("status = %q, want True — a stored status that contradicts the reason must be repaired, "+
			"not short-circuited as already-current (#119)", got.Status)
	}
	if got.Message != msg {
		t.Errorf("message = %q, want %q", got.Message, msg)
	}
}

// The issue names only setAvaibleStatus, but every Ready-condition setter shared the same
// Type+Reason+Message comparison. Each must repair a contradictory status.
func TestEveryReadySetterRepairsContradictoryStatus(t *testing.T) {
	cases := []struct {
		name       string
		reason     string
		message    string
		wantStatus metav1.ConditionStatus
		set        func(*unstructured.Unstructured, string) error
	}{
		{
			name: "Available", reason: condition.ReasonAvailable,
			message: "Composition is up-to-date", wantStatus: metav1.ConditionTrue,
			set: func(mg *unstructured.Unstructured, m string) error { return setAvaibleStatus(mg, m, false) },
		},
		{
			name: "Creating", reason: condition.ReasonCreating,
			message: "1 of 2 managed children are not ready", wantStatus: metav1.ConditionFalse,
			set: func(mg *unstructured.Unstructured, m string) error { return setCreatingStatus(mg, m, false) },
		},
		{
			name: "Unavailable", reason: condition.ReasonUnavailable,
			message: "managed children not healthy: Deployment/ns/x", wantStatus: metav1.ConditionFalse,
			set: func(mg *unstructured.Unstructured, m string) error { return setUnavailableStatus(mg, m, false) },
		},
		{
			name: "ReconcileGracefullyPaused", reason: compositionCondition.ReasonReconcileGracefullyPaused,
			message: "Composition is gracefully paused.", wantStatus: metav1.ConditionTrue,
			set: func(mg *unstructured.Unstructured, _ string) error { return setGracefullyPausedCondition(mg, false) },
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Seed with the WRONG status but the right reason+message — the state the old
			// comparison could not see past.
			wrong := metav1.ConditionTrue
			if c.wantStatus == metav1.ConditionTrue {
				wrong = metav1.ConditionFalse
			}
			mg := seedReady(wrong, c.reason, c.message)

			if err := c.set(mg, c.message); err != nil {
				t.Fatalf("setter: %v", err)
			}
			got := readyCond(t, mg, c.reason)
			if got == nil {
				t.Fatalf("Ready/%s condition disappeared", c.reason)
			}
			if got.Status != c.wantStatus {
				t.Errorf("status = %q, want %q — contradictory status must be repaired", got.Status, c.wantStatus)
			}
		})
	}
}

// A condition that already agrees must NOT be rewritten: rewriting every reconcile would churn
// lastTransitionTime and the resourceVersion. This is why Status was added to the comparison rather
// than the comparison being dropped.
func TestSetAvailableIsStillIdempotentWhenConsistent(t *testing.T) {
	const msg = "Composition is up-to-date"
	mg := seedReady(metav1.ConditionTrue, condition.ReasonAvailable, msg)

	if err := setAvaibleStatus(mg, msg, false); err != nil {
		t.Fatalf("setAvaibleStatus: %v", err)
	}

	got := readyCond(t, mg, condition.ReasonAvailable)
	if got == nil {
		t.Fatal("condition disappeared")
	}
	if got.LastTransitionTime.Format("2006-01-02T15:04:05Z") != "2026-09-17T15:06:15Z" {
		t.Errorf("lastTransitionTime was rewritten (%s) for an already-consistent condition; the "+
			"idempotence check should still short-circuit", got.LastTransitionTime)
	}
}

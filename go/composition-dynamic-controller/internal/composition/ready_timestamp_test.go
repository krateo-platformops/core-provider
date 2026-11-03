package composition

// #298 — the Ready condition must not be rewritten when readiness has not changed.
//
// The defect was not in setReadyCondition. Its guard is correct and was always correct:
//
//	cur.Reason == cond.Reason && cur.Message == message && cur.Status == cond.Status
//
// The defect was that the three phases FED IT THREE DIFFERENT MESSAGES for one Available state —
// "Composition is up-to-date" from Observe, "Composition created" from Create, "Composition values
// updated" from Update. A guard keyed on a value the caller varies is not a guard, so while the
// phases alternated the condition was rewritten on every reconcile and its LastTransitionTime
// claimed the composition had just become ready when it had been ready for hours.
//
// WHY A BEHAVIOURAL TEST RATHER THAN A SOURCE GUARD. An AST check over the three statusManagerOpts
// literals is the obvious instrument and it is the wrong one: #232 proved a source guard cannot tell
// a MENTION from a reachable one, and the guard written for exactly that purpose in
// stampready_exhaustive_test.go went blind today when a string literal became a call expression
// while its own calibration still passed. So this tests the mechanism — that an unchanged Available
// state produces no rewrite — and the differential arm proves the test can fail.
//
// WHY NOT DELETE THE message FIELD INSTEAD, which would make divergence structurally impossible:
// opts.message also feeds the Creating and Unavailable paths (support.go:339, :341), where a
// phase-specific message is legitimately informative. Removing it would conflate paths that should
// differ. Checked before concluding, rather than assumed.

import (
	"context"
	"testing"
	"time"

	unstructuredtools "github.com/krateo-platformops/unstructured-runtime/pkg/tools/unstructured"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func composition(t *testing.T) *unstructured.Unstructured {
	t.Helper()
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "composition.krateo.io/v1-0-0",
		"kind":       "Demo",
		"metadata":   map[string]any{"name": "demo", "namespace": "krateo-system"},
	}}
}

// readyStamp returns the Ready condition's LastTransitionTime, or the zero time if absent.
func readyStamp(t *testing.T, mg *unstructured.Unstructured) time.Time {
	t.Helper()
	for _, c := range unstructuredtools.GetConditions(mg) {
		if c.Type == "Ready" {
			return c.LastTransitionTime.Time
		}
	}
	return time.Time{}
}

// THE REGRESSION. Three stamps of the Available state, as the three phases now do, must move the
// timestamp exactly ONCE — on the first one, where readiness genuinely became true.
func TestAvailableConditionIsStampedOnceAcrossPhases(t *testing.T) {
	mg := composition(t)

	if err := setAvaibleStatus(mg, readyMessageAvailable, false); err != nil {
		t.Fatalf("first stamp: %v", err)
	}
	first := readyStamp(t, mg)
	if first.IsZero() {
		t.Fatal("no Ready condition after the first stamp — the rest of this test would " +
			"compare two zero times and pass over a condition that was never written")
	}

	// A real reconcile gap. Without this, two stamps inside the same second would compare equal
	// whether or not the guard fired, and the test would pass against the live defect.
	time.Sleep(1100 * time.Millisecond)

	for i, phase := range []string{"Create", "Update"} {
		if err := setAvaibleStatus(mg, readyMessageAvailable, false); err != nil {
			t.Fatalf("stamp from the %s phase: %v", phase, err)
		}
		if got := readyStamp(t, mg); !got.Equal(first) {
			t.Errorf("stamp %d (%s phase) moved LastTransitionTime %v -> %v; readiness did not "+
				"change, so the condition must not be rewritten (#298)", i+2, phase, first, got)
		}
	}
}

// THE DIFFERENTIAL, and it had to change its INSTRUMENT because another repo's fix removed the old
// one.
//
// It used to assert that a per-phase message MOVES LastTransitionTime. That worked while
// condition.Upsert replaced a condition wholesale — but unstructured-runtime #36 now PRESERVES
// LastTransitionTime whenever Status is unchanged, per the Kubernetes convention that the field
// tracks status transitions. Two writes at Status=True therefore leave the timestamp alone whatever
// their messages say, and this arm began failing with its own message claiming "the guard no longer
// compares messages" — which was a FALSE DIAGNOSIS baked into the assertion text. The guard is
// intact; the timestamp simply stopped being an observable proxy for "was the condition rewritten".
//
// So the differential now observes what the guard still controls: whether the condition's CONTENT
// was replaced. A per-phase message changes the stored Message; the unified constant does not. That
// is a property of setReadyCondition rather than of Upsert, so it cannot be taken away by a change
// in the runtime's timestamp policy.
//
// Worth keeping as a lesson rather than quietly rewriting: a test whose observable belongs to a
// DEPENDENCY can be invalidated by that dependency being improved, and it will fail with a message
// that blames the wrong thing.
func TestPerPhaseMessagesDoRewriteTheCondition(t *testing.T) {
	mg := composition(t)

	if err := setAvaibleStatus(mg, readyMessageAvailable, false); err != nil {
		t.Fatalf("Observe-phase stamp: %v", err)
	}
	firstMsg := readyMessage(t, mg)
	if firstMsg == "" {
		t.Fatal("no Ready condition after the first stamp — the assertions below would compare two " +
			"empty strings and pass over a condition that was never written")
	}

	// A SECOND stamp with the SAME message must leave the content untouched: this is the arm that
	// proves the guard fires at all.
	if err := setAvaibleStatus(mg, readyMessageAvailable, false); err != nil {
		t.Fatalf("second stamp with the same message: %v", err)
	}
	if got := readyMessage(t, mg); got != firstMsg {
		t.Errorf("an identical re-assertion changed the message %q -> %q", firstMsg, got)
	}

	// Now the message Create used to supply for the SAME Available state. The guard compares
	// messages, so this one DOES get through — which is precisely why three per-phase strings
	// rewrote the condition on every reconcile (#298).
	if err := setAvaibleStatus(mg, "Composition created", false); err != nil {
		t.Fatalf("Create-phase stamp: %v", err)
	}
	if got := readyMessage(t, mg); got == firstMsg {
		t.Fatal("a DIFFERENT message did NOT replace the stored message, so setReadyCondition has " +
			"stopped comparing messages. TestAvailableConditionIsStampedOnceAcrossPhases would then " +
			"pass even if the three phases diverged again, and #298 could return unnoticed")
	}
}

// readyMessage returns the Ready condition's message, or "" if absent.
func readyMessage(t *testing.T, mg *unstructured.Unstructured) string {
	t.Helper()
	for _, c := range unstructuredtools.GetConditions(mg) {
		if c.Type == "Ready" {
			return c.Message
		}
	}
	return ""
}

// THE STRUCTURAL ASSERTION, and it is the one that makes the mutant inert rather than uncaught.
//
// Reverting a phase's literal back to "Composition created" now breaks nothing — not because the
// tests above are weak, but because a call site's message can no longer REACH the Available
// condition. This test is the difference between those two readings: it hands deriveReadyFromHealth
// a deliberately wrong opts.message and asserts the condition carries the constant anyway.
//
// The seam exists because of the fail-safe: deriveReadyFromHealth degrades to a healthy verdict when
// h.kubeconfig is nil, so a zero handler exercises the real code path with no cluster. That
// fail-safe was written for #72, not for testability, and it is worth noticing that a defensive
// default produced the only unit-testable entry point into this function.
func TestAvailablePathIgnoresAPerPhaseMessage(t *testing.T) {
	h := &handler{} // kubeconfig nil -> the rollup degrades to the healthy default
	mg := composition(t)

	err := h.deriveReadyFromHealth(context.Background(), mg, &statusManagerOpts{
		message:       "Composition created", // the exact string Create used to send
		conditionType: ConditionTypeAvailable,
	})
	if err != nil {
		t.Fatalf("deriveReadyFromHealth: %v", err)
	}

	var msg, reason string
	found := false
	for _, c := range unstructuredtools.GetConditions(mg) {
		if c.Type == "Ready" {
			msg, reason, found = c.Message, c.Reason, true
		}
	}
	if !found {
		t.Fatal("no Ready condition was written — the assertion below would compare two empty " +
			"strings and pass over a function that did nothing")
	}
	// CONTROL that we are on the arm we think we are: a nil kubeconfig must give plain Available,
	// not a coverage or evidence reason. If this drifts, the message assertion is about a different
	// branch than the one the three phases hit.
	if reason != "Available" {
		t.Fatalf("expected reason Available on the fail-safe path, got %q — this test is no longer "+
			"exercising the branch the three phases reach", reason)
	}
	if msg != readyMessageAvailable {
		t.Errorf("Ready message = %q, want %q: a per-phase opts.message reached the Available "+
			"condition, so #298 can recur from a drifting call site", msg, readyMessageAvailable)
	}
}

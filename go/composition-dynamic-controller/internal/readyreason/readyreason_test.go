package readyreason

import (
	"testing"

	compositioncondition "github.com/krateo-platformops/composition-dynamic-controller/internal/condition"
	"github.com/krateo-platformops/unstructured-runtime/pkg/tools/unstructured/condition"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Every Reason builds a Ready condition with its own reason and status, a set transition time and
// no message; WithMessage changes only the message.
func TestConditionCarriesReasonAndStatus(t *testing.T) {
	for _, r := range Table() {
		c := r.Condition()
		if c.Type != condition.TypeReady || c.Reason != r.String() || c.Status != r.Status() || c.Message != "" || c.LastTransitionTime.IsZero() {
			t.Errorf("%s: got %+v", r, c)
		}
		if m := r.WithMessage("why"); m.Message != "why" || m.Reason != r.String() || m.Status != r.Status() {
			t.Errorf("%s WithMessage: got %+v", r, m)
		}
	}
}

// The graceful pause's readiness half stays Ready=TRUE (#241), now built here.
func TestReconcileGracefullyPausedIsReadyTrue(t *testing.T) {
	c := ReconcileGracefullyPaused.Condition()
	if c.Status != metav1.ConditionTrue || c.Reason != compositioncondition.ReasonReconcileGracefullyPaused {
		t.Fatalf("got %+v, want Ready=True/%s", c, compositioncondition.ReasonReconcileGracefullyPaused)
	}
}

// A reason that the cdc and the runtime both write carries ONE status, and it is the status the
// runtime's own constructor builds. A consumer reading the reason must never need the writer.
func TestOneStatusPerReasonAndItMatchesTheRuntime(t *testing.T) {
	status := map[string]metav1.ConditionStatus{}
	for _, r := range Table() {
		if s, ok := status[r.String()]; ok && s != r.Status() {
			t.Errorf("%s listed with status %s and %s", r, s, r.Status())
		}
		status[r.String()] = r.Status()
	}
	ctors := RuntimeConstructors()
	checked := 0
	for reason, c := range ctors {
		s, listed := status[reason]
		if !listed {
			continue // a runtime constructor this module does not write (none today)
		}
		checked++
		if c.Status != s {
			t.Errorf("%s: listed %s, the runtime's constructor builds %s", reason, s, c.Status)
		}
	}
	if checked != len(ctors) {
		t.Errorf("checked %d of %d runtime constructors against the table", checked, len(ctors))
	}
}

// Reasons are listed once per writer: a duplicate row would make the table lie about its size.
func TestNoDuplicateRows(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range Table() {
		k := string(r.Writer()) + "/" + r.String()
		if seen[k] {
			t.Errorf("duplicate row %s", k)
		}
		seen[k] = true
	}
}

package condition

import "testing"

func TestReasonReconcileGracefullyPausedConstant(t *testing.T) {
	expected := "ReconcileGracefullyPaused"
	if ReasonReconcileGracefullyPaused != expected {
		t.Errorf("Expected constant to be %s, got %s", expected, ReasonReconcileGracefullyPaused)
	}
}

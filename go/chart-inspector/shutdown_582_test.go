package main

import (
	"testing"
	"time"
)

// #582. The chart sets SHUTDOWN_TIMEOUT from the pod's termination budget; this pins the literal on
// this side of the contract (the chart's side is TestShippedChartInspector_TerminationBudget in
// core-provider's deploy package), and the fallback an older chart gets.
func TestShutdownTimeout(t *testing.T) {
	if shutdownTimeoutEnv != "SHUTDOWN_TIMEOUT" {
		t.Fatalf("shutdownTimeoutEnv = %q: the chart sets SHUTDOWN_TIMEOUT, so the deadline would silently fall back to 30s", shutdownTimeoutEnv)
	}
	t.Setenv(shutdownTimeoutEnv, "75s")
	if got := shutdownTimeout(); got != 75*time.Second {
		t.Errorf("SHUTDOWN_TIMEOUT=75s gives %s", got)
	}
	t.Run("unset: the 30s this process always used", func(t *testing.T) {
		t.Setenv(shutdownTimeoutEnv, "")
		// t.Setenv cannot unset; an empty value does not parse, which takes the same fallback.
		if got := shutdownTimeout(); got != 30*time.Second {
			t.Errorf("empty SHUTDOWN_TIMEOUT gives %s, want 30s", got)
		}
	})
}

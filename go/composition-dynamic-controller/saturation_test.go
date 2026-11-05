package main

import (
	"testing"

	"github.com/krateo-platformops/unstructured-runtime/pkg/logging"
	"github.com/krateo-platformops/unstructured-runtime/pkg/ratelimit"
)

// #66. The cdc's CPU-saturation opt-in.
func TestNewSaturationGovernor(t *testing.T) {
	// OFF (the default): no governor at all, so the controller is exactly as before. A governor
	// without saturation would gate nothing for the cdc, which names no rate-limit key.
	g, err := newSaturationGovernor(false, 0, 4, logging.NewNopLogger())
	if err != nil || g != nil {
		t.Fatalf("off must return no governor and no error, got %v, %v", g, err)
	}

	// ON: a governor the controller's Run will start, with local capacity starting at 1.
	g, err = newSaturationGovernor(true, 0, 4, logging.NewNopLogger())
	if err != nil || g == nil {
		t.Fatalf("on must return a governor, got %v, %v", g, err)
	}
	if _, ok := g.(ratelimit.Starter); !ok {
		t.Error("the governor does not implement ratelimit.Starter, so Run would never start its sampler")
	}
	lc, ok := g.(interface{ LocalCapacity() int })
	if !ok || lc.LocalCapacity() != 1 {
		t.Errorf("with saturation on, local capacity must start at 1 (the ramp), got ok=%v", ok)
	}
	// A keyless pass (the cdc's case) is gated: the first takes the single slot, the second waits.
	if _, d := g.TryAcquire(""); d != nil {
		t.Fatalf("the first keyless pass must get the local slot, got %+v", d)
	}
	if _, d := g.TryAcquire(""); d == nil || !d.Local {
		t.Errorf("a second keyless pass at capacity 1 must be a LOCAL deferral, got %+v", d)
	}

	// An out-of-range threshold fails at startup instead of capping silently.
	if _, err := newSaturationGovernor(true, 150, 4, logging.NewNopLogger()); err == nil {
		t.Error("a threshold of 150 must be an error")
	}
}

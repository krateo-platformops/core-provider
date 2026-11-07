package saturation

import (
	"context"
	"testing"
	"time"

	"github.com/krateo-platformops/unstructured-runtime/pkg/logging"
	"github.com/krateo-platformops/unstructured-runtime/pkg/ratelimit"
)

// #66. The cdc's CPU-saturation opt-in.
func TestNewGovernor(t *testing.T) {
	// OFF (the default): no governor at all, so the controller is exactly as before. A governor
	// without saturation would gate nothing for the cdc, which names no rate-limit key.
	g, err := NewGovernor(false, 0, 4, logging.NewNopLogger())
	if err != nil || g != nil {
		t.Fatalf("off must return no governor and no error, got %v, %v", g, err)
	}

	// ON: a governor the controller's Run will start, with local capacity starting at 1.
	g, err = NewGovernor(true, 0, 4, logging.NewNopLogger())
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
	// A worker takes the single slot before it pops; the next one waits. The controller finds the gate
	// by this interface, so a governor without it would leave the cdc unbounded with the rule on.
	gate, ok := g.(ratelimit.LocalGate)
	if !ok {
		t.Fatal("the governor does not implement ratelimit.LocalGate, so nothing would bound the workers")
	}
	if _, ok := gate.AcquireLocal(context.Background()); !ok {
		t.Fatal("the first worker must get the local slot")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, ok := gate.AcquireLocal(ctx); ok {
		t.Error("a second worker at capacity 1 must wait for the slot")
	}

	// An out-of-range threshold fails at startup instead of capping silently.
	if _, err := NewGovernor(true, 150, 4, logging.NewNopLogger()); err == nil {
		t.Error("a threshold of 150 must be an error")
	}
}

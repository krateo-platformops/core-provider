package main

import (
	"fmt"

	"github.com/krateo-platformops/unstructured-runtime/pkg/logging"
	"github.com/krateo-platformops/unstructured-runtime/pkg/ratelimit"
)

// newSaturationGovernor returns the governor that bounds this controller's reconciles by its own CPU
// saturation (unstructured-runtime v1.10.0, #66), or nil when the opt-in is off.
//
// OFF BY DEFAULT, and nil rather than a governor with saturation disabled: the cdc implements no
// RateLimitKeyer, so a governor without saturation would gate nothing, and passing none keeps the
// controller exactly as it was. With it on, every reconcile needs a slot in a process-wide local
// capacity that starts at 1, ramps up to `workers`, and halves while cpu.pressure "some avg10" is
// over the threshold. The controller's Run starts the sampler.
//
// threshold 0 means the runtime's default (50, provisional: the cdc measurement is what tests it).
// An out-of-range threshold is an error, so a misconfigured opt-in fails at startup instead of
// silently capping every reconcile at 1.
func newSaturationGovernor(enabled bool, threshold float64, workers int, log logging.Logger) (ratelimit.Governor, error) {
	if !enabled {
		return nil, nil
	}
	g, err := ratelimit.NewWithSaturation(ratelimit.Options{Ceiling: workers},
		ratelimit.SaturationOptions{Enabled: true, CPUPressureThreshold: threshold}, log)
	if err != nil {
		return nil, fmt.Errorf("configuring CPU-saturation gating: %w", err)
	}
	return g, nil
}

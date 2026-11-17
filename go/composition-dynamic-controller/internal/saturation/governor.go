// Package saturation wires the cdc's opt-in CPU-saturation gating (unstructured-runtime v1.12.0, #66).
//
// It lives here, not in package main, because the image builds `go build ./main.go` from main.go,
// internal/ and pkg/ only (Dockerfile). A second file in package main compiles with `go build ./...`
// and is ABSENT from the image build, which is how the first version of this broke the build.
package saturation

import (
	"fmt"

	"github.com/krateo-platformops/unstructured-runtime/pkg/logging"
	"github.com/krateo-platformops/unstructured-runtime/pkg/ratelimit"
)

// NewGovernor returns the governor that bounds this controller's reconciles by its own CPU
// saturation (unstructured-runtime v1.12.0, #66), or nil when the opt-in is off.
//
// OFF BY DEFAULT, and nil rather than a governor with saturation disabled: the cdc implements no
// RateLimitKeyer, so a governor without saturation would gate nothing, and passing none keeps the
// controller exactly as it was. With it on, every reconcile needs a slot in a process-wide local
// capacity that starts at 1 and ramps +1 per 2s up to `workers`, and that halves (minimum 1, at most
// once per window) when the CPU pressure of a whole 10s window, the share of it in which some task
// waited for CPU (cpu.pressure "some total"), is over the threshold (unstructured-runtime v1.16.0,
// #82; it was "some avg10" sampled every second). The controller's Run starts the sampler.
//
// threshold 0 means the runtime's default (50, provisional: the cdc measurement is what tests it).
// An out-of-range threshold is an error, so a misconfigured opt-in fails at startup instead of
// silently capping every reconcile at 1.
func NewGovernor(enabled bool, threshold float64, workers int, log logging.Logger) (ratelimit.Governor, error) {
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

// NewPoolGovernor returns the ONE governor a pool's members share (core-provider docs/design/
// cdc-pooling.md §5c, core-provider#561). Unlike NewGovernor it always returns one, because a pool
// needs it with saturation off too: poolWorkers is the process TOTAL of concurrent reconciles across
// every member, and the governor is what enforces it. With saturation on, the CPU pressure lowers
// that total, as it lowers a dedicated controller's.
//
// It is Shared (unstructured-runtime v1.25.0): a member's worker takes a slot only once it has an
// item, so an idle member never holds capacity another member needs, and the sampler runs once for
// the process however many members start it. Each member's own worker count stays its own
// (spec.controller.workers, else the controller default), and it should stay small: a worker
// holding an item while it waits for a slot is how long a later, higher-priority item of that member
// can wait.
//
// poolWorkers below 1 is an error: a pool that could run nothing would wedge every member, and
// guessing a total would hide a missing value that core-provider always renders.
func NewPoolGovernor(saturationEnabled bool, threshold float64, poolWorkers int, log logging.Logger) (ratelimit.Governor, error) {
	if poolWorkers < 1 {
		return nil, fmt.Errorf("a pool needs its total workers (COMPOSITION_CONTROLLER_POOL_WORKERS, from the chart's cdc.pool.*.workers), got %d", poolWorkers)
	}
	g, err := ratelimit.NewWithSaturation(ratelimit.Options{Ceiling: poolWorkers, Shared: true},
		ratelimit.SaturationOptions{Enabled: saturationEnabled, CPUPressureThreshold: threshold}, log)
	if err != nil {
		return nil, fmt.Errorf("configuring the pool's governor: %w", err)
	}
	return g, nil
}

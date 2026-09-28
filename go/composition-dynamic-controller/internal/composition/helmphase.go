package composition

import (
	"time"

	"github.com/krateo-platformops/unstructured-runtime/pkg/logging"
)

// helmPhase brackets a long-running helm operation with INFO breadcrumbs.
//
// A composition Update that performs a real helm operation — render, chart pull, apply of a large
// umbrella — can run for many minutes while the cdc logs NOTHING at INFO. Observed live: an
// installer controller went silent for ~27 minutes while it was actively working, the only output
// being the 30s metrics heartbeat (#69).
//
// From outside, that window is indistinguishable from a stalled controller: Synced=False carrying a
// stale message, no reconcile logs, no progress signal. It invites exactly the operator reactions
// the platform's self-healing design forbids — restart the controller, poke the CR — and those
// reactions are how a slow-but-converging reconcile becomes a broken one.
//
// Metrics already time these operations, but a metric answers "how long do upgrades take"; an
// operator staring at a silent controller needs "it is upgrading RIGHT NOW, and has been for 11
// minutes". Only one of those stops someone reaching for kubectl delete pod.
//
// Deliberately INFO on BOTH edges. A start line alone leaves the same ambiguity one step later —
// "it started 20 minutes ago" does not say whether it is still running — and a completion line
// alone is invisible for the entire window that matters. The pair, with elapsed, is what makes a
// long operation legible as progress rather than as a hang.
func helmPhase[T any](log logging.Logger, phase, release string, fn func() (T, error)) (T, error) {
	log.Info("Helm operation starting", "phase", phase, "release", release)

	started := time.Now()
	out, err := fn()
	elapsed := time.Since(started).Truncate(time.Millisecond)

	if err != nil {
		// INFO, not Error: the caller decides whether this is fatal — several of these call sites
		// tolerate the failure — and logging it as an error here would double-report the ones that
		// are and mis-report the ones that are not. The breadcrumb's job is to close the window it
		// opened, so an operator reading the log never sees a "starting" without its end.
		log.Info("Helm operation failed", "phase", phase, "release", release, "elapsed", elapsed.String(), "error", err.Error())
		return out, err
	}

	log.Info("Helm operation finished", "phase", phase, "release", release, "elapsed", elapsed.String())
	return out, err
}

// helmPhaseErr is helmPhase for operations that return only an error.
func helmPhaseErr(log logging.Logger, phase, release string, fn func() error) error {
	_, err := helmPhase(log, phase, release, func() (struct{}, error) {
		return struct{}{}, fn()
	})
	return err
}

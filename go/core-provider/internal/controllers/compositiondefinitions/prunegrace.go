package compositiondefinitions

import (
	"fmt"
	"sort"
	"strings"
	"time"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// servedVersionPruneGrace is how long a served version must stay prune-eligible before it is
// pruned (#454, Diego 2026-11-11: 5 minutes).
//
// WHY A WINDOW. A chart that renders against the CRDs (the installer umbrella) lists them, then
// looks each served version up. A prune landing between the two fails that lookup, and a helm
// lookup error aborts the WHOLE render (installer: compositionExistsAny), so one prune knocked a
// component's siblings and, through chart-inspector's same render, RBAC derivation offline.
// Measured on krateo-057: v0-1-42 pruned 0.95s before the failed lookup; on demo, three versions
// released at once by a helm history rotation failed three renders in three minutes.
//
// WHY 5 MINUTES. The window must outlast the slowest render. The Installer composition's
// reconcile pass on krateo-057 took p50 35.0s, p99 60.7s, max 74.5s over 1,468 passes in 24h
// (otel_traces, ServiceName composition-dynamic-controller, SpanName reconcile, 2026-11-12). 5m
// is 4x the worst pass. #103's concern, versions accumulating over days, is unaffected by minutes.
const servedVersionPruneGrace = 5 * time.Minute

// prunesDue applies the grace window to the versions prunableServedVersions found eligible now.
// It returns the versions due for pruning (eligible for at least window) and the record to store:
// every eligible version with the time it first became eligible, from pending when it was already
// there, else now. A version in pending that is no longer eligible is dropped, so a version that
// becomes eligible again starts a fresh clock. Each version has its own clock.
func prunesDue(prunable []string, pending []compositiondefinitionsv1alpha1.PendingPrune, now time.Time, window time.Duration) (due []string, next []compositiondefinitionsv1alpha1.PendingPrune) {
	since := make(map[string]metav1.Time, len(pending))
	for _, p := range pending {
		since[p.Version] = p.EligibleSince
	}
	for _, v := range prunable {
		t, ok := since[v]
		if !ok {
			t = metav1.NewTime(now)
		}
		next = append(next, compositiondefinitionsv1alpha1.PendingPrune{Version: v, EligibleSince: t})
		if now.Sub(t.Time) >= window { // a first sighting is never due: its clock is now
			due = append(due, v)
		}
	}
	sort.Strings(due)
	sort.Slice(next, func(i, j int) bool { return next[i].Version < next[j].Version })
	return due, next
}

// applyPruneGrace is the ONE decision both Observe and Update take on the versions found
// prune-eligible: it stores each version's clock in cr's status and returns those due for pruning.
// Observe reports not-up-to-date only when something is due, and Update prunes only what is due,
// so the two sides agree by construction (#103's "same predicate on both sides", kept for #454).
func applyPruneGrace(cr *compositiondefinitionsv1alpha1.CompositionDefinition, prunable []string, now time.Time) (due []string) {
	due, cr.Status.ServedVersionsPendingPrune = prunesDue(prunable, cr.Status.ServedVersionsPendingPrune, now, servedVersionPruneGrace)
	return due
}

// observePruneDecision is Observe's whole decision on the prune-eligible versions: it records the
// clocks (applyPruneGrace) and reports whether the definition is NOT up to date, with the message,
// only when a version is due. A version still inside its window leaves the definition up to date: an
// Observe that reported every eligible version would re-run Update on every poll for the whole
// window, and mark the definition update-pending to every readiness gate reading it during a roll.
func observePruneDecision(cr *compositiondefinitionsv1alpha1.CompositionDefinition, prunable []string, now time.Time) (pending bool, due []string, message string) {
	due = applyPruneGrace(cr, prunable, now)
	if len(due) == 0 {
		return false, nil, ""
	}
	return true, due, fmt.Sprintf("stale served versions pending prune: %v", due)
}

// heldVersions turns prunableServedVersions' keep reasons ("<version>:<reason>", or
// "ALL:<reason>" when every version is held) into the status record (#471), sorted by version so an
// unchanged hold writes an unchanged status.
func heldVersions(kept []string) []compositiondefinitionsv1alpha1.HeldVersion {
	if len(kept) == 0 {
		return nil
	}
	out := make([]compositiondefinitionsv1alpha1.HeldVersion, 0, len(kept))
	for _, k := range kept {
		v, reason, _ := strings.Cut(k, ":")
		if v == "ALL" {
			v = "*"
		}
		out = append(out, compositiondefinitionsv1alpha1.HeldVersion{Version: v, Reason: reason})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out
}

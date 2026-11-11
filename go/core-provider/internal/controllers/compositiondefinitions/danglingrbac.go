package compositiondefinitions

import (
	"context"
	"fmt"

	crdclient "github.com/krateo-platformops/core-provider/internal/tools/crd"
	"github.com/krateo-platformops/core-provider/internal/tools/deploy"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// danglingSweep is what one sweep for #414's dangling controller bindings saw and did.
type danglingSweep struct {
	// Found is every binding that names only a retired controller's absent ServiceAccount.
	Found []deploy.DanglingBinding
	// Kept is every candidate refused, with the reason (its ServiceAccount exists, helm owns it).
	Kept []string
	// Removed is what was deleted: empty in a dry run, which only reports Found.
	Removed []string
	// DryRun is true when nothing was deleted because removal is not enabled.
	DryRun bool
}

// sweepDanglingControllerRBAC finds, and when removal is enabled removes, the RoleBindings and
// ClusterRoleBindings whose every subject is a retired controller version's ServiceAccount, absent
// and off the Kind's CRD (#414, "retire by subject"; the rules are in deploy.FindDanglingControllerBindings).
//
// It runs after the leaked-controller reclaim, so a controller that reclaim just retired has its
// bindings repointed (ResubjectDerivedRBAC) BEFORE this looks: anything left naming the retired
// ServiceAccount is a binding the repoint deliberately does not touch.
//
// The versions come from the LIVE CRD. Without it there is no reliable notion of which versions are
// retired, so a missing or unreadable CRD is an error, never "every version is retired". The ONE
// exception is kindRemoved: the definition's Delete has just deleted the Kind's CRD itself, because no
// other definition keeps the Kind, so every version is retired by that act rather than by inference.
//
// Removal is off unless enabled (CORE_PROVIDER_REMOVE_DANGLING_CONTROLLER_RBAC): a dry run reports
// what it would remove, so the first run on a cluster can be checked against a census first.
func (e *external) sweepDanglingControllerRBAC(ctx context.Context, gvr schema.GroupVersionResource, placements []string, kindRemoved bool) (danglingSweep, error) {
	var known []string
	if !kindRemoved {
		crd, err := crdclient.Get(ctx, e.kube, gvr.GroupResource())
		if err != nil {
			return danglingSweep{}, fmt.Errorf("fetching CRD: %w", err)
		}
		if crd == nil {
			return danglingSweep{}, fmt.Errorf("CRD %s not found: cannot tell which controller versions are retired", gvr.GroupResource())
		}
		known = make([]string, 0, len(crd.Spec.Versions))
		for _, v := range crd.Spec.Versions {
			known = append(known, v.Name)
		}
	}
	found, kept, err := deploy.FindDanglingControllerBindings(ctx, deploy.DanglingOptions{
		DynamicClient: e.dynamic,
		Resource:      gvr.Resource,
		KnownVersions: known,
		Placements:    placements,
	})
	if err != nil {
		return danglingSweep{}, err
	}
	out := danglingSweep{Found: found, Kept: kept, DryRun: !e.removeDanglingRBAC}
	if len(found) == 0 || out.DryRun {
		return out, nil
	}
	out.Removed, err = deploy.RemoveDanglingControllerBindings(ctx, e.dynamic, found)
	return out, err
}

// Outcomes of one sweep, as logged in the "outcome" field.
const (
	danglingOutcomeError   = "error"   // the search failed: NOT evaluated
	danglingOutcomeDryRun  = "dry-run" // evaluated; found is what removal would delete (may be 0)
	danglingOutcomeRemoved = "removed" // evaluated with removal on, and something was found
	danglingOutcomeNone    = "none"    // evaluated with removal on, nothing found
)

// Phases a sweep runs in, as logged in the "phase" field.
const (
	danglingPhaseUpdate = "update" // a definition's Update, after the leaked-controller reclaim
	danglingPhaseDelete = "delete" // a definition's Delete, after its controller is undeployed
)

// danglingSweepLogLine is the ONE line every sweep logs, whatever it found, including nothing. The
// first version logged only when it found something, so a definition whose sweep found nothing was
// indistinguishable in the log from one whose sweep never ran, and a census diff could not tell
// "the rule excludes it" from "not evaluated yet". Every line carries phase, outcome, resource and
// found, and the logger adds the definition's name and namespace. bindings, kept and removed are
// PRESENT ON EVERY LINE and always arrays: removed was first added only on the removed outcome,
// so the other lines lacked it and a reader saw it as null (github-provider-kog, 2.13.81).
func danglingSweepLogLine(phase, resource string, s danglingSweep, err error) (string, []any) {
	kv := []any{"phase", phase, "resource", resource, "found", len(s.Found), "bindings", danglingNames(s.Found),
		"kept", nonNil(s.Kept), "removed", nonNil(s.Removed)}
	switch {
	case err != nil && len(s.Found) == 0:
		// A failed search returns nothing found; removal only runs when something was.
		return "could not check for dangling controller bindings; will retry (#414)",
			append(kv, "outcome", danglingOutcomeError, "error", err.Error())
	case s.DryRun:
		return "DRY RUN: would remove bindings that name only a retired controller's absent ServiceAccount (#414)",
			append(kv, "outcome", danglingOutcomeDryRun)
	case len(s.Found) == 0:
		return "no bindings name only a retired controller's absent ServiceAccount (#414)",
			append(kv, "outcome", danglingOutcomeNone)
	}
	kv = append(kv, "outcome", danglingOutcomeRemoved)
	if err != nil {
		// Some deletes failed; what was removed is still reported. Retried.
		return "removed some bindings that named only a retired controller's absent ServiceAccount; the rest will be retried (#414)",
			append(kv, "error", err.Error())
	}
	return "removed bindings that named only a retired controller's absent ServiceAccount (#414)", kv
}

// danglingNames renders bindings for a log line.
func danglingNames(found []deploy.DanglingBinding) []string {
	out := make([]string, 0, len(found))
	for _, f := range found {
		out = append(out, f.String())
	}
	return out
}

// nonNil returns s, or an empty slice for nil, so a JSON log field is [] rather than null.
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// sweepOnDelete is the #414 sweep as a definition's Delete runs it, after Undeploy. crdKept is
// Delete's skipCRD: false means this Delete removed the Kind's CRD (no other definition keeps it), so
// every version is retired by that act. It logs the one sweep line and returns the error that holds
// the deletion, if any (deleteSweepError).
func (e *external) sweepOnDelete(ctx context.Context, gvr schema.GroupVersionResource, placements []string, crdKept bool, log logging.Logger) error {
	sweep, serr := e.sweepDanglingControllerRBAC(ctx, gvr, placements, !crdKept)
	msg, kv := danglingSweepLogLine(danglingPhaseDelete, gvr.Resource, sweep, serr)
	log.Info(msg, kv...)
	return deleteSweepError(sweep, serr)
}

// deleteSweepError decides whether a sweep's failure holds the definition's Delete (Diego,
// 2026-11-10). After Delete nothing sweeps this definition again, so with removal on a failure
// returns an error: the finalizer stays, the CR shows it, and Delete retries. A dry run never holds a
// deletion, because it deletes nothing either way.
func deleteSweepError(s danglingSweep, err error) error {
	if err == nil || s.DryRun {
		return nil
	}
	return fmt.Errorf("removing bindings left by this definition's controllers (#414): %w", err)
}

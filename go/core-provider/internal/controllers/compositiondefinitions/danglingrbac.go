package compositiondefinitions

import (
	"context"
	"fmt"

	crdclient "github.com/krateo-platformops/core-provider/internal/tools/crd"
	"github.com/krateo-platformops/core-provider/internal/tools/deploy"
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
// retired, so a missing or unreadable CRD is an error, never "every version is retired".
//
// Removal is off unless enabled (CORE_PROVIDER_REMOVE_DANGLING_CONTROLLER_RBAC): a dry run reports
// what it would remove, so the first run on a cluster can be checked against a census first.
func (e *external) sweepDanglingControllerRBAC(ctx context.Context, gvr schema.GroupVersionResource, placements []string) (danglingSweep, error) {
	crd, err := crdclient.Get(ctx, e.kube, gvr.GroupResource())
	if err != nil {
		return danglingSweep{}, fmt.Errorf("fetching CRD: %w", err)
	}
	if crd == nil {
		return danglingSweep{}, fmt.Errorf("CRD %s not found: cannot tell which controller versions are retired", gvr.GroupResource())
	}
	known := make([]string, 0, len(crd.Spec.Versions))
	for _, v := range crd.Spec.Versions {
		known = append(known, v.Name)
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

// danglingSweepLogLine is the ONE line every sweep logs, whatever it found, including nothing. The
// first version logged only when it found something, so a definition whose sweep found nothing was
// indistinguishable in the log from one whose sweep never ran, and a census diff could not tell
// "the rule excludes it" from "not evaluated yet". Every line carries outcome, resource and found,
// and the logger adds the definition's name and namespace.
func danglingSweepLogLine(resource string, s danglingSweep, err error) (string, []any) {
	kv := []any{"resource", resource, "found", len(s.Found), "bindings", danglingNames(s.Found), "kept", s.Kept}
	switch {
	case err != nil && len(s.Found) == 0:
		// A failed search returns nothing found; removal only runs when something was.
		return "could not check for dangling controller bindings; will retry on the next update (#414)",
			append(kv, "outcome", danglingOutcomeError, "error", err.Error())
	case s.DryRun:
		return "DRY RUN: would remove bindings that name only a retired controller's absent ServiceAccount (#414)",
			append(kv, "outcome", danglingOutcomeDryRun)
	case len(s.Found) == 0:
		return "no bindings name only a retired controller's absent ServiceAccount (#414)",
			append(kv, "outcome", danglingOutcomeNone)
	}
	kv = append(kv, "outcome", danglingOutcomeRemoved, "removed", s.Removed)
	if err != nil {
		// Some deletes failed; what was removed is still reported. Retried on the next update.
		return "removed some bindings that named only a retired controller's absent ServiceAccount; the rest will be retried on the next update (#414)",
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

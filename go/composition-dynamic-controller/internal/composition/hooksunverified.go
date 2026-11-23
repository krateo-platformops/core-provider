package composition

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/krateo-platformops/composition-dynamic-controller/internal/auditrec"

	"github.com/krateo-platformops/composition-dynamic-controller/internal/conditionreason"
	compositionMeta "github.com/krateo-platformops/composition-dynamic-controller/pkg/meta"
	helmconfig "github.com/krateo-platformops/plumbing/helm"
	"github.com/krateo-platformops/plumbing/kubeutil/event"
	"github.com/krateo-platformops/unstructured-runtime/pkg/logging"
	"github.com/krateo-platformops/unstructured-runtime/pkg/tools"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// hooksUnverifiedSince is krateo.io/hooks-unverified's value, and whether it is set (#563).
func hooksUnverifiedSince(mg *unstructured.Unstructured) (string, bool) {
	v, ok := mg.GetAnnotations()[compositionMeta.AnnotationKeyHooksUnverified]
	return v, ok
}

// withHooksUnverified makes Ready say the hooks of an interrupted operation may not have run while
// krateo.io/hooks-unverified is set (Diego, 2026-11-17, #563). It outranks every rollup outcome, a
// healthy one included: the children can all be healthy while a post-install hook that rotates a
// password never ran, which is exactly the state the mark exists to keep visible. The rollup's own
// reason and message are kept in the text, so a failing child is not hidden behind it.
func withHooksUnverified(out readyOutcome, mg *unstructured.Unstructured) readyOutcome {
	since, ok := hooksUnverifiedSince(mg)
	if !ok {
		return out
	}
	return readyOutcome{
		reason: conditionreason.ReasonHooksMayNotHaveRun,
		message: fmt.Sprintf("an interrupted helm operation was recovered without a deployed revision to return to, so its hooks may not have run "+
			"(%s=%s): verify what they do, then remove the annotation; the next successful upgrade also clears it. Children: %s: %s",
			compositionMeta.AnnotationKeyHooksUnverified, since, out.reason, out.message),
	}
}

// upgradeRanHooks says a Reconcile ran a real helm install or upgrade, hooks included, and it
// succeeded: the only outcome that clears krateo.io/hooks-unverified on its own (#563).
//
// From plumbing's contract (v1.93.0 helm/v3/reconcile.go:85, "If NOT changed, return Changed:false —
// no Releases write, no hooks"), with every return that has no error enumerated:
//   - Changed=true: an initial install (:116), an upgrade to a changed desired state (:372), or the
//     upgrade after a drift heal (:600). Each ran helm with its hooks.
//   - RetriedFailedRelease: the upgrade retried over a failed latest revision (:600), Changed false
//     when nothing else differed. It ran the hooks too.
//   - DesiredStateHeld (:343) and the steady state (:573): nothing written, no hook ran.
//
// DesiredStateChange alone would miss the drift-heal and retry upgrades.
func upgradeRanHooks(res *helmconfig.ReconcileResult, err error) bool {
	return err == nil && res != nil && !res.DesiredStateHeld && (res.Changed || res.RetriedFailedRelease)
}

// patchHooksUnverified sets krateo.io/hooks-unverified to *value, or removes it when value is nil,
// with a JSON merge patch of that one key. Not tools.Update: a full update carries the
// resourceVersion and 409s under any concurrent writer (an umbrella re-applying the instance, a
// human edit), and the write this makes cannot be retried later by recomputing it. The mark is
// written BEFORE the rollback it describes (see the recovery in Observe), and a lost clear would keep
// a composition failed that a successful upgrade had healed. A merge patch has no precondition and
// touches no other key.
func patchHooksUnverified(ctx context.Context, mg *unstructured.Unstructured, value *string, opts tools.UpdateOptions) (*unstructured.Unstructured, error) {
	return patchAnnotation(ctx, mg, compositionMeta.AnnotationKeyHooksUnverified, value, opts)
}

// patchAnnotation sets one annotation key to *value, or removes it when value is nil, with a JSON
// merge patch of that key alone (no resourceVersion precondition, no other key touched). See
// patchHooksUnverified for why a merge patch and not tools.Update.
func patchAnnotation(ctx context.Context, mg *unstructured.Unstructured, key string, value *string, opts tools.UpdateOptions) (*unstructured.Unstructured, error) {
	if opts.Pluralizer == nil || opts.DynamicClient == nil {
		return mg, fmt.Errorf("patching %s: no pluralizer or dynamic client", key)
	}
	gvr, err := opts.Pluralizer.GVKtoGVR(mg.GroupVersionKind())
	if err != nil {
		return mg, fmt.Errorf("patching %s: %w", key, err)
	}
	var v any // nil removes the key in a merge patch
	if value != nil {
		v = *value
	}
	body, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]any{key: v}}})
	if err != nil {
		return mg, err
	}
	out, err := opts.DynamicClient.Resource(gvr).Namespace(mg.GetNamespace()).Patch(ctx, mg.GetName(), types.MergePatchType, body, metav1.PatchOptions{})
	if err != nil {
		return mg, fmt.Errorf("patching %s: %w", key, err)
	}
	return out, nil
}

// clearHooksUnverifiedAfter removes krateo.io/hooks-unverified when the Reconcile that just returned
// ran a real install or upgrade, hooks included, and it succeeded (#563). It returns the object to
// carry on with.
//
// A failed clear does not fail the pass: the upgrade succeeded, and the mark that stays is the
// conservative direction (Ready keeps saying the hooks are unverified, a human can remove it). It is
// said, as a Warning Event and a log line, because nothing will retry it: the next Reconcile is a
// no-op and runs no hook.
func (h *handler) clearHooksUnverifiedAfter(ctx context.Context, mg *unstructured.Unstructured, res *helmconfig.ReconcileResult, err error,
	opts tools.UpdateOptions, log logging.Logger, phase event.Action) *unstructured.Unstructured {
	since, marked := hooksUnverifiedSince(mg)
	if !marked || !upgradeRanHooks(res, err) {
		return mg
	}
	cleared, perr := patchHooksUnverified(ctx, mg, nil, opts)
	if perr != nil {
		log.Info("A helm upgrade ran the hooks, but krateo.io/hooks-unverified could not be removed; it stays until removed by hand",
			"since", since, "error", perr.Error())
		h.eventRecorder.Event(mg, event.Warning(reasonHooksUnverifiedNotClear, phase, fmt.Errorf(
			"a helm upgrade ran the release's hooks, but %s (set %s) could not be removed, so Ready keeps saying they may not have run: %w",
			compositionMeta.AnnotationKeyHooksUnverified, since, perr)))
		return mg
	}
	h.eventRecorder.Event(cleared, event.Normal(reasonHooksUnverifiedCleared, phase, fmt.Sprintf(
		"a helm upgrade ran the release's hooks, so %s (set %s) was removed", compositionMeta.AnnotationKeyHooksUnverified, since)))
	return cleared
}

// recoverStuckOperation rolls back a release stuck in an incomplete helm operation past its grace
// (Observe), and returns the object and the release to carry on with (#563). The target comes from
// recoveryTarget over the stored history: the last deployed revision, else the pending revision
// itself with krateo.io/hooks-unverified written FIRST. Every read or write it cannot complete
// returns an error before the rollback, never a guessed target.
//
// #629: a release that was never deployed is not rolled back to itself when gate allows; it is
// uninstalled without hooks so the runtime's Create installs it again (reinstallInterruptedFreshInstall).
// uninstalled then reports that the release is gone and Observe must report it absent.
func (h *handler) recoverStuckOperation(ctx context.Context, mg *unstructured.Unstructured, hc helmconfig.Client, rel *helmconfig.Release,
	releaseName string, gate reinstallGate, updateOpts tools.UpdateOptions, log logging.Logger) (_ *unstructured.Unstructured, _ *helmconfig.Release, uninstalled bool, _ error) {
	history, err := hc.History(ctx, releaseName, &helmconfig.HistoryConfig{})
	if err != nil {
		return mg, nil, false, fmt.Errorf("not rolling back release %s: reading its history to choose the target: %w", releaseName, err)
	}
	plan, err := recoveryTarget(rel, history)
	if err != nil {
		return mg, nil, false, fmt.Errorf("not rolling back release %s: %w", releaseName, err)
	}
	if plan.hooksUnverified {
		verdict, attempts, why := freshReinstallVerdict(mg, gate, rel, history)
		switch verdict {
		case reinstallAllowed:
			if mg, err = h.reinstallInterruptedFreshInstall(ctx, mg, hc, rel, releaseName, attempts, gate, updateOpts, log); err != nil {
				return mg, nil, false, err
			}
			return mg, nil, true, nil
		case reinstallCapped:
			h.eventRecorder.Event(mg, event.Warning(reasonInterruptedInstallReinstallCapped, "Observe", fmt.Errorf(
				"not reinstalling the interrupted install of revision %d again: %s; its hooks are marked unverified for a human", rel.Revision, why)))
			log.Info("Not reinstalling an interrupted fresh install again; falling back to the manual gate", "why", why)
		default:
			log.Debug("Interrupted install not reinstalled automatically", "why", why)
		}
	}
	log.Info("Recovering a stuck helm operation", "status", string(rel.Status), "revision", rel.Revision,
		"target", plan.revision, "hooksUnverified", plan.hooksUnverified, "why", plan.why)
	if plan.hooksUnverified {
		// The mark is written BEFORE the rollback it describes: a rollback that succeeded with
		// the mark lost would leave a deployed release that no later pass can tell was ever
		// interrupted. A failed write stops here, with the release still pending, and the next
		// pass retries both. A rollback that then fails leaves a mark that is still true.
		if _, marked := hooksUnverifiedSince(mg); !marked {
			now := time.Now().UTC().Format(time.RFC3339)
			if mg, err = patchHooksUnverified(ctx, mg, &now, updateOpts); err != nil {
				return mg, nil, false, fmt.Errorf("not rolling back release %s: marking its hooks unverified first: %w", releaseName, err)
			}
		}
		h.eventRecorder.Event(mg, event.Warning(reasonHooksUnverified, "Observe", errors.New(plan.why)))
	}
	priorRecord := priorHelmOperationRecord(mg)
	if mg, err = recordHelmOperation(ctx, mg, "rollback", time.Now(), updateOpts, log); err != nil {
		return mg, nil, false, err
	}
	rel, err = helmPhase(log, "rollback", releaseName, func() (*helmconfig.Release, error) {
		op, err := hc.Rollback(ctx, releaseName, rollbackConfig(plan.revision))
		h.audit.Record(ctx, auditrec.FromOperation(auditrec.ActionRollback, op, childrenLeft(releaseOf(op), err)), mg)
		return releaseOf(op), err
	})
	markWrote(ctx, writeRollback) // the rollback ran, which writes, whether or not it then failed
	mg = settleHelmOperationRecord(ctx, mg, priorRecord, err, updateOpts, log)
	if err != nil {
		return mg, nil, false, fmt.Errorf("rolling back release: %w", err)
	}
	return mg, rel, false, nil
}

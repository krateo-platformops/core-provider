package composition

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/krateo-platformops/composition-dynamic-controller/internal/auditrec"
	compositionMeta "github.com/krateo-platformops/composition-dynamic-controller/pkg/meta"
	helmconfig "github.com/krateo-platformops/plumbing/helm"
	"github.com/krateo-platformops/plumbing/kubeutil/event"
	"github.com/krateo-platformops/unstructured-runtime/pkg/logging"
	"github.com/krateo-platformops/unstructured-runtime/pkg/tools"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
)

// core-provider#629 (Diego, 2026-11-23): an interrupted FRESH install is reinstalled automatically.
//
// The case: a controller loses its lease (or its pod) mid-install, so the release is left
// pending-install at its first revision and the post-install hooks that had not run yet never will.
// #563's recovery rolls a stuck release back to its last DEPLOYED revision so the next upgrade runs
// its hooks; a fresh install has no such revision, so it was rolled back to itself and marked
// krateo.io/hooks-unverified, waiting for a human (measured: openstack-as-a-service r074f, GKE
// control-plane resize ~30 min after cluster creation, cyborg's ks-endpoints never ran).
//
// The fix, only for a release that was NEVER deployed: uninstall it WITHOUT hooks, delete the hook
// Jobs its release record names, and let the runtime's Create install it again, hooks included.
//
// The uninstall is the destructive part, so every condition below is a reason NOT to do it, and any
// state the recovery cannot read keeps the #563 human gate:
//   - "never deployed" is decided from the stored HISTORY: the interrupted operation is an install,
//     and every other revision failed. A deployed, superseded, uninstalled or unknown revision means
//     the release was live once, and an uninstall would drop real state (an openstack chart's
//     pre-delete hook drops the component's database).
//   - the uninstall runs WITHOUT hooks (helm DisableHooks), so no pre-delete hook runs even here.
//   - the kill switch krateo.io/interrupted-install-recovery, on the CompositionDefinition or the
//     composition, keeps the gate for a chart whose install hooks are not safe to run twice. Any value
//     counts: an unrecognised value is read as "manual", never as permission.
//   - the management policy must allow both create and delete.
//   - the attempts are counted on the composition and capped, so a release interrupted again and
//     again falls back to the gate instead of looping.

// maxInterruptedInstallReinstalls caps the automatic reinstalls of one composition. Three: one
// interruption is the case this exists for (a control-plane resize), a second covers another
// transient during the retry, and a third failure means the cause is not transient (a hook that
// hangs, a cluster that keeps dropping the lease), which is a human's to look at.
const maxInterruptedInstallReinstalls = 3

const (
	// reasonInterruptedInstallReinstalled is the Normal Event for an automatic reinstall.
	reasonInterruptedInstallReinstalled event.Reason = "InterruptedInstallReinstalled"
	// reasonInterruptedInstallReinstallCapped is the Warning Event when the cap sends the release to
	// the human gate instead.
	reasonInterruptedInstallReinstallCapped event.Reason = "InterruptedInstallReinstallCapped"
)

// reinstallGate is what Observe knows about whether an automatic reinstall may run this pass.
type reinstallGate struct {
	// policyAllows: the composition's management policy allows both create and delete.
	policyAllows bool
	// cdAnnotationsKnown: the CompositionDefinition object was read, so cdAnnotations is its truth.
	cdAnnotationsKnown bool
	cdAnnotations      map[string]string
	// dyn deletes the interrupted attempt's hook Jobs.
	dyn dynamic.Interface
}

// reinstallVerdict is why a stuck fresh install is, or is not, reinstalled automatically.
type reinstallVerdict int

const (
	reinstallAllowed reinstallVerdict = iota
	reinstallNotEligible
	reinstallCapped
)

// neverDeployed says whether pending is an interrupted install of a release no revision of which was
// ever deployed: the pending revision itself is pending-install, and every other stored revision
// failed. Anything else, including an entry it cannot read, is "deployed at some point", the side
// that keeps the release.
func neverDeployed(pending *helmconfig.Release, history []*helmconfig.Release) (bool, string) {
	if pending == nil {
		return false, "no release"
	}
	if pending.Status != helmconfig.StatusPendingInstall {
		return false, fmt.Sprintf("the interrupted operation is %s, not an install", pending.Status)
	}
	if len(history) == 0 {
		return false, "the release has no stored history"
	}
	found := false
	for _, r := range history {
		if r == nil {
			return false, "the history holds an entry that cannot be read"
		}
		if r.Revision == pending.Revision {
			if r.Status != helmconfig.StatusPendingInstall {
				return false, fmt.Sprintf("the history records revision %d as %s", r.Revision, r.Status)
			}
			found = true
			continue
		}
		if r.Status != helmconfig.StatusFailed {
			return false, fmt.Sprintf("revision %d is %s, so the release was live once", r.Revision, r.Status)
		}
	}
	if !found {
		return false, fmt.Sprintf("the history does not hold revision %d", pending.Revision)
	}
	return true, ""
}

// interruptedInstallReinstalls reads the attempt counter. A value that does not parse is an error:
// "could not count" must not read as "no attempts yet".
func interruptedInstallReinstalls(mg *unstructured.Unstructured) (int, error) {
	raw, ok := mg.GetAnnotations()[compositionMeta.AnnotationKeyInterruptedInstallReinstalls]
	if !ok {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("annotation %s=%q is not a count", compositionMeta.AnnotationKeyInterruptedInstallReinstalls, raw)
	}
	return n, nil
}

// freshReinstallVerdict decides whether this stuck release is reinstalled automatically, and says why
// not. It never errors: every state it cannot read is reinstallNotEligible, which keeps the gate.
func freshReinstallVerdict(mg *unstructured.Unstructured, gate reinstallGate, pending *helmconfig.Release, history []*helmconfig.Release) (reinstallVerdict, int, string) {
	if ok, why := neverDeployed(pending, history); !ok {
		return reinstallNotEligible, 0, why
	}
	if !gate.policyAllows {
		return reinstallNotEligible, 0, "the management policy withholds create or delete"
	}
	if !gate.cdAnnotationsKnown {
		return reinstallNotEligible, 0, "the CompositionDefinition's annotations could not be read, so its " +
			compositionMeta.AnnotationKeyInterruptedInstallRecovery + " cannot be ruled out"
	}
	if v, ok := gate.cdAnnotations[compositionMeta.AnnotationKeyInterruptedInstallRecovery]; ok {
		return reinstallNotEligible, 0, fmt.Sprintf("the CompositionDefinition sets %s=%q", compositionMeta.AnnotationKeyInterruptedInstallRecovery, v)
	}
	if v, ok := mg.GetAnnotations()[compositionMeta.AnnotationKeyInterruptedInstallRecovery]; ok {
		return reinstallNotEligible, 0, fmt.Sprintf("the composition sets %s=%q", compositionMeta.AnnotationKeyInterruptedInstallRecovery, v)
	}
	if gate.dyn == nil {
		return reinstallNotEligible, 0, "no client to delete the interrupted attempt's hook Jobs"
	}
	n, err := interruptedInstallReinstalls(mg)
	if err != nil {
		return reinstallNotEligible, 0, err.Error()
	}
	if n >= maxInterruptedInstallReinstalls {
		return reinstallCapped, n, fmt.Sprintf("it was already reinstalled automatically %d times (the cap), so the cause is not transient", n)
	}
	return reinstallAllowed, n, ""
}

// reinstallInterruptedFreshInstall clears an interrupted fresh install so the runtime's Create
// installs it again. Order, each step stopping the pass on failure with the release still pending,
// so the next pass redoes them (and counts again, so the cap bounds a step that keeps failing):
//  1. count the attempt on the composition, BEFORE anything is deleted;
//  2. delete the hook Jobs the release record names (#576's identity), while that record exists, so
//     the reinstall's hook Jobs cannot collide with the first attempt's whatever the chart's
//     hook-delete-policy;
//  3. uninstall WITHOUT hooks;
//  4. confirm the release is gone.
func (h *handler) reinstallInterruptedFreshInstall(ctx context.Context, mg *unstructured.Unstructured, hc helmconfig.Client, rel *helmconfig.Release,
	releaseName string, attempts int, gate reinstallGate, updateOpts tools.UpdateOptions, log logging.Logger) (*unstructured.Unstructured, error) {
	next := strconv.Itoa(attempts + 1)
	mg, err := patchAnnotation(ctx, mg, compositionMeta.AnnotationKeyInterruptedInstallReinstalls, &next, updateOpts)
	if err != nil {
		return mg, fmt.Errorf("not reinstalling release %s: counting the attempt first: %w", releaseName, err)
	}
	jobs, err := recordedHookJobs(rel)
	if err != nil {
		return mg, fmt.Errorf("not reinstalling release %s: reading its hook Jobs: %w", releaseName, err)
	}
	if remaining, err := deleteHookJobs(ctx, gate.dyn, jobs); err != nil {
		return mg, fmt.Errorf("not reinstalling release %s: %d hook Job(s) of the interrupted install could not be deleted: %w", releaseName, len(remaining), err)
	}
	priorRecord := priorHelmOperationRecord(mg)
	if mg, err = recordHelmOperation(ctx, mg, "uninstall", time.Now(), updateOpts, log); err != nil {
		return mg, err
	}
	err = helmPhaseErr(log, "uninstall", releaseName, func() error {
		op, err := hc.Uninstall(ctx, releaseName, &helmconfig.UninstallConfig{
			DisableHooks:   true, // no pre-delete hook runs: an interrupted install must not run teardown hooks
			IgnoreNotFound: true,
			Timeout:        helmOperationTimeout,
		})
		h.audit.Record(ctx, auditrec.FromOperation(auditrec.ActionUninstall, op, -1), mg)
		return err
	})
	markWrote(ctx, writeHelmUninstall) // the uninstall ran, which writes, whether or not it then failed
	mg = settleHelmOperationRecord(ctx, mg, priorRecord, err, updateOpts, log)
	if err != nil {
		return mg, fmt.Errorf("uninstalling the interrupted install of release %s: %w", releaseName, err)
	}
	gone, err := hc.GetRelease(ctx, releaseName, &helmconfig.GetConfig{})
	if err != nil {
		return mg, fmt.Errorf("confirming the interrupted install of release %s was uninstalled: %w", releaseName, err)
	}
	if gone != nil {
		return mg, fmt.Errorf("release %s still exists (revision %d, %s) after uninstalling its interrupted install", releaseName, gone.Revision, gone.Status)
	}
	log.Info("Reinstalling an interrupted fresh install", "release", releaseName, "revision", rel.Revision,
		"attempt", attempts+1, "max", maxInterruptedInstallReinstalls, "hookJobsDeleted", len(jobs))
	h.eventRecorder.Event(mg, event.Normal(reasonInterruptedInstallReinstalled, "Observe", fmt.Sprintf(
		"the install of revision %d was interrupted before it was ever deployed; uninstalled it without hooks and deleted %d hook Job(s), so it is installed again with all its hooks (attempt %d of %d; set %s: %s to keep the manual gate instead)",
		rel.Revision, len(jobs), attempts+1, maxInterruptedInstallReinstalls,
		compositionMeta.AnnotationKeyInterruptedInstallRecovery, compositionMeta.InterruptedInstallRecoveryManual)))
	return mg, nil
}

// clearInterruptedInstallReinstalls removes the attempt counter once the release has a deployed
// revision. A failed clear only leaves the counter, which costs a later interrupted install one of its
// attempts; it is logged, not failed.
func clearInterruptedInstallReinstalls(ctx context.Context, mg *unstructured.Unstructured, rel *helmconfig.Release, updateOpts tools.UpdateOptions, log logging.Logger) *unstructured.Unstructured {
	if rel == nil || rel.Status != helmconfig.StatusDeployed {
		return mg
	}
	if _, ok := mg.GetAnnotations()[compositionMeta.AnnotationKeyInterruptedInstallReinstalls]; !ok {
		return mg
	}
	cleared, err := patchAnnotation(ctx, mg, compositionMeta.AnnotationKeyInterruptedInstallReinstalls, nil, updateOpts)
	if err != nil {
		log.Info("Could not clear the interrupted-install reinstall counter; a later interrupted install has one attempt fewer",
			"error", err.Error())
		return mg
	}
	return cleared
}

package composition

import (
	"context"
	"fmt"
	"strconv"
	"time"

	compositionMeta "github.com/krateo-platformops/composition-dynamic-controller/pkg/meta"
	helmconfig "github.com/krateo-platformops/plumbing/helm"
	"github.com/krateo-platformops/plumbing/kubeutil/event"
	"github.com/krateo-platformops/unstructured-runtime/pkg/logging"
	"github.com/krateo-platformops/unstructured-runtime/pkg/tools"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
)

// core-provider#629: an interrupted FRESH install is recovered automatically; core-provider#636 (Diego,
// 2026-11-23): by RETRYING it, with its objects and volumes kept. This controller never uninstalls a
// release by itself.
//
// The case: a controller loses its lease (or its pod) mid-install, so the release is left
// pending-install and the post-install hooks that had not run yet never will. #563's recovery rolls a
// stuck release back to its last DEPLOYED revision so the next upgrade runs its hooks; a fresh install
// has no such revision, so it was rolled back to itself and marked krateo.io/hooks-unverified, waiting
// for a human (measured: openstack-as-a-service r074f, GKE control-plane resize ~30 min after cluster
// creation, cyborg's ks-endpoints never ran).
//
// The fix: the interrupted revision is recorded as FAILED (retrier.SettleInterruptedInstall), and the
// retry of a failed first install (failedinstall.go) installs it again with every install hook. Only when
//   - the release was never deployed: POSITIVE proof (neverDeployed), never the absence of a deployed
//     revision. Uninstalling or re-installing over a release that was live would touch real state, and
//     whatever a hook of it drops (openstack-helm's job_db_drop drops the component's database where it
//     is enabled; it is off by default, manifests.job_db_drop: false);
//   - this controller started the interrupted install (recordedInstall): the release name on a
//     composition can be a hand-installed release's, and that install is not this controller's to end;
//   - nobody is running it: the process that started it is gone, or it has been pending past the grace.
//     Settling writes over the record of an install that may still be running, and nothing in helm stops
//     that install from recording after it.
//
// Not proven, or not this controller's: #563's recovery exactly, nothing new. Proven, and not retried
// (the kill switch krateo.io/interrupted-install-recovery, the cap, a policy that withholds create):
//   - revision 1: #563's rollback to itself, marked krateo.io/hooks-unverified. Its stored manifest is
//     the render, so that re-applies what the install was applying;
//   - revision 2 and above, an interrupted RETRY: never rolled back. What such a revision stores is what
//     the failed attempts applied, not a render: a rollback would re-apply stale objects, take over ones
//     that are no longer the release's, and mark that deployed. It is settled and marked, and the upgrade
//     converges it; that upgrade runs upgrade hooks only, so it does not clear the mark.

// maxInterruptedInstallRetries caps the automatic retries of one composition's INTERRUPTED installs.
// Three: one interruption is the case this exists for (a control-plane resize), a second covers another
// transient during the retry, and a third failure means the cause is not transient (a hook that hangs, a
// cluster that keeps dropping the lease), which is a human's to look at. An install that FAILS is not
// counted: it ends by itself and is retried at the error backoff.
const maxInterruptedInstallRetries = 3

const (
	// reasonInterruptedInstallRetried is the Normal Event for an automatic retry of an interrupted install.
	reasonInterruptedInstallRetried event.Reason = "InterruptedInstallRetried"
	// reasonInterruptedInstallRetryCapped is the Warning Event when the cap sends the release to the human
	// gate instead.
	reasonInterruptedInstallRetryCapped event.Reason = "InterruptedInstallRetryCapped"
)

// interruptedGate is what Observe knows about whether an interrupted install may be retried this pass.
type interruptedGate struct {
	// heldByNobody: the process that started the operation is gone, or it is pending past the grace.
	heldByNobody bool
	// createAllowed: the composition's management policy allows create. The retry is an install.
	createAllowed bool
	// cdAnnotationsKnown: the CompositionDefinition object was read, so cdAnnotations is its truth.
	cdAnnotationsKnown bool
	cdAnnotations      map[string]string
	// dyn deletes the interrupted attempt's hook Jobs.
	dyn dynamic.Interface
}

// interruptedVerdict is what a stuck install is recovered by.
type interruptedVerdict int

const (
	// interruptedNotProven: not an install, not proven never deployed, or not this controller's. #563.
	interruptedNotProven interruptedVerdict = iota
	// interruptedRetry: settled as failed and retried as an install.
	interruptedRetry
	// interruptedManual: proven, and not retried automatically.
	interruptedManual
)

// interruptedInstallAttempts reads the attempt counter. A value that does not parse is an error:
// "could not count" must not read as "no attempts yet".
func interruptedInstallAttempts(mg *unstructured.Unstructured) (int, error) {
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

// interruptedCapped says whether the attempts reached the cap. An unreadable counter counts as reached.
func interruptedCapped(mg *unstructured.Unstructured) (bool, string) {
	n, err := interruptedInstallAttempts(mg)
	if err != nil {
		return true, err.Error()
	}
	if n >= maxInterruptedInstallRetries {
		return true, fmt.Sprintf("it was already retried automatically %d times after an interruption (the cap), so the cause is not transient", n)
	}
	return false, ""
}

// interruptedInstallVerdict decides how this stuck install is recovered, and says why it is not retried.
// It never errors: every state it cannot read is on the side that does less.
func interruptedInstallVerdict(mg *unstructured.Unstructured, releaseName string, gate interruptedGate, pending *helmconfig.Release, history []*helmconfig.Release) (interruptedVerdict, string) {
	if pending == nil || pending.Status != helmconfig.StatusPendingInstall {
		return interruptedNotProven, "the interrupted operation is not an install"
	}
	if ok, why := neverDeployed(mg, releaseName, pending, history); !ok {
		return interruptedNotProven, why
	}
	if ok, why := recordedInstall(mg, pending); !ok {
		return interruptedNotProven, why
	}
	if !gate.createAllowed {
		return interruptedManual, "the management policy withholds create"
	}
	if off, why := recoverySwitchedOff(mg, gate.cdAnnotationsKnown, gate.cdAnnotations); off {
		return interruptedManual, why
	}
	if gate.dyn == nil {
		return interruptedManual, "no client to delete the interrupted attempt's hook Jobs"
	}
	if capped, why := interruptedCapped(mg); capped {
		return interruptedManual, why
	}
	return interruptedRetry, ""
}

// settleInterruptedInstall records the interrupted revision as failed, so the retry of a failed first
// install can run in the same pass, and returns the release as it now is. Order, each step stopping the
// pass on failure with the release still pending-install, so the next pass redoes them (and counts
// again, so the cap bounds a step that keeps failing):
//
//  1. for a retry: count the attempt on the composition, and record it durably (status.installRecovery);
//     otherwise mark the hooks unverified, which is what keeps the upgrade that follows honest;
//  2. mark the release never deployed, while the evidence is whole;
//  3. for a retry: delete the hook Jobs the attempts recorded, and confirm they are gone;
//  4. settle that exact revision. Refused (it moved on) is an error: never a rollback instead.
func (h *handler) settleInterruptedInstall(ctx context.Context, mg *unstructured.Unstructured, hc helmconfig.Client, rel *helmconfig.Release, history []*helmconfig.Release,
	releaseName string, verdict interruptedVerdict, why string, gate interruptedGate, updateOpts tools.UpdateOptions, log logging.Logger) (*unstructured.Unstructured, stuckRecovery, error) {
	deleted := 0
	fail := func(step string, err error) (*unstructured.Unstructured, stuckRecovery, error) {
		return mg, stuckRecovery{}, fmt.Errorf("not settling the interrupted install of release %s (revision %d): %s: %w", releaseName, rel.Revision, step, err)
	}
	if !gate.heldByNobody {
		return fail("is anybody running it", fmt.Errorf("the process that started it may still be running it"))
	}
	var err error
	if verdict == interruptedRetry {
		n, cerr := interruptedInstallAttempts(mg)
		if cerr != nil {
			return fail("counting the attempt", cerr)
		}
		next := strconv.Itoa(n + 1)
		if mg, err = patchAnnotation(ctx, mg, compositionMeta.AnnotationKeyInterruptedInstallReinstalls, &next, updateOpts); err != nil {
			return fail("counting the attempt first", err)
		}
		if mg, err = recordInstallRecovery(ctx, mg, fmt.Sprintf(
			"Interrupted: the install of revision %d was interrupted before the release was ever deployed", rel.Revision), time.Now(), updateOpts, log); err != nil {
			return fail("recording it first", err)
		}
	} else if mg, err = h.markHooksUnverified(ctx, mg, updateOpts, "Observe", fmt.Errorf(
		"the install of revision %d was interrupted and the release was never deployed, but it is not retried (%s): it is upgraded instead, which does not run the install hooks, so they may not have run",
		rel.Revision, why)); err != nil {
		return fail("marking its hooks unverified first", err)
	}
	if mg, err = markNeverDeployed(ctx, mg, releaseName, rel, updateOpts); err != nil {
		return fail("marking the release never deployed first", err)
	}
	if verdict == interruptedRetry {
		if deleted, _, err = deleteAttemptsHookJobs(ctx, gate.dyn, history); err != nil {
			return fail("its hook Jobs", err)
		}
	}
	if err := retrier.SettleInterruptedInstall(ctx, hc, releaseName, rel.Revision); err != nil {
		return fail("recording it as failed", err)
	}
	settled, err := hc.GetRelease(ctx, releaseName, &helmconfig.GetConfig{})
	if err != nil {
		return fail("reading it back", err)
	}
	if settled == nil || settled.Revision != rel.Revision || settled.Status != helmconfig.StatusFailed {
		return fail("reading it back", fmt.Errorf("it is %s, not revision %d failed", releaseState(settled), rel.Revision))
	}
	log.Info("Settled an interrupted first install as failed", "release", releaseName, "revision", rel.Revision, "retried", verdict == interruptedRetry, "why", why, "hookJobsDeleted", deleted)
	return mg, stuckRecovery{rel: settled, interrupted: true, hookJobsDeleted: deleted}, nil
}

// clearInterruptedInstallAttempts removes the attempt counter once the release has a deployed
// revision. A failed clear only leaves the counter, which costs a later interrupted install one of its
// attempts; it is logged, not failed.
func clearInterruptedInstallAttempts(ctx context.Context, mg *unstructured.Unstructured, rel *helmconfig.Release, updateOpts tools.UpdateOptions, log logging.Logger) *unstructured.Unstructured {
	if rel == nil || rel.Status != helmconfig.StatusDeployed {
		return mg
	}
	if _, ok := mg.GetAnnotations()[compositionMeta.AnnotationKeyInterruptedInstallReinstalls]; !ok {
		return mg
	}
	cleared, err := patchAnnotation(ctx, mg, compositionMeta.AnnotationKeyInterruptedInstallReinstalls, nil, updateOpts)
	if err != nil {
		log.Info("Could not clear the interrupted-install attempt counter; a later interrupted install has one attempt fewer",
			"error", err.Error())
		return mg
	}
	return cleared
}

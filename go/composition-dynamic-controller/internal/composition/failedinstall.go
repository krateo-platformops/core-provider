package composition

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/krateo-platformops/composition-dynamic-controller/internal/auditrec"
	"github.com/krateo-platformops/composition-dynamic-controller/internal/conditionreason"
	"github.com/krateo-platformops/composition-dynamic-controller/internal/metrics"
	"github.com/krateo-platformops/composition-dynamic-controller/internal/tools/archive"
	compositionMeta "github.com/krateo-platformops/composition-dynamic-controller/pkg/meta"
	helmconfig "github.com/krateo-platformops/plumbing/helm"
	"github.com/krateo-platformops/plumbing/kubeutil/event"
	"github.com/krateo-platformops/plumbing/managementpolicy"
	"github.com/krateo-platformops/unstructured-runtime/pkg/controller"
	"github.com/krateo-platformops/unstructured-runtime/pkg/logging"
	"github.com/krateo-platformops/unstructured-runtime/pkg/meta"
	"github.com/krateo-platformops/unstructured-runtime/pkg/tools"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
)

// core-provider#636 (Diego, 2026-11-23): a failed FIRST install is retried as an install.
//
// A first install that fails (a post-install hook exits 1, or a graceful controller stop cancels helm)
// leaves revision 1 failed. Every site then retried it as an UPGRADE, which runs upgrade hooks only, so
// the composition read Ready=True/Available in seconds with its install hooks failed or never created
// (measured on 2.13.128). While no revision was EVER deployed (neverDeployed) of a release this
// controller installed (startedByController), the retry is now an install over the failed one: the
// install hooks run again, the objects the failed attempt created are kept, and nothing is uninstalled.
//
// ONE DECISION AND ONE ACTION, used by the three sites that retry a failed release: Observe (before its
// reconcile), Create (its "release already exists" branch) and Update (before its reconcile). In
// OBSERVE rather than by reporting the release absent so the runtime's Create runs it, because:
//   - absent is false: something is installed. Exists (existence.go) answers true for the same release,
//     and a consumer of the observation would be told the two things at once.
//   - the runtime's retry of a failed Create is dispatched as an Observe (the release exists, so the
//     create is recorded as succeeded), and the Create that Observe asks for is then dropped: its digest
//     is the one the retry itself holds (unstructured-runtime v1.32.1 enqueueFollowUp). The retry would
//     run once per resync and never at the error backoff.
//   - the update site needs it anyway: a spec change reaches Update without an Observe.
// An error from Observe is retried at the runtime's error backoff, as a failing upgrade's was.

// failedInstallPlan is what a pass does about a release whose latest revision failed.
type failedInstallPlan int

const (
	// planUpgrade: not failed at all, or not proven never deployed and installed by this controller. The
	// behaviour before #636.
	planUpgrade failedInstallPlan = iota
	// planInstallAgain: install over it, install hooks included.
	planInstallAgain
	// planUpgradeUnverified: never deployed, and installing again is switched off or withheld. The
	// upgrade runs, which skips the install hooks, so krateo.io/hooks-unverified is written first.
	planUpgradeUnverified
	// planReport: never deployed, and the policy withholds update, so nothing is written to the
	// release. Ready says the install hooks may not have run.
	planReport
	// planLeave: never deployed, and this controller is retiring. The install is the current version's
	// controller's (#519).
	planLeave
)

// failedInstall is the decision and what it was made from.
type failedInstall struct {
	plan     failedInstallPlan
	revision int // the failed latest revision
	// interrupted: this pass settled it as failed, having counted and recorded the retry already.
	interrupted bool
	// hookJobsDeleted: how many of the attempts' hook Jobs that settling deleted.
	hookJobsDeleted int
	why             string // why it is not simply installed again
	// attempts are the stored revisions, all of them attempts of this never-deployed release: their
	// recorded hook Jobs are deleted before it is installed again.
	attempts []*helmconfig.Release
}

// failedInstallGate is what a site knows about whether it may install the release again.
type failedInstallGate struct {
	retiring    bool
	retiringWhy string
	// createAllowed: the management policy allows create. The retry is an install.
	createAllowed bool
	// updateWithheld is set by Observe alone, the site whose writes the policy's update verb gates
	// (#597). The runtime gates Update's dispatch, and Create is gated by create.
	updateWithheld bool
	// interrupted: this pass's recovery settled the release's interrupted install as failed, having
	// deleted hookJobsDeleted of its hook Jobs.
	interrupted        bool
	hookJobsDeleted    int
	cdAnnotationsKnown bool
	cdAnnotations      map[string]string
	// dyn deletes the failed attempt's hook Jobs.
	dyn dynamic.Interface
}

// newFailedInstallGate reads the gate for the composition and the definition pkg describes.
func newFailedInstallGate(mg *unstructured.Unstructured, pkg *archive.Info, dyn dynamic.Interface) failedInstallGate {
	g := failedInstallGate{createAllowed: meta.ManagementDecision(mg).May(managementpolicy.Create), dyn: dyn}
	g.retiring, g.retiringWhy = retiringController(pkg)
	g.cdAnnotations, g.cdAnnotationsKnown = definitionAnnotations(pkg)
	return g
}

// definitionAnnotations are the CompositionDefinition's annotations, and whether they were read.
func definitionAnnotations(pkg *archive.Info) (map[string]string, bool) {
	if pkg == nil || pkg.CompositionDefinitionInfo == nil || pkg.CompositionDefinitionInfo.Annotations == nil {
		return nil, false
	}
	return pkg.CompositionDefinitionInfo.Annotations, true
}

// recoverySwitchedOff is the kill switch krateo.io/interrupted-install-recovery for both recoveries
// (#629, #636): set on the CompositionDefinition or on the composition, whatever its value, or a
// definition whose annotations could not be read, which cannot rule it out.
func recoverySwitchedOff(mg *unstructured.Unstructured, cdAnnotationsKnown bool, cdAnnotations map[string]string) (bool, string) {
	if !cdAnnotationsKnown {
		return true, "the CompositionDefinition's annotations could not be read, so its " +
			compositionMeta.AnnotationKeyInterruptedInstallRecovery + " cannot be ruled out"
	}
	if v, ok := cdAnnotations[compositionMeta.AnnotationKeyInterruptedInstallRecovery]; ok {
		return true, fmt.Sprintf("the CompositionDefinition sets %s=%q", compositionMeta.AnnotationKeyInterruptedInstallRecovery, v)
	}
	if v, ok := mg.GetAnnotations()[compositionMeta.AnnotationKeyInterruptedInstallRecovery]; ok {
		return true, fmt.Sprintf("the composition sets %s=%q", compositionMeta.AnnotationKeyInterruptedInstallRecovery, v)
	}
	return false, ""
}

// decideFailedInstall decides what to do about rel, whose status is failed, from its stored history.
func decideFailedInstall(mg *unstructured.Unstructured, releaseName string, gate failedInstallGate, rel *helmconfig.Release, history []*helmconfig.Release) failedInstall {
	if ok, why := neverDeployed(mg, releaseName, rel, history); !ok {
		return failedInstall{plan: planUpgrade, revision: rel.Revision, why: why}
	}
	if ok, why := startedByController(mg, releaseName, rel); !ok {
		return failedInstall{plan: planUpgrade, revision: rel.Revision, why: why}
	}
	fi := failedInstall{revision: rel.Revision, attempts: history, interrupted: gate.interrupted, hookJobsDeleted: gate.hookJobsDeleted}
	capped, capWhy := interruptedCapped(mg)
	_, gated := hooksUnverifiedSince(mg)
	switch off, why := recoverySwitchedOff(mg, gate.cdAnnotationsKnown, gate.cdAnnotations); {
	case gate.retiring:
		fi.plan, fi.why = planLeave, gate.retiringWhy
	case gate.updateWithheld:
		fi.plan, fi.why = planReport, "the management policy withholds update"
	case !gate.createAllowed:
		fi.plan, fi.why = planUpgradeUnverified, "the management policy withholds create"
	case off:
		fi.plan, fi.why = planUpgradeUnverified, why
	case capped && gated:
		// An INTERRUPTED install that reached the cap, settled and marked by this pass or an earlier one:
		// a human's, until they remove the mark. A failed install is not capped, whatever the counter says.
		fi.plan, fi.why = planUpgradeUnverified, capWhy
	case gate.dyn == nil:
		fi.plan, fi.why = planUpgradeUnverified, "no client to delete the failed attempt's hook Jobs"
	default:
		fi.plan = planInstallAgain
	}
	return fi
}

// assessFailedInstall decides what to do about rel and writes what must be on the composition BEFORE
// helm runs: the never-deployed mark, and krateo.io/hooks-unverified when the upgrade that follows will
// skip the install hooks. Any read or write it cannot complete is an error, with helm not called.
// A release whose latest revision did not fail costs nothing: the history is not read.
func (h *handler) assessFailedInstall(ctx context.Context, mg *unstructured.Unstructured, hc helmconfig.Client, rel *helmconfig.Release,
	releaseName string, gate failedInstallGate, updateOpts tools.UpdateOptions, log logging.Logger, phase event.Action) (*unstructured.Unstructured, failedInstall, error) {
	if rel == nil || rel.Status != helmconfig.StatusFailed {
		return mg, failedInstall{}, nil
	}
	history, err := hc.History(ctx, releaseName, &helmconfig.HistoryConfig{})
	if err != nil {
		return mg, failedInstall{}, fmt.Errorf("not retrying failed release %s: reading its history to decide how: %w", releaseName, err)
	}
	fi := decideFailedInstall(mg, releaseName, gate, rel, history)
	log.Info("The release's latest revision failed", "release", releaseName, "revision", rel.Revision, "plan", fi.plan.String(), "why", fi.why)
	switch fi.plan {
	case planUpgradeUnverified:
		if mg, err = h.markHooksUnverified(ctx, mg, updateOpts, phase, fmt.Errorf(
			"the first install failed (revision %d) and the release was never deployed, but it is not installed again (%s): it is upgraded instead, which does not run the install hooks, so they may not have run",
			rel.Revision, fi.why)); err != nil {
			return mg, fi, fmt.Errorf("not upgrading failed release %s: marking its hooks unverified first: %w", releaseName, err)
		}
	case planLeave:
		h.eventRecorder.Event(mg, event.Normal(reasonRetiringLeftInstall, phase, fmt.Sprintf(
			"The first install failed (revision %d); this controller is retiring and leaves installing it again to the current version's controller: %s",
			rel.Revision, fi.why)))
		return mg, fi, nil
	case planInstallAgain:
	default:
		return mg, fi, nil
	}
	// Both helm calls that can follow write a revision and can prune the ones that prove it.
	if mg, err = markNeverDeployed(ctx, mg, releaseName, rel, updateOpts); err != nil {
		return mg, fi, fmt.Errorf("not retrying failed release %s: marking it never deployed first: %w", releaseName, err)
	}
	return mg, fi, nil
}

// errInstallWhileDeleting: the composition was deleted before its failed install was installed again.
// Nothing was done; the Delete handler uninstalls the failed release.
var errInstallWhileDeleting = errors.New("the composition is being deleted, so its failed install is not installed again")

// installRetrier is plumbing's two calls, as an interface so a unit test can stand in for them.
type installRetrier interface {
	RetryFailedInstall(ctx context.Context, hc helmconfig.Client, releaseName, chartURL string, ac *helmconfig.ActionConfig, maxHistory int) (*helmconfig.OperationResult, error)
	SettleInterruptedInstall(ctx context.Context, hc helmconfig.Client, releaseName string, revision int) error
}

var retrier installRetrier = plumbingRetrier{}

// plumbingRetrier is what ships.
//
//   - RetryFailed: refused, nothing written, unless the latest stored revision is failed and none is
//     deployed; helm's install order with every install hook; the objects merged against what the failed
//     attempts applied, so an object or a field the new values drop is deleted. NOT a bare Replace: helm
//     then merges each live object with original == target, and what only the failed attempt had stays
//     live for good (measured by authn in envtest). Never with Atomic, which uninstalls a failed retry.
//   - SettleInterruptedInstall: records that exact revision as failed, refused, nothing written, unless it
//     is the latest, is pending-install, and none is deployed. It does not protect against a process that
//     is still running the install and records after it: proving there is none is the caller's.
type plumbingRetrier struct{}

func (plumbingRetrier) RetryFailedInstall(ctx context.Context, hc helmconfig.Client, releaseName, chartURL string, ac *helmconfig.ActionConfig, maxHistory int) (*helmconfig.OperationResult, error) {
	op, err := hc.Install(ctx, releaseName, chartURL, &helmconfig.InstallConfig{ActionConfig: ac, RetryFailed: true, MaxHistory: maxHistory})
	if errors.Is(err, helmconfig.ErrNotRetryableAsInstall) {
		err = fmt.Errorf("%w: %w", errRetryRefused, err)
	}
	return op, err
}

func (plumbingRetrier) SettleInterruptedInstall(ctx context.Context, hc helmconfig.Client, releaseName string, revision int) error {
	err := hc.SettleInterruptedInstall(ctx, releaseName, revision)
	if errors.Is(err, helmconfig.ErrNotInterruptedInstall) {
		err = fmt.Errorf("%w: %w", errRetryRefused, err)
	}
	return err
}

var (
	// errRetryRefused: the call started nothing and wrote nothing, because the release is not, or no
	// longer, one that can be installed again. It fails the pass like any other error: never an upgrade
	// instead, never an uninstall.
	errRetryRefused = errors.New("the retry was refused, nothing was written")
)

// retryMaxHistory bounds the retry's history. At least 2 when it is bounded: helm prunes to make room
// BEFORE it stores the new revision, so at 1 (HELM_MAX_HISTORY is the operator's to set) it would remove
// the failed revision the retry merges against. 0 is helm's "unlimited".
func retryMaxHistory(configured int) int {
	if configured == 0 {
		return 0
	}
	return max(configured, 2)
}

// retryRefused says whether a retry's error means it wrote no revision: refused, or somebody else's
// operation holds the release.
func retryRefused(err error) bool {
	return errors.Is(err, errRetryRefused) || errors.Is(err, helmconfig.ErrOperationInProgress)
}

// installAgain installs the release again over its failed attempts, install hooks included, after
// beforeInstallAgain.
func (h *handler) installAgain(ctx context.Context, mg *unstructured.Unstructured, hc helmconfig.Client, fi failedInstall, releaseName, chartURL string,
	ac *helmconfig.ActionConfig, gate failedInstallGate, updateOpts tools.UpdateOptions, log logging.Logger, phase event.Action) (*unstructured.Unstructured, *helmconfig.Release, error) {
	mg, priorRecord, started, err := h.beforeInstallAgain(ctx, mg, fi, releaseName, gate, updateOpts, log, phase)
	if err != nil {
		return mg, nil, err
	}
	rel, err := helmPhase(log, "install", releaseName, func() (*helmconfig.Release, error) {
		return metrics.NewHelmMetrics(ctx).TimedInstallWithResult(func() (*helmconfig.Release, error) {
			op, err := retrier.RetryFailedInstall(ctx, hc, releaseName, chartURL, ac, retryMaxHistory(helmMaxHistory))
			h.audit.Record(ctx, auditrec.FromOperation(auditrec.ActionInstall, op, childrenLeft(releaseOf(op), err)), mg)
			return releaseOf(op), err
		})
	})
	settleErr := err
	switch {
	case retryRefused(err):
		// As for an upgrade helm refused: the record just written is false, so the one it replaced, which
		// may name who holds the release, is put back (#520).
		settleErr = errors.Join(helmconfig.ErrOperationInProgress, err)
	case err != nil:
		markWrote(ctx, writeHelmInstall) // the install ran, which writes, although it then failed
	case rel == nil || rel.Status != helmconfig.StatusDeployed:
		// Never read as installed: Ready would then say Available over hooks that did not run.
		markWrote(ctx, writeHelmInstall)
		err = fmt.Errorf("it returned without a deployed release (%s)", releaseState(rel))
	default:
		markWrote(ctx, writeHelmInstall)
		mg = clearNeverDeployedAfterDeploy(ctx, mg, rel, updateOpts, log)
		// Here and not a pass later: the composition must not read Ready with the marks of a retry on it.
		mg = clearInterruptedInstallAttempts(ctx, mg, rel, updateOpts, log)
		// Every install hook ran, so a mark an earlier pass left (the kill switch, since removed) is over.
		mg = h.clearHooksUnverified(ctx, mg, updateOpts, log, phase, "a helm install ran the release's hooks")
	}
	// Last: the patches above return the stored object, which still carries the record.
	mg = settleHelmOperationRecord(ctx, mg, priorRecord, settleErr, updateOpts, log)
	if err != nil {
		mg, err = afterFailedInstall(ctx, mg, hc, releaseName, started, err, updateOpts, log)
		return mg, nil, fmt.Errorf("installing helm chart again after its first install failed (revision %d): %w", fi.revision, err)
	}
	clearInstallFailures(mg)
	return mg, rel, nil
}

// releaseState is a release's revision and status, for a message.
func releaseState(rel *helmconfig.Release) string {
	if rel == nil {
		return "no release"
	}
	return fmt.Sprintf("revision %d %s", rel.Revision, rel.Status)
}

// beforeInstallAgain is everything that must hold before the install, in this order, each step stopping
// the pass on failure with the release still failed, so the next pass redoes them. It returns the
// operation record it replaced and when it recorded this one.
//  1. record the retry durably (status.installRecovery), before the first deletion, unless settling an
//     interrupted install just did;
//  2. delete the hook Jobs the attempts recorded, and confirm they and their pods are gone: a Job of
//     the previous attempt may still be running (a cancelled install leaves it), and one left behind
//     collides with the retry's when the chart's hook-delete-policy omits before-hook-creation;
//  3. confirm the composition is not being deleted;
//  4. record the operation as this pod's, so a controller that dies in it is found by orphanedOperation.
//     Last, because that record is matched to the revision within orphanWindow.
func (h *handler) beforeInstallAgain(ctx context.Context, mg *unstructured.Unstructured, fi failedInstall, releaseName string,
	gate failedInstallGate, updateOpts tools.UpdateOptions, log logging.Logger, phase event.Action) (_ *unstructured.Unstructured, prior map[string]interface{}, started time.Time, err error) {
	if !fi.interrupted {
		if mg, err = recordInstallRecovery(ctx, mg, fi.revision, installRecoveryFailed, fmt.Sprintf(
			"the first install failed (revision %d) before the release was ever deployed", fi.revision), time.Now(), updateOpts, log); err != nil {
			return mg, nil, started, fmt.Errorf("not installing release %s again: %w", releaseName, err)
		}
	}
	deleted, recorded, err := deleteAttemptsHookJobs(ctx, gate.dyn, fi.attempts)
	if err != nil {
		return mg, nil, started, fmt.Errorf("not installing release %s again: %w", releaseName, err)
	}
	// After the wait above, and an error is "could not determine", which must not install: unlike the
	// self-heal's re-check (#276), not installing costs one pass.
	deleting, err := isBeingDeleted(ctx, mg, updateOpts)
	if err != nil {
		return mg, nil, started, fmt.Errorf("not installing release %s again: checking the composition is not being deleted: %w", releaseName, err)
	}
	if deleting {
		return mg, nil, started, errInstallWhileDeleting
	}
	prior, started = priorHelmOperationRecord(mg), time.Now()
	if mg, err = recordHelmOperation(ctx, mg, "install", started, updateOpts, log); err != nil {
		return mg, nil, started, err // a lost status race: not started unrecorded (#526), recomputed by the next pass
	}
	reason, what := reasonFailedInstallRetried, "failed"
	if fi.interrupted {
		reason, what = reasonInterruptedInstallRetried, "was interrupted"
	}
	h.eventRecorder.Event(mg, event.Normal(reason, phase, fmt.Sprintf(
		"the first install %s (revision %d) and the release was never deployed: installing it again with all its hooks over what it created, after deleting %d of the %d hook Job(s) it recorded, the others being absent (set %s: %s to keep this to a human)",
		what, fi.revision, fi.hookJobsDeleted+deleted, recorded, compositionMeta.AnnotationKeyInterruptedInstallRecovery, compositionMeta.InterruptedInstallRecoveryManual)))
	return mg, prior, started, nil
}

// deleteAttemptsHookJobs deletes the hook Jobs every stored attempt recorded, and confirms they and their
// pods are gone. It returns how many it DELETED, of how many they recorded: the rest were not there.
func deleteAttemptsHookJobs(ctx context.Context, dyn dynamic.Interface, attempts []*helmconfig.Release) (deleted, recorded int, _ error) {
	var jobs []hookJobRef
	for _, r := range attempts {
		refs, err := recordedHookJobs(r)
		if err != nil {
			return 0, 0, fmt.Errorf("reading the hook Jobs of revision %d: %w", r.Revision, err)
		}
		jobs = append(jobs, refs...)
	}
	jobs = uniqueHookJobs(jobs)
	deleted, err := deleteHookJobsAndWait(ctx, dyn, jobs, hookJobsGoneTimeout)
	return deleted, len(jobs), err
}

// failInstallAgain is how Observe and Update fail when installAgain did. A failed ATTEMPT recorded its own
// Ready (recordInstallFailure); the error is marked so the runtime leaves it. Anything else, a retry that
// did not start, is a finding on Ready, InstallFailing where that is what Ready already says. A lost status
// race is returned as it is: the runtime requeues it quietly.
func failInstallAgain(ctx context.Context, mg *unstructured.Unstructured, cause error, opts tools.UpdateOptions) error {
	if errors.Is(cause, tools.ErrStatusConflict) {
		return cause
	}
	var failed *installFailed
	if errors.As(cause, &failed) && failed.recorded {
		return controller.ReadyAssessed(cause)
	}
	if cond, failing := installFailing(mg, "the next retry did not start: "+boundedError(cause)); failing {
		return failObserveWithCondition(ctx, mg, cond, cause, opts)
	}
	return failObserveWithFinding(ctx, mg, cause, opts)
}

// withInstallHooksNotRun makes Ready say the install hooks of a failed first install may not have run
// when nothing is written to the release (planReport): without it a healthy rollup over a failed release
// reads Ready=True. It is computed each pass, not marked on the composition, because it clears by
// itself when the policy allows update and the release is installed again.
func withInstallHooksNotRun(out readyOutcome, notRun string) readyOutcome {
	if notRun == "" || out.reason == conditionreason.ReasonHooksMayNotHaveRun {
		return out
	}
	return readyOutcome{reason: conditionreason.ReasonHooksMayNotHaveRun,
		message: fmt.Sprintf("%s. Children: %s: %s", notRun, out.reason, out.message)}
}

// installHooksNotRunMessage is withInstallHooksNotRun's text for a decision, or "".
func installHooksNotRunMessage(fi failedInstall) string {
	if fi.plan != planReport {
		return ""
	}
	return fmt.Sprintf("the first install failed (revision %d) and the release was never deployed, so its install hooks may not have run; "+
		"it is not installed again because %s", fi.revision, fi.why)
}

func (p failedInstallPlan) String() string {
	switch p {
	case planInstallAgain:
		return "install again"
	case planUpgradeUnverified:
		return "upgrade, install hooks unverified"
	case planReport:
		return "report only"
	case planLeave:
		return "leave to the current version's controller"
	default:
		return "upgrade"
	}
}

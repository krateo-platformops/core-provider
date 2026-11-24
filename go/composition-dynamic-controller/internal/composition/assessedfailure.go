package composition

import (
	"context"
	"fmt"
	"time"

	"github.com/krateo-platformops/composition-dynamic-controller/internal/chartinspector"
	"github.com/krateo-platformops/composition-dynamic-controller/internal/conditionreason"
	"github.com/krateo-platformops/composition-dynamic-controller/internal/tools/rbac"
	"github.com/krateo-platformops/unstructured-runtime/pkg/tools/unstructured/condition"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/krateo-platformops/unstructured-runtime/pkg/controller"
	"github.com/krateo-platformops/unstructured-runtime/pkg/tools"
	unstructuredtools "github.com/krateo-platformops/unstructured-runtime/pkg/tools/unstructured"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// failObserveWithFinding is how Observe fails when it has DIAGNOSED the failure: it records
// Ready=False/Unavailable with the cause, persists it, and returns the cause marked
// controller.ReadyAssessed, so the runtime writes only Synced=ReconcileError and leaves that finding
// in place (core-provider#508).
//
// Without the mark the runtime's observe-failed path overwrites Ready with Unknown/ObserveFailed
// ("could not be observed"). That hid the cause, made the composition roll up into its parent as
// not assessed (childUnevaluated) instead of failed, and flipped Ready False -> Unknown on every
// pass: two status writes per reconcile and a lastTransitionTime that moved every time. Measured on
// krateo-057's alerts-page, whose helm reconcile was refused. unstructured-runtime added
// ReadyAssessed for exactly these sites, the derived-RBAC failure and the helm reconcile failure.
//
// If the status write itself fails, the finding never reached the object, so the error is returned
// WITHOUT the mark: the runtime then records what it can (and a conflict requeues quietly, as
// before).
func failObserveWithFinding(ctx context.Context, mg *unstructured.Unstructured, cause error, opts tools.UpdateOptions) error {
	return failObserveWithCondition(ctx, mg, conditionreason.Unavailable.WithMessage(cause.Error()), cause, opts)
}

// failObserveWithCondition is failObserveWithFinding with the Ready to record.
func failObserveWithCondition(ctx context.Context, mg *unstructured.Unstructured, cond metav1.Condition, cause error, opts tools.UpdateOptions) error {
	if err := unstructuredtools.SetConditions(mg, cond); err != nil {
		return fmt.Errorf("recording the failure on Ready: %w (cause: %v)", err, cause)
	}
	if _, err := updateStatus(ctx, mg, opts); err != nil {
		return fmt.Errorf("updating status after failure: %w", err)
	}
	return controller.ReadyAssessed(cause)
}

// inspectorUnreachableGrace is how long a composition keeps its last Ready while chart-inspector
// gives no answer (#577, Diego 2026-11-18: bounded, 5 minutes). A core-provider roll is ~35 s; a
// chart-inspector that is really down reaches Ready, and every gate reading it, within 5 minutes.
const inspectorUnreachableGrace = 5 * time.Minute

// keepReadyWhileInspectorUnreachable decides whether an Observe that failed on cause may leave Ready
// as last assessed instead of recording Ready=False/Unavailable. All of:
//
//   - cause is chart-inspector giving NO answer (chartinspector.IsUnreachable). Any HTTP answer, a
//     5xx included, is a verdict and keeps today's path;
//   - a Ready is stored: there is a last assessment to keep. A composition never assessed is not
//     given a silent Ready-less window;
//   - ChartRenderable reads False/InspectorUnavailable (applyDerivedRBAC wrote it this pass), and its
//     lastTransitionTime is under inspectorUnreachableGrace before now.
//
// THE ANCHOR IS STORED ON THE OBJECT, so the grace survives a cdc restart and needs no in-memory
// clock. It is ChartRenderable's False transition, which condition.Upsert keeps across same-status
// writes: if ChartRenderable was already False for another reason (a broken chart), the anchor is
// that older transition and the grace is SHORTER, never longer. A condition list that does not parse
// reads as no anchor (GetConditions is all-or-nothing), which also takes today's path.
func keepReadyWhileInspectorUnreachable(mg *unstructured.Unstructured, cause error, now time.Time) (bool, time.Time) {
	if !chartinspector.IsUnreachable(cause) {
		return false, time.Time{}
	}
	if !hasCondition(mg, condition.TypeReady) {
		return false, time.Time{}
	}
	cr := unstructuredtools.GetCondition(mg, conditionreason.ChartInspectorUnavailable.Type(), conditionreason.ChartInspectorUnavailable.String())
	if cr == nil || cr.Status != metav1.ConditionFalse {
		return false, time.Time{}
	}
	deadline := cr.LastTransitionTime.Add(inspectorUnreachableGrace)
	return now.Before(deadline), deadline
}

// hasCondition reports whether a condition of type condType is stored, whatever its reason.
func hasCondition(mg *unstructured.Unstructured, condType string) bool {
	for _, c := range unstructuredtools.GetConditions(mg) {
		if c.Type == condType {
			return true
		}
	}
	return false
}

// failObserveKeepingReady is failObserveWithFinding's counterpart inside the grace: it persists the
// conditions this pass wrote (ChartRenderable=False/InspectorUnavailable) and returns the cause marked
// controller.ReadyAssessed, so the runtime writes Synced=ReconcileError and leaves Ready as it was.
// The returned error requeues, so the pass after the deadline records Ready=False/Unavailable.
func failObserveKeepingReady(ctx context.Context, mg *unstructured.Unstructured, cause error, deadline time.Time, opts tools.UpdateOptions) error {
	if _, err := updateStatus(ctx, mg, opts); err != nil {
		return fmt.Errorf("updating status after failure: %w", err)
	}
	return controller.ReadyAssessed(readyKeptUntil(cause, deadline))
}

// readyKeptUntil is cause saying that Ready was left as last assessed, and until when.
func readyKeptUntil(cause error, deadline time.Time) error {
	return fmt.Errorf("%w (chart-inspector gave no answer; Ready is kept as last assessed until %s)",
		cause, deadline.UTC().Format(time.RFC3339))
}

// failObserveOnDerivedRBAC is how Observe fails when applyDerivedRBAC did. #577: chart-inspector
// giving NO answer (a core-provider roll, ~35 s) is not a finding about this composition, so inside
// the bounded grace Ready stays as last assessed; after it, or for any answer chart-inspector did
// give, the failure is recorded on Ready as before.
func failObserveOnDerivedRBAC(ctx context.Context, mg *unstructured.Unstructured, cause error, now time.Time, opts tools.UpdateOptions) error {
	if keep, deadline := keepReadyWhileInspectorUnreachable(mg, cause, now); keep {
		return failObserveKeepingReady(ctx, mg, cause, deadline, opts)
	}
	if chartinspector.IsUnreachable(cause) {
		// The grace is over (or never applied): record what applyDerivedRBAC deferred for it.
		setRBACCompleteCondition(ctx, mg, rbacCompleteCondition(false, rbac.ApplyReport{}, nil))
	}
	return failObserveWithFinding(ctx, mg, cause, opts)
}

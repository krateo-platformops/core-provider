package composition

import (
	"context"
	"errors"
	"time"

	"github.com/krateo-platformops/composition-dynamic-controller/internal/chartinspector"
	"github.com/krateo-platformops/composition-dynamic-controller/internal/conditionreason"
	"github.com/krateo-platformops/composition-dynamic-controller/internal/tools/rbac"
	"github.com/krateo-platformops/unstructured-runtime/pkg/tools"
	unstructuredtools "github.com/krateo-platformops/unstructured-runtime/pkg/tools/unstructured"
	"github.com/krateo-platformops/unstructured-runtime/pkg/tools/unstructured/condition"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// core-provider#646: the conditions a FAILED Create leaves on the composition.
//
// Observe persists what it computed before it fails (failObserveWithFinding). Create cannot: after a
// failed create the runtime re-fetches the object and writes Ready=False/Creating and
// Synced=False/ReconcileError on that copy, so what Create set in memory is lost. A first render the
// chart refused then carried no ChartRenderable and read Creating, which a parent counts converging. What
// Create RETURNS is applied on that copy, in the same write (unstructured-runtime v1.32.1: every type but
// Synced), so Create returns it.
//
//   - Derived RBAC failed: ChartRenderable and RBACComplete, as Observe records the same failure, and Ready
//     only where it is a VERDICT (Diego, 2026-11-24): the chart or the values refused the render, or the API
//     server refused the RBAC. Then Ready=False/Unavailable, as Observe. Anything else says nothing about the
//     composition that a retry may not change (a CRD not served yet, a chart-inspector that is down), so a
//     first create stays the runtime's Creating, which a parent counts converging. It is not an install
//     attempt: nothing is counted in status.installFailures.
//   - Anything after it failed: ChartRenderable and RBACComplete as this pass computed them, so a refusal
//     an earlier pass recorded does not outlive the render that cleared it. Ready is what it was: the
//     runtime's Creating, or InstallFailing (installfailing.go).
//   - Anything before it failed: nothing was computed, nothing is returned.

// createFailed is a failed Create with the conditions it computed.
type createFailed struct {
	err   error
	conds []metav1.Condition
}

func (f *createFailed) Error() string { return f.err.Error() }
func (f *createFailed) Unwrap() error { return f.err }

// carryOnFailedCreate returns err carrying conds, unless they must not be written: a lost status race
// (computed from a stale read, the next pass recomputes), a cancelled pass (its errors are not findings),
// or a composition that is being deleted, or whose deletion could not be ruled out. Without them the
// failure is recorded as any other.
func carryOnFailedCreate(ctx context.Context, mg *unstructured.Unstructured, err error, conds []metav1.Condition, opts tools.UpdateOptions) error {
	if err == nil || len(conds) == 0 || errors.Is(err, tools.ErrStatusConflict) || ctx.Err() != nil {
		return err
	}
	if deleting, derr := isBeingDeleted(ctx, mg, opts); derr != nil || deleting {
		return err
	}
	return &createFailed{err: err, conds: conds}
}

// failedCreateConditions is what CreateWithResult returns for a Create that failed with err.
func failedCreateConditions(err error) []metav1.Condition {
	var conds []metav1.Condition
	var computed *createFailed
	if errors.As(err, &computed) {
		conds = append(conds, computed.conds...)
	}
	var failed *installFailed
	if errors.As(err, &failed) && failed.ready.Reason == conditionreason.ReasonInstallFailing {
		conds = append(conds, failed.ready)
	}
	return conds
}

// createFailedOnDerivedRBAC is how Create fails when applyDerivedRBAC did: the conditions to return, and the
// error. Where it returns no Ready, the runtime's Creating stands, with the error. On a failure that is not a
// verdict two Readys are kept instead of falling to Creating and back:
//
//   - the stored one, inside the grace for a chart-inspector that gives no answer (#577). Not a stored True:
//     Create runs because the release is absent, so that is no assessment of what there is now;
//   - InstallFailing, which is as true as before: the first install has failed N times.
func createFailedOnDerivedRBAC(ctx context.Context, mg *unstructured.Unstructured, cause error, now time.Time) ([]metav1.Condition, error) {
	keep, deadline := keepReadyWhileInspectorUnreachable(mg, cause, now)
	stored := conditionsOn(mg, condition.TypeReady)
	if keep && stored[0].Status != metav1.ConditionTrue {
		return append(derivedRBACConditions(mg), stored...), readyKeptUntil(cause, deadline)
	}
	if chartinspector.IsUnreachable(cause) {
		setRBACCompleteCondition(ctx, mg, rbacCompleteCondition(false, rbac.ApplyReport{}, nil))
	}
	conds := derivedRBACConditions(mg)
	if isCreateVerdict(cause) {
		return append(conds, conditionreason.Unavailable.WithMessage(cause.Error())), cause
	}
	if cond, failing := installFailing(mg, "the next retry did not start: "+boundedError(cause)); failing {
		return append(conds, cond), cause
	}
	// Recorded as failing, and its count was not (recordInstallFailure's write failed, its Ready was returned).
	if len(stored) == 1 && stored[0].Reason == conditionreason.ReasonInstallFailing {
		return append(conds, stored[0]), cause
	}
	return conds, cause
}

// isCreateVerdict says whether applyDerivedRBAC failed on a verdict: an ALLOW-LIST, so a reason nobody
// classified, or an error that cannot be determined, is not one.
func isCreateVerdict(cause error) bool {
	var apply *rbacApplyFailed
	if errors.As(cause, &apply) {
		return rbacRefused(apply.err)
	}
	reason, _ := chartRenderableFailure(cause)
	return chartVerdict(reason)
}

// chartVerdict says whether a ChartRenderable reason means the chart or the values are wrong.
func chartVerdict(reason conditionreason.Reason) bool {
	switch reason {
	case conditionreason.ChartTemplateRefused, conditionreason.ChartValuesSchemaViolation:
		return true
	}
	return false
}

// rbacApplyFailed is applyDerivedRBAC's failure to APPLY what it derived. Typed, because an API server's
// refusal is a verdict on this step only: a Forbidden met resolving the kind is not one.
type rbacApplyFailed struct{ err error }

func (f *rbacApplyFailed) Error() string { return "applying rbac: " + f.err.Error() }
func (f *rbacApplyFailed) Unwrap() error { return f.err }

// rbacRefused says whether the API server refused any of it: Forbidden (an escalation included) or Invalid.
// Every error of the tree is read, since errors.As stops at the first: one refused object is enough, whatever
// failed beside it. Not a namespace being deleted, whose Forbidden ends with the namespace.
func rbacRefused(err error) bool {
	if status, ok := err.(apierrors.APIStatus); ok {
		switch status.Status().Reason {
		case metav1.StatusReasonForbidden:
			return !apierrors.HasStatusCause(err, corev1.NamespaceTerminatingCause)
		case metav1.StatusReasonInvalid:
			return true
		}
		return false
	}
	switch w := err.(type) {
	case interface{ Unwrap() error }:
		return rbacRefused(w.Unwrap())
	case interface{ Unwrap() []error }:
		for _, e := range w.Unwrap() {
			if rbacRefused(e) {
				return true
			}
		}
	}
	return false
}

// derivedRBACConditions are the conditions applyDerivedRBAC sets, as they are on mg.
func derivedRBACConditions(mg *unstructured.Unstructured) []metav1.Condition {
	return conditionsOn(mg, ConditionTypeChartRenderable, ConditionTypeRBACComplete)
}

// conditionsOn is mg's conditions of the given types.
func conditionsOn(mg *unstructured.Unstructured, types ...string) []metav1.Condition {
	var out []metav1.Condition
	for _, c := range unstructuredtools.GetConditions(mg) {
		for _, t := range types {
			if c.Type == t {
				out = append(out, c)
			}
		}
	}
	return out
}

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
//   - Derived RBAC failed (a refused render, a chart-inspector that gives no answer, a refused RBAC
//     apply): ChartRenderable, RBACComplete and Ready, as Observe records the same failure. It is not an
//     install attempt: nothing is counted in status.installFailures.
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

// createFailedOnDerivedRBAC is how Create fails when applyDerivedRBAC did: the conditions
// failObserveOnDerivedRBAC would persist, and the error to return.
//
// Inside the grace for a chart-inspector that gives no answer (#577) the stored Ready is RETURNED, or the
// runtime would write Creating over it. Not a stored True: Create runs because the release is absent, so
// there is no last assessment of THIS state to keep, as on a first create.
func createFailedOnDerivedRBAC(ctx context.Context, mg *unstructured.Unstructured, cause error, now time.Time) ([]metav1.Condition, error) {
	keep, deadline := keepReadyWhileInspectorUnreachable(mg, cause, now)
	if stored := conditionsOn(mg, condition.TypeReady); keep && stored[0].Status != metav1.ConditionTrue {
		return append(derivedRBACConditions(mg), stored...), readyKeptUntil(cause, deadline)
	}
	if chartinspector.IsUnreachable(cause) {
		setRBACCompleteCondition(ctx, mg, rbacCompleteCondition(false, rbac.ApplyReport{}, nil))
	}
	return append(derivedRBACConditions(mg), conditionreason.Unavailable.WithMessage(cause.Error())), cause
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

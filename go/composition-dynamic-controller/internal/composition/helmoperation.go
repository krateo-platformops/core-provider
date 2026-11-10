package composition

import (
	"context"
	"fmt"
	"time"

	"github.com/krateo-platformops/unstructured-runtime/pkg/logging"
	"github.com/krateo-platformops/unstructured-runtime/pkg/tools"
	unstructuredtools "github.com/krateo-platformops/unstructured-runtime/pkg/tools/unstructured"
	"github.com/krateo-platformops/unstructured-runtime/pkg/tools/unstructured/condition"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// helmOperationInProgress is the Ready condition written BEFORE an install or uninstall starts
// (#410). On krateo-057 a composition's install sat on a hook that could never start for three
// hours with no condition at all, and its deletion waited behind it just as silently. The message
// says what is running, since when, and the bound that applies to each of its hooks and waits.
func helmOperationInProgress(phase string, started time.Time, bound time.Duration) metav1.Condition {
	c := condition.FailWithReason(reasonHelmOperationInProgress)
	c.Message = fmt.Sprintf("helm %s in progress since %s; each hook and each wait must finish within %s, or the operation fails",
		phase, started.UTC().Format(time.RFC3339), bound)
	return c
}

// recordHelmOperation writes helmOperationInProgress to mg's status and returns the updated object,
// so the caller's later writes start from the current resourceVersion.
//
// Only for install (Create) and uninstall (Delete). Update and Observe's reconcile run on live
// compositions every time something drifts, and writing Ready=False before each of those would make
// Ready flap on every self-heal; their operations are still bounded by helmOperationTimeout.
//
// A status conflict is returned (the runtime requeues quietly and the next pass writes it), any other
// write failure is logged and the operation goes ahead: the condition is a report, not a gate. Same
// rule as the PendingHelmOperation write (pendingConditionWriteOutcome).
func recordHelmOperation(ctx context.Context, mg *unstructured.Unstructured, phase string, started time.Time,
	opts tools.UpdateOptions, log logging.Logger) (*unstructured.Unstructured, error) {
	unstructuredtools.SetConditions(mg, helmOperationInProgress(phase, started, helmOperationTimeout))
	updated, err := updateStatus(ctx, mg, opts)
	if err != nil {
		if werr := pendingConditionWriteOutcome(err); werr != nil {
			return mg, werr
		}
		log.Debug("Could not record the helm operation in progress", "phase", phase, "error", err.Error())
		return mg, nil
	}
	if updated == nil {
		return mg, nil
	}
	return updated, nil
}

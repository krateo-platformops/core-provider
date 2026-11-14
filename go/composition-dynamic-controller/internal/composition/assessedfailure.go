package composition

import (
	"context"
	"fmt"

	"github.com/krateo-platformops/unstructured-runtime/pkg/controller"
	"github.com/krateo-platformops/unstructured-runtime/pkg/tools"
	unstructuredtools "github.com/krateo-platformops/unstructured-runtime/pkg/tools/unstructured"
	"github.com/krateo-platformops/unstructured-runtime/pkg/tools/unstructured/condition"
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
	cond := condition.Unavailable()
	cond.Message = cause.Error()
	if err := unstructuredtools.SetConditions(mg, cond); err != nil {
		return fmt.Errorf("recording the failure on Ready: %w (cause: %v)", err, cause)
	}
	if _, err := updateStatus(ctx, mg, opts); err != nil {
		return fmt.Errorf("updating status after failure: %w", err)
	}
	return controller.ReadyAssessed(cause)
}

package composition

import (
	"context"
	"errors"
	"fmt"

	"github.com/krateo-platformops/composition-dynamic-controller/internal/writtenby"
	"github.com/krateo-platformops/unstructured-runtime/pkg/tools"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// updateStatus writes mg's status subresource ONCE. A 409 is RETURNED, never retried with mg's body.
//
// IT USED TO RETRY, AND THE RETRY WAS A LOST UPDATE (#359). On a conflict it re-read the object
// solely to copy the current resourceVersion onto mg, then re-submitted mg's status. That is not a
// merge: it replaces whatever the newer write recorded with this pass's view, which was computed
// from a read the conflict had just proven stale. The justification was "this controller owns the
// status subresource, so last-writer-wins is safe". That is false whenever two passes of THIS
// controller overlap: the stale one wins. Measured on a snowplow bench cluster (cdc 2.13.57,
// portals/krateo-system/portal). A recovery pass that read the CR before Create recorded the
// inventory got the 409, adopted the fresh resourceVersion, and wiped status.managed (57 children)
// and previousManagedCount, leaving managedDigest at its schema default "". On 2.13.57 nothing ever
// repaired it (#326).
//
// A conflict means this pass's inputs were stale, so its computed status is too. Returning the
// error makes the runtime requeue the pass, and the next pass recomputes from the current object.
// That still converges the case the retry was added for (#57, two per-version controllers briefly
// contending during a GVK handover): the next pass reads what the other wrote and writes on top of
// it, instead of forcing a stale write through. Concurrent passes of one controller on one object
// are the runtime's to prevent (unstructured-runtime#62).
//
// EVERY STATUS THE HANDLER WRITES GOES OUT HERE, so this is where status.writtenBy is stamped (#669):
// the build stamp the entry point put on ctx (withPassContext). Stamped on the body about to be sent,
// never a reason to send one, so a pass that changes nothing else sends back the stored stamp and the
// apiserver records nothing. Not on mg earlier in the pass: a main-resource Update in between
// (tools.Update) returns the stored status and would drop a stamp put on before it.
func updateStatus(ctx context.Context, mg *unstructured.Unstructured, opts tools.UpdateOptions) (*unstructured.Unstructured, error) {
	if err := writtenby.FromContext(ctx).Set(mg); err != nil {
		return nil, fmt.Errorf("setting status.writtenBy: %w", err)
	}
	updated, err := tools.UpdateStatus(ctx, mg, opts)
	if apierrors.IsConflict(err) {
		return nil, fmt.Errorf("status computed from a stale read conflicts with a newer write; "+
			"requeueing to recompute from the current object: %w", err)
	}
	return updated, err
}

// pendingConditionWriteOutcome decides what a failed write of the PendingHelmOperation condition does
// to the pass. It is separate from Observe because that path needs a helm release stuck in a
// pending state, which no unit test here can build. The decision is the part that can be wrong.
//
// A STATUS CONFLICT IS RETURNED. The write lost a race, so the condition was not recorded and the
// composition keeps asserting whatever it said before: typically Ready=True "up-to-date" while a
// spec change waits behind the pending lock, which is the false claim this condition exists to
// replace. Returning it makes the runtime requeue quietly after 1s (unstructured-runtime >= v1.9.0:
// no ReconcileError, no event, no retry-budget cost), and the next pass records the condition.
//
// Any OTHER error is swallowed, as before: the wait itself is not an error, and failing the reconcile
// on a cosmetic write would burn retry budget. That reason never applied to a conflict, which costs
// no budget.
func pendingConditionWriteOutcome(err error) error {
	if errors.Is(err, tools.ErrStatusConflict) {
		return err
	}
	return nil
}

// Package readyreason is the closed set of reasons a composition's Ready condition can carry
// (core-provider#515), and the only place in the composition-dynamic-controller that constructs a
// Ready condition.
//
// Consumers gate on Ready's reason with no else-arm: installer's dependency gate and verifier refuse a
// reason they do not know, by design. Until this package the set existed only as whatever the code
// happened to write, and consumers learned it one incident at a time (InventoryNotRecorded,
// HelmOperationInProgress). Here it is declared, and enforced by construction:
//
//   - Reason has unexported fields, so code outside this package cannot make one. A cdc Ready write
//     must name one of the values below, and a reason outside the set does not compile.
//   - TestNoReadyConditionBuiltOutsideReadyreason (internal/composition) refuses any other
//     construction of a Ready condition in the module, which the type alone cannot stop.
//   - TestReadyReasonTableIsCurrent renders Table, with each reason's verdict as the cdc's own child
//     rollup computes it, into docs/ready-reasons.md and fails while the file is stale. That file,
//     at a tag, is what a consumer diffs its classifier against.
//
// The set also lists the reasons unstructured-runtime writes on these same compositions. The cdc
// cannot construct those (their values are unexported); they are listed because a consumer reading a
// composition sees them all the same.
package readyreason

import (
	compositioncondition "github.com/krateo-platformops/composition-dynamic-controller/internal/condition"
	"github.com/krateo-platformops/unstructured-runtime/pkg/tools/unstructured/condition"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The reason literals this module owns. The runtime's are its own constants.
const (
	// AvailablePartialCoverage: available, and the rollup assessed a MINORITY of the children the
	// composition names (#297).
	ReasonAvailablePartialCoverage = "AvailablePartialCoverage"
	// ChildrenForbidden: not one child could be assessed, and at least one read was refused by RBAC
	// (#318). Matched exactly by installer's dependency gate: never rename it alone.
	ReasonChildrenForbidden = "ChildrenForbidden"
	// InventoryNotRecorded: the composition's managed-children inventory has never been recorded,
	// so there is nothing to assess.
	ReasonInventoryNotRecorded = "InventoryNotRecorded"
	// HelmOperationInProgress: written before an install or uninstall starts (#410).
	ReasonHelmOperationInProgress = "HelmOperationInProgress"
	// PendingHelmOperation: an earlier helm operation's lock is still within its grace period.
	ReasonPendingHelmOperation = "PendingHelmOperation"
)

// Writer is who writes a reason on a composition.
type Writer string

const (
	WriterCDC     Writer = "composition-dynamic-controller"
	WriterRuntime Writer = "unstructured-runtime"
)

// Reason is one reason Ready can carry, with the status it is always written with.
type Reason struct {
	reason  string
	status  metav1.ConditionStatus
	writer  Writer
	meaning string
}

// String is the reason as written on the condition.
func (r Reason) String() string { return r.reason }

// Status is the status the reason is always written with.
func (r Reason) Status() metav1.ConditionStatus { return r.status }

// Writer is who writes it.
func (r Reason) Writer() Writer { return r.writer }

// Meaning is one line on when it is written.
func (r Reason) Meaning() string { return r.meaning }

// Condition is the Ready condition carrying r, with no message.
func (r Reason) Condition() metav1.Condition {
	return metav1.Condition{
		Type:               condition.TypeReady,
		Status:             r.status,
		LastTransitionTime: metav1.Now(),
		Reason:             r.reason,
	}
}

// WithMessage is r's Ready condition with message.
func (r Reason) WithMessage(message string) metav1.Condition {
	c := r.Condition()
	c.Message = message
	return c
}

// The reasons the composition-dynamic-controller writes.
var (
	Available = Reason{condition.ReasonAvailable, metav1.ConditionTrue, WriterCDC,
		"stampReady: the health verdict is healthy, over most of the children it names"}
	AvailablePartialCoverage = Reason{ReasonAvailablePartialCoverage, metav1.ConditionTrue, WriterCDC,
		"stampReady: healthy, but the rollup assessed a minority of the children it names (#297)"}
	ReconcileGracefullyPaused = Reason{compositioncondition.ReasonReconcileGracefullyPaused, metav1.ConditionTrue, WriterCDC,
		"Observe, and setGracefullyPausedCondition: krateo.io/gracefully-paused is in effect (Synced=False is the liveness half)"}
	Creating = Reason{condition.ReasonCreating, metav1.ConditionFalse, WriterCDC,
		"stampReady: a child is converging; also stamped after Create's install (ConditionTypeCreating)"}
	Unavailable = Reason{condition.ReasonUnavailable, metav1.ConditionFalse, WriterCDC,
		"stampReady: the health verdict is failing; failObserveWithFinding: Observe failed, the message is the error"}
	HelmOperationInProgress = Reason{ReasonHelmOperationInProgress, metav1.ConditionFalse, WriterCDC,
		"recordHelmOperation, before an install or uninstall (#410); the message names the start time and the per-hook bound"}
	PendingHelmOperation = Reason{ReasonPendingHelmOperation, metav1.ConditionFalse, WriterCDC,
		"Observe: a release is pending within its grace period, so this reconcile stands down"}
	ZeroEvidence = Reason{condition.ReasonZeroEvidence, metav1.ConditionUnknown, WriterCDC,
		"stampReady: no child could be assessed at all (#165)"}
	ChildrenForbidden = Reason{ReasonChildrenForbidden, metav1.ConditionUnknown, WriterCDC,
		"stampReady: no child could be assessed, and at least one read was refused by RBAC (#318)"}
	InventoryNotRecorded = Reason{ReasonInventoryNotRecorded, metav1.ConditionUnknown, WriterCDC,
		"stampReady: the managed-children inventory was never recorded"}
)

// The reasons unstructured-runtime writes on the same compositions. Unexported: the cdc must not
// write them itself.
var (
	runtimeReadyPaused = Reason{condition.ReasonReconcilePausedReady, metav1.ConditionTrue, WriterRuntime,
		"krateo.io/paused is set (Synced=False/ReconcilePaused is the liveness half)"}
	runtimeCreating = Reason{condition.ReasonCreating, metav1.ConditionFalse, WriterRuntime,
		"worker: a Create failed or was incomplete, or writing the resource or its annotations failed (message: the error)"}
	runtimeObserveFailed = Reason{condition.ReasonObserveFailed, metav1.ConditionUnknown, WriterRuntime,
		"Observe failed and the handler did not wrap its error with ReadyAssessed"}
	runtimeDeleting = Reason{condition.ReasonDeleting, metav1.ConditionFalse, WriterRuntime,
		"Delete is in progress or failed (message: the error, on failure)"}
)

// Table is every reason a composition's Ready can carry, cdc first, in a fixed order.
func Table() []Reason {
	return []Reason{
		Available, AvailablePartialCoverage, ReconcileGracefullyPaused,
		Creating, Unavailable, HelmOperationInProgress, PendingHelmOperation,
		ZeroEvidence, ChildrenForbidden, InventoryNotRecorded,
		runtimeReadyPaused, runtimeCreating, runtimeObserveFailed, runtimeDeleting,
	}
}

// RuntimeConstructors are unstructured-runtime's own constructors for the reasons it writes, keyed by
// reason, so a test can assert each listed status against what the runtime actually builds.
func RuntimeConstructors() map[string]metav1.Condition {
	return map[string]metav1.Condition{
		condition.ReasonReconcilePausedReady: condition.ReadyPaused(),
		condition.ReasonCreating:             condition.Creating(),
		condition.ReasonObserveFailed:        condition.ObserveFailed(""),
		condition.ReasonDeleting:             condition.Deleting(),
		condition.ReasonUnavailable:          condition.Unavailable(),
		condition.ReasonAvailable:            condition.Available(),
		condition.ReasonZeroEvidence:         condition.ZeroEvidence(),
	}
}

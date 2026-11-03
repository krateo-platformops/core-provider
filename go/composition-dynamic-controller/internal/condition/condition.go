package condition

import (
	"github.com/krateo-platformops/unstructured-runtime/pkg/tools/unstructured/condition"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	ReasonReconcileGracefullyPaused = "ReconcileGracefullyPaused"
)

// ReconcileGracefullyPaused is the READINESS half of a graceful pause: Ready stays TRUE.
//
// A pause is a deliberate operator action, not a failure. Its children are still running and
// nothing has regressed, so every readiness-gating consumer — the installer's chart gate and
// operator gate, the cdc's own parent rollup, the portal's bucketing — is unaffected by someone
// pausing a composition. #241 considered flipping this to False and chose not to: readiness and
// liveness are different axes, and flipping Ready moves all four consumers at once.
func ReconcileGracefullyPaused() metav1.Condition {
	return metav1.Condition{
		Type:               condition.TypeReady,
		Status:             metav1.ConditionTrue,
		LastTransitionTime: metav1.Now(),
		Reason:             ReasonReconcileGracefullyPaused,
	}
}

// SyncedGracefullyPaused is the LIVENESS half: Synced=False, because nothing is converging this.
//
// This is what #241 was actually asking for. "Not reconciling" is not "healthy", and a `deps:` edge
// means "wait until this is converged" — but a paused composition is not converging and will not,
// by definition. A consumer that needs that distinction now has a TYPED condition to read rather
// than a reason string on Ready.
//
// The shape is deliberately the runtime's own: its annotation pause sets
// Synced=False/ReconcilePaused plus Ready=True/ReconcilePausedReady. The graceful pause set only
// the Ready half, so two pause mechanisms reported differently for no reason anyone chose. This
// makes them consistent rather than inventing a third vocabulary — no new condition TYPE, and
// nothing for a consumer to learn beyond what the annotation pause already does.
//
// REQUIRES unstructured-runtime with the handler-condition fix (core-provider#241, runtime #41).
// Before it, the runtime stamped its own ReconcileSuccess (Synced=True) over whatever the handler
// returned on the up-to-date path — which is exactly the path a graceful pause takes — so this
// condition would have been overwritten on the same reconcile and the field would have lied.
func SyncedGracefullyPaused() metav1.Condition {
	return metav1.Condition{
		Type:               condition.TypeSynced,
		Status:             metav1.ConditionFalse,
		LastTransitionTime: metav1.Now(),
		Reason:             ReasonReconcileGracefullyPaused,
	}
}

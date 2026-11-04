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
// The shape is deliberately the runtime's own: its annotation pause sets Synced=False plus
// Ready=True, and BOTH carry the emitted reason "ReconcilePaused". The graceful pause set only the
// Ready half, so two pause mechanisms reported differently for no reason anyone chose. This makes
// them consistent rather than inventing a third vocabulary — no new condition TYPE, and nothing for
// a consumer to learn beyond what the annotation pause already does.
//
// THE EMITTED REASON IS "ReconcilePaused" ON BOTH CONDITIONS, and an earlier version of this
// comment wrote the Ready half as "ReconcilePausedReady". That is the Go IDENTIFIER, not the value:
// unstructured-runtime declares
//
//	ReasonReconcilePausedReady string = "ReconcilePaused"   // written on Ready
//	ReasonReconcilePaused      string = "ReconcilePaused"   // written on Synced
//
// Two identifiers, ONE string. The old wording cost a real wrong answer: asked what a cluster
// emits, I quoted it back as "Ready=True/ReconcilePausedReady" and told the installer session that
// `True/ReconcilePaused` could not exist. They printed the object and it does — on every engine
// since runtime v1.7.3, which is older than anything deployed. A comment naming a constant where a
// reader expects a wire value reads as the value, so it must be the value.
//
// FOR A CONSUMER: the reason alone cannot tell Ready-paused from Synced-paused, because it is the
// same string on both. The condition TYPE is the only discriminator, so a gate that wants "is this
// converging" must read Synced rather than pattern-match a reason on Ready.
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

package compositiondefinitions

import (
	"context"
	"strings"
	"time"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/tools/clusterkube"
	"github.com/krateo-platformops/core-provider/internal/tools/deploy"
	"github.com/krateo-platformops/plumbing/conditions"
	rtv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// scaleToZeroOptedIn reports whether cr opted its controller into scale-to-zero (#393). It is the
// ONE place the opt-in is read, so Observe's dry-run, Create and Update cannot disagree about who
// writes the controller's replicas: if they did, Observe would see the helper's 0 as drift and
// Update would apply the chart value over it.
//
// Remote definitions never count: admission rejects the combination (core-provider#442), and a
// definition stored before that rule existed must not be treated as opted in either.
func scaleToZeroOptedIn(cr *compositiondefinitionsv1alpha1.CompositionDefinition) bool {
	if cr == nil || clusterkube.IsRemote(cr.Spec.Deploy) {
		return false
	}
	c := cr.Spec.Controller
	return c != nil && c.ScaleToZero != nil && *c.ScaleToZero
}

// controllerReadiness is what a deployed definition reports about its composition controller (#393):
// its Ready condition (owners only; a follower keeps followerCondition) and ControllerActive, the
// shared condition (plumbing conditions) for "is anything reconciling this Kind right now".
//
// The cases, from Diego's decisions of 2026-11-10/11:
//
//	replicas > 0                       Ready Available           ControllerActive True  Running
//	0, opted in (declared)             Ready True ScaledToZero    ControllerActive False ScaledToZero
//	0, NOT opted in (undeclared)       Ready False Unavailable    ControllerActive False Unavailable
//	Deployment unreadable or absent    Ready Available (as before) ControllerActive Unknown NotObserved
//
// The undeclared zero used to read Ready=True only because 0 == 0 ready replicas: nothing reconciles
// the Kind and nobody declared that, so a gate must not pass on it.
//
// Every message is stable while the state is: SetConditions rewrites a condition whose message
// changes, so no counts and no "now" go into them (Diego, 2026-11-11). The times quoted are
// lastSeen, which changes only on a transition.
func controllerReadiness(cr *compositiondefinitionsv1alpha1.CompositionDefinition, kind string, dep *appsv1.Deployment, depErr error) (ready, active rtv1.Condition) {
	activeType := rtv1.ConditionType(conditions.TypeControllerActive)
	cond := func(t rtv1.ConditionType, s metav1.ConditionStatus, reason rtv1.ConditionReason, msg string) rtv1.Condition {
		return rtv1.Condition{Type: t, Status: s, Reason: reason, Message: msg, LastTransitionTime: metav1.Now()}
	}
	if depErr != nil || dep == nil {
		why := "the controller Deployment was not found"
		if depErr != nil {
			why = "the controller Deployment could not be read: " + depErr.Error()
		}
		return rtv1.Available(), cond(activeType, metav1.ConditionUnknown, rtv1.ReasonNotObserved, why)
	}
	replicas := int32(1) // the apiserver defaults a nil to 1
	if dep.Spec.Replicas != nil {
		replicas = *dep.Spec.Replicas
	}
	st := cr.Status.ScaleToZero
	optedIn := scaleToZeroOptedIn(cr)

	if replicas == 0 {
		if optedIn {
			msg := "idle: no instances of " + kind + "; wakes on the first"
			if st != nil && st.LastSeen != nil {
				msg = "idle: no instances of " + kind + " since " + st.LastSeen.UTC().Format(time.RFC3339) + "; wakes on the first"
			}
			reason := rtv1.ConditionReason(conditions.ReasonScaledToZero)
			return cond(rtv1.TypeReady, metav1.ConditionTrue, reason, msg),
				cond(activeType, metav1.ConditionFalse, reason, msg)
		}
		msg := "the controller has 0 replicas, but this definition has not opted into scale-to-zero: nothing reconciles " + kind
		return rtv1.Unavailable().WithMessage(msg), cond(activeType, metav1.ConditionFalse, rtv1.ReasonUnavailable, msg)
	}

	msg := "controller running"
	if optedIn {
		hd, err := scaleToZeroHoldDown(cr)
		switch {
		case err != nil:
			msg = "controller running; scale-to-zero is not in effect: " + err.Error()
		case st != nil && len(st.HeldBy) > 0:
			msg = "controller running; kept running: " + strings.Join(st.HeldBy, ", ") + " not opted into scale-to-zero"
		case st != nil && st.Reason == "IdleHoldDown" && st.LastSeen != nil:
			msg = "controller running; no instances of " + kind + " since " + st.LastSeen.UTC().Format(time.RFC3339) +
				"; scales to zero at " + st.LastSeen.Add(hd).UTC().Format(time.RFC3339)
		default:
			msg = "controller running; scales to zero after " + hd.String() + " without instances"
		}
	}
	return rtv1.Available(), cond(activeType, metav1.ConditionTrue, rtv1.ConditionReason(conditions.ReasonRunning), msg)
}

// controllerConditions reads the definition's controller Deployment and returns controllerReadiness.
// ok is false for a remote definition: ControllerActive is reported for local definitions only
// (Diego, 2026-11-11), and a remote one keeps the Ready it had.
func (e *external) controllerConditions(ctx context.Context, cr *compositiondefinitionsv1alpha1.CompositionDefinition,
	gvr schema.GroupVersionResource) (ready, active rtv1.Condition, ok bool) {
	if clusterkube.IsRemote(cr.Spec.Deploy) {
		return rtv1.Condition{}, rtv1.Condition{}, false
	}
	var dep appsv1.Deployment
	err := e.kube.Get(ctx, deploy.ControllerDeploymentKey(controllerNamespace(cr), gvr), &dep)
	switch {
	case apierrors.IsNotFound(err):
		ready, active = controllerReadiness(cr, cr.Status.Kind, nil, nil)
	case err != nil:
		ready, active = controllerReadiness(cr, cr.Status.Kind, nil, err)
	default:
		ready, active = controllerReadiness(cr, cr.Status.Kind, &dep, nil)
	}
	return ready, active, true
}

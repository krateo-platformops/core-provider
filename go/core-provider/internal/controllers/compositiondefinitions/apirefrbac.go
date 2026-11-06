package compositiondefinitions

import (
	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/tools/restactionrbac"
	rtv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// conditionTypeApiRefRBACComplete says whether the read-set of a declared apiRef could be resolved
// and granted (#372). Without it a failure here is one line inside Synced=False/ReconcileError, and
// an operator cannot tell a missing RESTAction from a snowplow outage, or see that this is what is
// holding a version bump: Observe stops at this step, so a definition makes no progress at all until
// it clears.
//
// A definition with no apiRef does not carry the condition. Observe retries on the error it returns,
// so a False is never a durable mark: it clears on the first pass that resolves.
const conditionTypeApiRefRBACComplete rtv1.ConditionType = "ApiRefRBACComplete"

const reasonReadSetResolved rtv1.ConditionReason = "ReadSetResolved"

// recordApiRefRBAC records what a Deploy or Lookup learned about the apiRef's read-set.
//
// AN ERROR FROM ANYWHERE ELSE LEAVES IT ALONE. A Deploy can fail before it reaches this step, and
// that failure says nothing either way about the read-set.
func recordApiRefRBAC(cr *compositiondefinitionsv1alpha1.CompositionDefinition, err error) {
	if apiRefName(cr) == "" {
		removeCondition(cr, conditionTypeApiRefRBACComplete)
		return
	}
	if failure, ok := restactionrbac.AsError(err); ok {
		cr.SetConditions(rtv1.Condition{
			Type:               conditionTypeApiRefRBACComplete,
			Status:             metav1.ConditionFalse,
			LastTransitionTime: metav1.Now(),
			Reason:             rtv1.ConditionReason(failure.Cause),
			Message:            failure.Error(),
		})
		return
	}
	if err == nil {
		cr.SetConditions(rtv1.Condition{
			Type:               conditionTypeApiRefRBACComplete,
			Status:             metav1.ConditionTrue,
			LastTransitionTime: metav1.Now(),
			Reason:             reasonReadSetResolved,
		})
	}
}

// removeCondition drops a condition that no longer applies. A definition that loses its apiRef would
// otherwise keep reporting on a RESTAction nothing refers to.
func removeCondition(cr *compositiondefinitionsv1alpha1.CompositionDefinition, ct rtv1.ConditionType) {
	kept := cr.Status.Conditions[:0]
	for _, c := range cr.Status.Conditions {
		if c.Type != ct {
			kept = append(kept, c)
		}
	}
	cr.Status.Conditions = kept
}

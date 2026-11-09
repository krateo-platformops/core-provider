package compositiondefinitions

import (
	"testing"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/tools/deploy"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// A declared delegation the cluster's authn CRD can't carry must fail on the user's own definition
// (#405, authn's review), not as a silently pruned field on the mapping.
func TestRecordAuthnDelegation(t *testing.T) {
	declared := func() *compositiondefinitionsv1alpha1.CompositionDefinition {
		cr := &compositiondefinitionsv1alpha1.CompositionDefinition{}
		cr.Spec.ApiRef = &compositiondefinitionsv1alpha1.ApiReference{Name: "r", Namespace: "n",
			Delegation: &compositiondefinitionsv1alpha1.ApiRefDelegation{Allowed: true}}
		return cr
	}

	cr := declared()
	recordAuthnDelegation(cr, deploy.AuthnSupport{}) // authn-crds 0.36.0
	c := cr.GetCondition(conditionTypeAuthnDelegation)
	if c.Status != metav1.ConditionFalse || c.Reason != reasonDelegationUnsupported {
		t.Errorf("declared on 0.36.0: condition = %s/%s, want False/%s", c.Status, c.Reason, reasonDelegationUnsupported)
	}

	recordAuthnDelegation(cr, deploy.AuthnSupport{Delegation: true}) // after the upgrade
	if c := cr.GetCondition(conditionTypeAuthnDelegation); c.Status != metav1.ConditionTrue || c.Reason != reasonDelegationRendered {
		t.Errorf("declared on 0.37.0: condition = %s/%s, want True/%s", c.Status, c.Reason, reasonDelegationRendered)
	}

	// Not declared: no condition at all, so the other 70 definitions carry nothing new.
	cr.Spec.ApiRef.Delegation = nil
	recordAuthnDelegation(cr, deploy.AuthnSupport{})
	for _, cond := range cr.Status.Conditions {
		if cond.Type == conditionTypeAuthnDelegation {
			t.Errorf("an undeclared delegation left the %s condition behind: %+v", conditionTypeAuthnDelegation, cond)
		}
	}
}

package compositiondefinitions

import (
	"context"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/tools/deploy"
	rtv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// conditionTypeAuthnDelegation says whether a definition's declared spec.apiRef.delegation reaches
// its controller's authn mapping (#405). Present only while delegation is declared.
const conditionTypeAuthnDelegation rtv1.ConditionType = "AuthnDelegation"

const (
	// reasonDelegationUnsupported: the installed authn mapping CRD does not serve spec.delegation
	// (authn-crds < 0.37.0). Writing it anyway would be PRUNED on the full Update, so the mapping
	// would silently not delegate while this definition says it does. It is not rendered, and this
	// condition is where the user sees why.
	reasonDelegationUnsupported rtv1.ConditionReason = "DelegationUnsupported"
	// reasonDelegationRendered: the CRD serves it and it is rendered onto the mapping.
	reasonDelegationRendered rtv1.ConditionReason = "Rendered"
)

// authnSupport is which optional mapping fields the cluster's authn CRD serves, read once per
// reconcile and only for a definition that has a mapping at all (an apiRef): a definition without one
// never reads the CRD, so an unreadable authn CRD cannot fail its reconcile.
func (e *external) authnSupport(ctx context.Context, cr *compositiondefinitionsv1alpha1.CompositionDefinition) (deploy.AuthnSupport, error) {
	if apiRefName(cr) == "" {
		return deploy.AuthnSupport{}, nil
	}
	return deploy.AuthnMappingSupport(ctx, e.dynamic)
}

// recordAuthnDelegation surfaces on the definition whether its declared delegation reaches the
// mapping, so a cluster too old to carry it fails on the user's own object rather than silently.
func recordAuthnDelegation(cr *compositiondefinitionsv1alpha1.CompositionDefinition, support deploy.AuthnSupport) {
	if apiRefDelegation(cr) == nil {
		removeCondition(cr, conditionTypeAuthnDelegation)
		return
	}
	if !support.Delegation {
		cr.SetConditions(rtv1.Condition{
			Type:               conditionTypeAuthnDelegation,
			Status:             metav1.ConditionFalse,
			LastTransitionTime: metav1.Now(),
			Reason:             reasonDelegationUnsupported,
			Message: "spec.apiRef.delegation needs authn-crds >= 0.37.0 on this cluster: the installed " +
				"serviceaccounts.serviceaccount.authn.krateo.io does not serve spec.delegation, so it is not " +
				"rendered on the controller's authn mapping and the controller cannot delegate (check: " +
				"kubectl get crd serviceaccounts.serviceaccount.authn.krateo.io -o jsonpath=" +
				"'{.spec.versions[?(@.name==\"v1alpha1\")].schema.openAPIV3Schema.properties.spec.properties.delegation}' is empty)",
		})
		return
	}
	cr.SetConditions(rtv1.Condition{
		Type:               conditionTypeAuthnDelegation,
		Status:             metav1.ConditionTrue,
		LastTransitionTime: metav1.Now(),
		Reason:             reasonDelegationRendered,
	})
}

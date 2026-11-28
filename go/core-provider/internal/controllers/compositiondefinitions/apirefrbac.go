package compositiondefinitions

import (
	"context"
	"errors"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/cdreason"
	"github.com/krateo-platformops/core-provider/internal/tools/deploy"
	"github.com/krateo-platformops/core-provider/internal/tools/restactionrbac"
	rtv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// conditionTypeApiRefRBACComplete says whether the read-set of a declared apiRef could be resolved
// and granted (#372). Without it a failure here is one line inside Synced=False/ReconcileError, and
// an operator cannot tell a missing RESTAction from a snowplow outage, or see that this is what is
// holding a version bump: Observe stops at this step, so a definition makes no progress at all until
// it clears.
//
// A definition with no apiRef does not carry the condition. Observe retries on the error it returns,
// so a False is never a durable mark: it clears on the first pass that resolves.
//
// IT DOES NOT FEED READY. Nothing reads it to compute Ready (the rollup reads Ready and Synced
// only; every condition type here has the NotRead verdict). A pass that cannot resolve returns an
// error, so Ready follows from that error exactly as it did before this condition had more reasons.
const conditionTypeApiRefRBACComplete = cdreason.TypeApiRefRBACComplete

// planApiRef resolves the apiRef read-set of every composition of cd (#691) and hands it to Deploy,
// which turns it into the ClusterRole: the cluster-scoped reads and the rows no composition can place.
// The namespaced reads become Roles, applied by the Roles reconciler from the same plan.
//
// An error is a plan that could not be determined, and Deploy must not run on it: a ClusterRole
// rendered from a partial plan would drop a cluster-wide grant a composition is using. It is recorded
// on ApiRefRBACComplete here, so every caller reports it the same way. A definition without an apiRef,
// or a remote one, is left on the single ClusterRole.
func (e *external) planApiRef(ctx context.Context, cr *compositiondefinitionsv1alpha1.CompositionDefinition,
	gvr schema.GroupVersionResource, opts *deploy.DeployOptions) error {
	if !usesPerNamespaceRBAC(cr, e.remote) || e.planner == nil {
		cr.Status.ApiRefRBAC = nil
		return nil
	}
	plan, err := e.planner.plan(ctx, cr, gvr)
	if err != nil {
		recordApiRefRBAC(cr, err)
		return err
	}
	opts.ApiRefPerNamespace = true
	opts.ApiRefCompositions = plan.Compositions
	cr.Status.ApiRefRBAC = apiRefRBACStatus(plan)
	e.planner.noteObserved(client.ObjectKeyFromObject(cr), plan)
	return nil
}

func apiRefRBACStatus(plan *apiRefPlan) *compositiondefinitionsv1alpha1.ApiRefRBACStatus {
	st := &compositiondefinitionsv1alpha1.ApiRefRBACStatus{Namespaces: plan.namespaces()}
	for _, cw := range plan.clusterWide() {
		st.ClusterWide = append(st.ClusterWide, compositiondefinitionsv1alpha1.ApiRefClusterWideRow{Group: cw.Group, Resource: cw.Resource, Verb: cw.Verb})
	}
	return st
}

// recordApiRefRBAC records what a Deploy or Lookup learned about the apiRef's read-set.
//
// AN ERROR FROM ANYWHERE ELSE LEAVES IT ALONE. A Deploy can fail before it reaches this step, and
// that failure says nothing either way about the read-set.
func recordApiRefRBAC(cr *compositiondefinitionsv1alpha1.CompositionDefinition, err error) {
	if apiRefName(cr) == "" {
		removeCondition(cr, conditionTypeApiRefRBACComplete)
		cr.Status.ApiRefRBAC = nil
		return
	}
	var nd *notDetermined
	if errors.As(err, &nd) {
		// snowplow could not be asked, or the compositions could not be listed: Unknown, and no Role is
		// touched (#691). Not a failure of the read-set, which is not known.
		cr.SetConditions(cdreason.ApiRefRolesNotDetermined.WithMessage(err.Error()))
		return
	}
	if failure, ok := restactionrbac.AsError(err); ok {
		r, known := cdreason.ApiRefRBACFailure(failure.Cause)
		if !known {
			// Unreachable while TestEveryRESTActionCauseIsRegistered holds: every Cause has a row.
			// "Answered in a way nothing here recognises" is the true statement about a cause the
			// registry does not know.
			r, _ = cdreason.ApiRefRBACFailure(restactionrbac.CauseSnowplowError)
		}
		cr.SetConditions(r.WithMessage(err.Error()))
		return
	}
	if err == nil {
		if st := cr.Status.ApiRefRBAC; st != nil && len(st.ClusterWide) > 0 {
			cr.SetConditions(cdreason.ApiRefClusterWide.Condition())
			return
		}
		cr.SetConditions(cdreason.ApiRefReadSetResolved.Condition())
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

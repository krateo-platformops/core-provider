package compositiondefinitions

import (
	"context"
	"fmt"
	"strings"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/cdreason"
	"github.com/krateo-platformops/core-provider/internal/tools/deploy"
	"github.com/krateo-platformops/plumbing/managementpolicy"
	"github.com/krateo-platformops/provider-runtime/pkg/meta"
)

// ensureSelfAuthnMapping makes core-provider's own authn mapping exist on the management cluster, for a
// definition with an apiRef. Every caller of deploy.Deploy runs it first, Observe's dry run included:
// Deploy cannot fetch the apiRef's read-set without it (deploy.EnsureSelfAuthnMapping, #642).
//
// e.mgmtDynamic and not e.dynamic: for a remote definition Connect swaps the latter to the target, and
// core-provider exchanges its token with the authn it runs beside.
func (e *external) ensureSelfAuthnMapping(ctx context.Context, cr *compositiondefinitionsv1alpha1.CompositionDefinition, mayWrite bool) mappingRead {
	if apiRefName(cr) == "" {
		return mappingRead{}
	}
	state, err := deploy.EnsureSelfAuthnMapping(ctx, e.mgmtDynamic, deploy.SelfAuthnMapping{
		SAName:         SelfSAName,
		SANamespace:    SelfSANamespace,
		Group:          SelfGroup,
		AuthnNamespace: AuthnNamespace,
		RBACFolderPath: CDCrbacConfigFolder,
	}, mayWrite)
	if err != nil {
		err = fmt.Errorf("ensuring core-provider's own authn mapping: %w", err)
	}
	return mappingRead{state: state, err: err}
}

// observeMayWriteSelfMapping reports whether Observe may write core-provider's own mapping for cr. The
// runtime decides whether to call Create or Update, but it calls Observe whatever the policy says
// (observe-only included), so Observe asks for itself:
//   - a definition whose krateo.io/management-policy withholds update (observe-only, or a value that
//     does not parse) writes nothing: past this point Update is the action Observe stands in for;
//   - a paused definition writes nothing. The runtime skips it before Observe, but on the cached copy,
//     and Observe re-reads the definition, so the pause can be seen first here;
//   - nor does one being deleted, whose teardown does not need the mapping.
func observeMayWriteSelfMapping(cr *compositiondefinitionsv1alpha1.CompositionDefinition) bool {
	return !meta.IsPaused(cr) && !meta.WasDeleted(cr) && meta.ManagementDecision(cr).May(managementpolicy.Update)
}

// mappingRead is one live read of an authn mapping: its state, or why it could not be read.
type mappingRead struct {
	state deploy.MappingState
	err   error
}

// authorizes: the mapping is there, or there is none to be.
func (r mappingRead) authorizes() bool {
	return r.err == nil && (r.state == deploy.MappingPresent || r.state == deploy.MappingNotNeeded)
}

func (r mappingRead) notInstalled() bool {
	return r.err == nil && r.state == deploy.MappingNotInstalled
}

// recordLiveAuthnMappings reads the mapping of the controller opts deploys, and records it on the
// definition together with core-provider's own (self).
func recordLiveAuthnMappings(ctx context.Context, cr *compositiondefinitionsv1alpha1.CompositionDefinition, opts deploy.DeployOptions, self mappingRead) mappingRead {
	state, err := deploy.ControllerAuthnMapping(ctx, opts)
	controller := mappingRead{state: state, err: err}
	recordAuthnMappingApplied(cr, controller, self)
	return controller
}

// conditionTypeAuthnMappingApplied says whether the two authn mappings a definition with an apiRef
// needs are in place (#251 item 2, Diego 2026-11-14; computed from the live mappings since #642): its
// controller's, where the controller runs, and core-provider's own, on the management cluster. Present
// only while the definition has an apiRef. Ready does not depend on it: a target without authn still
// runs the controller, and its compositions report the consequence themselves
// (ApiResolved=False/AuthnUnreachable).
const conditionTypeAuthnMappingApplied = cdreason.TypeAuthnMappingApplied

// recordAuthnMappingApplied sets AuthnMappingApplied from what was read of the two mappings:
//
//   - True/Applied only when both were read and both authorize;
//   - False/AuthnNotInstalled when a cluster a mapping belongs on does not serve the kind, naming the
//     cluster and what waits on it. It stands whatever the other read said: it is known;
//   - no condition otherwise: a mapping that could not be read, or one that is absent while the kind is
//     served. Neither is Applied, and neither is authn not being installed, the only two reasons this
//     type has. Both are transient where core-provider may act: an unreadable mapping fails the pass
//     and is retried, an absent one is written (core-provider's own in the same pass, the controller's
//     by the Update the pass asks for). Removing rather than keeping the last one: a kept True would
//     be a claim no read of this pass supports.
func recordAuthnMappingApplied(cr *compositiondefinitionsv1alpha1.CompositionDefinition, controller, self mappingRead) {
	if apiRefName(cr) == "" {
		removeCondition(cr, conditionTypeAuthnMappingApplied)
		return
	}
	const kind = "serviceaccounts.serviceaccount.authn.krateo.io"
	var missing []string
	if controller.notInstalled() {
		missing = append(missing, deployTargetDescription(cr)+" serves no "+kind+", "+
			"so this definition's controller has no authn mapping there and cannot obtain service tokens: "+
			"its compositions report ApiResolved=False/AuthnUnreachable until authn is installed on that cluster")
	}
	if self.notInstalled() {
		missing = append(missing, "the management cluster serves no "+kind+", "+
			"so core-provider has no authn mapping of its own and cannot resolve this definition's apiRef: "+
			"it is waiting for authn to be installed there, and looks again on every reconcile")
	}
	switch {
	case len(missing) > 0:
		cr.SetConditions(cdreason.AuthnNotInstalled.WithMessage(strings.Join(missing, "; ")))
	case controller.authorizes() && self.authorizes():
		cr.SetConditions(cdreason.AuthnMappingApplied.Condition())
	default:
		removeCondition(cr, conditionTypeAuthnMappingApplied)
	}
}

// deployTargetDescription names the cluster a definition's controller runs in, for messages.
func deployTargetDescription(cr *compositiondefinitionsv1alpha1.CompositionDefinition) string {
	if d := cr.Spec.Deploy; d != nil && d.TargetRef != nil && d.TargetRef.Name != "" {
		return "KubernetesTarget " + cr.Namespace + "/" + d.TargetRef.Name
	}
	return "the management cluster"
}

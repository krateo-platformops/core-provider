package compositiondefinitions

import (
	"context"
	"errors"
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

// errMappingNotRead is the controller's mapping on a pass that stops before reading it, because
// core-provider's own could not be ensured: unknown, and the failure is not its own.
var errMappingNotRead = errors.New("the pass stopped before reading it")

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
//   - False/MappingMissing when the kind is served and a mapping is absent or does not authorize. Also
//     known, so it stands beside a read that failed;
//   - Unknown/NotObserved otherwise: one of them could not be read, and neither is known to be missing.
//
// Never the last one kept: a True would be a claim no read of this pass supports. And never absent, which
// says the definition has no apiRef.
func recordAuthnMappingApplied(cr *compositiondefinitionsv1alpha1.CompositionDefinition, controller, self mappingRead) {
	if apiRefName(cr) == "" {
		removeCondition(cr, conditionTypeAuthnMappingApplied)
		return
	}
	const kind = "serviceaccounts.serviceaccount.authn.krateo.io"
	var uninstalled, absent, unread []string
	if controller.notInstalled() {
		uninstalled = append(uninstalled, deployTargetDescription(cr)+" serves no "+kind+", "+
			"so this definition's controller has no authn mapping there and cannot obtain service tokens: "+
			"its compositions report ApiResolved=False/AuthnUnreachable until authn is installed on that cluster")
	}
	if self.notInstalled() {
		uninstalled = append(uninstalled, "the management cluster serves no "+kind+", "+
			"so core-provider has no authn mapping of its own and cannot resolve this definition's apiRef: "+
			"it is waiting for authn to be installed there, and looks again on every reconcile")
	}
	for _, m := range []struct {
		read mappingRead
		what string
	}{
		{controller, "the authn mapping of this definition's controller, on " + deployTargetDescription(cr)},
		{self, "core-provider's own authn mapping, on the management cluster"},
	} {
		switch {
		case m.read.err != nil:
			unread = append(unread, m.what+", could not be read: "+m.read.err.Error())
		case m.read.state == deploy.MappingMissing:
			absent = append(absent, m.what+", is absent or does not authorize its ServiceAccount")
		}
	}
	switch {
	case len(uninstalled) > 0:
		cr.SetConditions(cdreason.AuthnNotInstalled.WithMessage(strings.Join(uninstalled, "; ")))
	case len(absent) > 0:
		cr.SetConditions(cdreason.AuthnMappingMissing.WithMessage(strings.Join(absent, "; ")))
	case len(unread) > 0:
		cr.SetConditions(cdreason.AuthnMappingNotObserved.WithMessage(strings.Join(unread, "; ")))
	case controller.authorizes() && self.authorizes():
		cr.SetConditions(cdreason.AuthnMappingApplied.Condition())
	default: // a state this does not know: never Applied
		cr.SetConditions(cdreason.AuthnMappingNotObserved.WithMessage("the mappings were read in a state that is not known"))
	}
}

// deployTargetDescription names the cluster a definition's controller runs in, for messages.
func deployTargetDescription(cr *compositiondefinitionsv1alpha1.CompositionDefinition) string {
	if d := cr.Spec.Deploy; d != nil && d.TargetRef != nil && d.TargetRef.Name != "" {
		return "KubernetesTarget " + cr.Namespace + "/" + d.TargetRef.Name
	}
	return "the management cluster"
}

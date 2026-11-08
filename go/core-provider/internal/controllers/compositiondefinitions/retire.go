package compositiondefinitions

import (
	"context"
	"errors"
	"fmt"
	"sort"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/tools/clusterkube"
	"github.com/krateo-platformops/core-provider/internal/tools/deploy"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery/cached/memory"
)

// retireController removes the per-version controller for `retired` and then moves every derived
// binding that names its ServiceAccount onto the current controller's (`current`, running in
// currentNamespace).
//
// It is the ONE retirement sequence. The version-change path and the leaked-controller reclaim
// (#327) both call it, so neither can delete an identity without moving the bindings that name it.
// Those bindings are release-scoped and survive a bump; the identity is version-scoped and does
// not. Leaving them to each composition's next Observe cannot repair a composition whose CR is not
// present to be observed (#166).
//
// spec is the retired version's chart when it is known. Undeploy reads it only for the credentials
// Role and RoleBinding in the passwordRef namespace, and their names are version-scoped. So the
// definition's current chart, passed for a version whose own chart is no longer recorded, can at
// worst miss that pair. It can never reach the current version's.
func (e *external) retireController(ctx context.Context, cr *compositiondefinitionsv1alpha1.CompositionDefinition,
	spec *compositiondefinitionsv1alpha1.ChartInfo, retired, current schema.GroupVersionResource, currentNamespace string,
	log logging.Logger) error {
	// The retired version's controller may live in two places (#313: an old version is not relocated,
	// so it may still be in the definition's namespace). Both are retired here, in this one routine,
	// and the re-subject below runs after both (#166).
	err := deploy.Undeploy(ctx, e.kube, e.retiredUndeployOptions(cr, spec, retired, controllerNamespace(cr)))
	if err != nil {
		return fmt.Errorf("error undeploying older version of dynamic controller: %w", err)
	}
	if legacy := legacyControllerNamespace(cr); legacy != "" {
		if err := deploy.Undeploy(ctx, e.kube, e.retiredUndeployOptions(cr, spec, retired, legacy)); err != nil {
			return fmt.Errorf("error undeploying older version of dynamic controller from %s: %w", legacy, err)
		}
	}
	log.Debug("Undeployed older versions of dynamic controller", "gvr", retired.String(), "placements", controllerPlacements(cr))

	repaired, err := deploy.ResubjectDerivedRBAC(ctx, deploy.ResubjectOptions{
		DynamicClient: e.dynamic,
		Namespace:     cr.Namespace,
		NewNamespace:  currentNamespace,
		OldGVR:        retired,
		NewGVR:        current,
	})
	if err != nil {
		return fmt.Errorf("error re-subjecting derived RBAC from %s to %s: %w", retired.Version, current.Version, err)
	}
	if repaired > 0 {
		log.Debug("Re-subjected derived RBAC onto the current controller ServiceAccount",
			"count", repaired, "from", retired.String(), "to", current.String())
	}
	return nil
}

// currentControllerNamespace is where this definition's current-version controller runs: the
// owner's namespace when this definition follows another (controllerowner.go), its own otherwise.
func currentControllerNamespace(cr *compositiondefinitionsv1alpha1.CompositionDefinition, owner *compositiondefinitionsv1alpha1.CompositionDefinition) string {
	if owner != nil {
		return controllerNamespace(owner)
	}
	return controllerNamespace(cr)
}

// reclaimLeakedControllers retires, through retire, every controller findLeakedControllers reports
// (#327). It decides nothing of its own: the predicate's refusals are the whole safety argument, so
// a refusal, or an error establishing it, retires nothing.
//
// One failed retirement does not stop the others. Each leaked controller is independent debris,
// and stopping at the first would leave the rest behind the one that cannot go.
func (e *external) reclaimLeakedControllers(ctx context.Context, gvk schema.GroupVersionKind, gvr schema.GroupVersionResource,
	selfName, selfNamespace string, placements []string, retire func(context.Context, schema.GroupVersionResource) error,
) (reclaimed []string, failed map[string]error, kept []string, err error) {
	leaked, kept, err := e.findLeakedControllers(ctx, gvk, gvr, selfName, selfNamespace, placements)
	if err != nil {
		return nil, nil, nil, err
	}
	failed = map[string]error{}
	for _, l := range leaked {
		retired := schema.GroupVersionResource{Group: gvr.Group, Version: l.Version, Resource: gvr.Resource}
		if rerr := retire(ctx, retired); rerr != nil {
			failed[l.Name] = rerr
			continue
		}
		reclaimed = append(reclaimed, l.Name)
	}
	sort.Strings(reclaimed)
	return reclaimed, failed, kept, nil
}

// controllerNamespace is where cr's version controllers live (#313). Every DeployOptions and
// UndeployOptions in this package takes its ControllerNamespace from here, so the move to the
// management namespace is a change to this function (plus the migration), not to each call site.
//
// LOCAL definitions: the management namespace, core-provider's own, so the controller outlives
// every namespace whose compositions it finalizes. REMOTE definitions: unchanged here, the
// definition's namespace on the spoke. KubernetesTarget.spec.controllerNamespace is step 3.
func controllerNamespace(cr *compositiondefinitionsv1alpha1.CompositionDefinition) string {
	if clusterkube.IsRemote(cr.Spec.Deploy) {
		return cr.Namespace
	}
	return ManagementNamespace
}

// errNoManagementNamespace stops core-provider at startup when CDC_NAMESPACE is unset (see
// ManagementNamespace).
var errNoManagementNamespace = errors.New("CDC_NAMESPACE is not set: core-provider needs its own namespace to place composition controllers (#313)")

// retiredUndeployOptions remove a retired version's controller from one placement. Absence is not an
// error, so they are safe for a placement that never held it.
func (e *external) retiredUndeployOptions(cr *compositiondefinitionsv1alpha1.CompositionDefinition,
	spec *compositiondefinitionsv1alpha1.ChartInfo, retired schema.GroupVersionResource, controllerNS string) deploy.UndeployOptions {
	return deploy.UndeployOptions{
		DiscoveryClient:        memory.NewMemCacheClient(e.client.Discovery()),
		RBACFolderPath:         CDCrbacConfigFolder,
		DeploymentTemplatePath: CDCtemplateDeploymentPath,
		ConfigmapTemplatePath:  CDCtemplateConfigmapPath,
		JsonSchemaTemplatePath: JSONSchemaTemplateConfigmapPath,
		ServiceTemplatePath:    ServiceTemplatePath,
		DynamicClient:          e.dynamic,
		Spec:                   spec,
		GVR:                    retired,
		KubeClient:             e.kube,
		Namespace:              cr.Namespace,
		ControllerNamespace:    controllerNS,
		SkipCRD:                true,
		AuthnNamespace:         AuthnNamespace,
	}
}

// controllerPlacements is every namespace one of cr's controllers may live in: its controller
// namespace and, for a local definition outside the management namespace, the definition's own
// namespace. A controller kept for an older version is NOT relocated (Diego, 2026-11-08: nothing
// redeploys an old version), so retiring or reclaiming one must look in both, or it would leak.
func controllerPlacements(cr *compositiondefinitionsv1alpha1.CompositionDefinition) []string {
	to := controllerNamespace(cr)
	if cr.Namespace == to {
		return []string{to}
	}
	return []string{to, cr.Namespace}
}

// legacyControllerNamespace is the definition's own namespace when cr's controllers may still live
// there (#313), and "" otherwise.
func legacyControllerNamespace(cr *compositiondefinitionsv1alpha1.CompositionDefinition) string {
	if p := controllerPlacements(cr); len(p) == 2 {
		return p[1]
	}
	return ""
}

// relocateLegacyController moves cr's CURRENT-version controller from the definition's namespace to
// its controller namespace (#313), STOP-BEFORE-START: the cdc has no leader election, so two copies
// would both reconcile. Undeploy(Relocating) deletes the old placement and waits for its pods to
// drain (#263) before returning; only then does the caller's Deploy create the new one. If a step
// fails, the next pass resumes from what exists: a remaining old Deployment is removed first again,
// and the new one is deployed only after it is gone.
//
// A no-op for a remote definition (not moved until step 3), for a definition already in the
// management namespace, and when no old placement exists.
func (e *external) relocateLegacyController(ctx context.Context, cr *compositiondefinitionsv1alpha1.CompositionDefinition,
	gvr schema.GroupVersionResource, log logging.Logger) error {
	from := legacyControllerNamespace(cr)
	if from == "" {
		return nil
	}
	var dep appsv1.Deployment
	err := e.kube.Get(ctx, deploy.ControllerDeploymentKey(from, gvr), &dep)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("checking for a controller to relocate from %s: %w", from, err)
	}
	log.Info("Relocating the composition controller to the management namespace (#313)",
		"gvr", gvr.String(), "from", from, "to", controllerNamespace(cr))
	opts := e.retiredUndeployOptions(cr, cr.Spec.Chart, gvr, from)
	opts.Relocating = true
	if err := deploy.Undeploy(ctx, e.kube, opts); err != nil {
		return fmt.Errorf("relocating the composition controller from %s: %w", from, err)
	}
	return nil
}

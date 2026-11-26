package compositiondefinitions

import (
	"context"
	"slices"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/tools/clusterkube"
	"github.com/krateo-platformops/core-provider/internal/tools/deploy"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// poolOf is the pool a definition names, or "" for a dedicated controller.
func poolOf(cd *compositiondefinitionsv1alpha1.CompositionDefinition) string {
	if cd.Spec.Controller == nil || cd.Spec.Controller.Pool == nil {
		return ""
	}
	return *cd.Spec.Controller.Pool
}

// poolOptions are the pools of the namespace this definition's controller runs in, or nil for a remote
// definition, whose controller runs on its spoke: a pool is local only (docs/design/cdc-pooling.md §5c).
func (e *external) poolOptions(cr *compositiondefinitionsv1alpha1.CompositionDefinition) *deploy.PoolOptions {
	if clusterkube.IsRemote(cr.Spec.Deploy) {
		return nil
	}
	return &deploy.PoolOptions{
		Namespace: controllerNamespace(cr), DynClient: e.mgmtDynamic, KubeClient: e.mgmt, LeaseTimings: e.leaseTimings,
		Assets: deploy.PoolAssets{
			SizingPath: PoolSizingPath, ConfigmapTemplatePath: PoolConfigmapTemplatePath,
			DeploymentTemplatePath: PoolDeploymentTemplatePath, AuthnURL: AuthnURL,
		},
	}
}

// poolsOfKind are the pools that run a controller of cr's Kind now, for reconcilePools once it has left
// one of them.
func (e *external) poolsOfKind(ctx context.Context, cr *compositiondefinitionsv1alpha1.CompositionDefinition, gvr schema.GroupVersionResource) (map[string]bool, error) {
	opts := e.poolOptions(cr)
	if opts == nil {
		return nil, nil
	}
	return deploy.PoolsOfKind(ctx, opts.DynClient, opts.Namespace, gvr.Group, gvr.Resource)
}

// reconcilePools is the last step of a local definition's Create, Update and Delete (docs/design/
// cdc-pooling.md §5c, Lifecycle): the pool it names, and those in were (poolsOfKind, before this pass
// moved anything), are brought to their members, and a pool with none left is deleted by this last
// leaver. Membership itself is not decided here: it is the live env ConfigMaps (deploy, pool_index.go),
// so a controller that joined or left during this pass is already in, or out of, its index.
//
// "Could not determine" is never "empty": a failed list returns an error and changes nothing, so a
// transient read failure cannot delete a pool that still has members (the rule every teardown path here
// follows).
func (e *external) reconcilePools(ctx context.Context, cr *compositiondefinitionsv1alpha1.CompositionDefinition, were map[string]bool) error {
	opts := e.poolOptions(cr)
	if opts == nil {
		return nil
	}
	touched := map[string]bool{}
	for p := range were {
		touched[p] = true
	}
	if p := poolOf(cr); p != "" && cr.GetDeletionTimestamp() == nil {
		touched[p] = true
	}
	return deploy.ReconcilePools(ctx, *opts, touched)
}

// runsInPool reports whether a pool runs the controller for gvr in namespace, or is still draining it.
func (e *external) runsInPool(ctx context.Context, namespace string, gvr schema.GroupVersionResource) (bool, error) {
	versions, err := deploy.PoolMemberVersions(ctx, e.mgmtDynamic, namespace, gvr.Group, gvr.Resource)
	if err != nil {
		return false, err
	}
	return slices.Contains(versions, gvr.Version), nil
}

// controllerPlacement is where the controller for gvr runs, decided by runner: the definition that
// owns the Kind x version (cr itself, or a follower's owner). A pool member runs in its pool's
// Deployment; anything else, including every remote definition (a remote pool is refused at
// admission), in <resource>-<version>-controller. Recorded as status.controller (#580) and read by
// controllerConditions, so the two cannot disagree about which Deployment is the controller.
func controllerPlacement(runner *compositiondefinitionsv1alpha1.CompositionDefinition, gvr schema.GroupVersionResource) compositiondefinitionsv1alpha1.ControllerStatus {
	ns := controllerNamespace(runner)
	if p := poolOf(runner); p != "" && !clusterkube.IsRemote(runner.Spec.Deploy) {
		return compositiondefinitionsv1alpha1.ControllerStatus{
			Mode: compositiondefinitionsv1alpha1.ControllerModePool, Namespace: ns,
			Deployment: deploy.PoolNames(p).Workload, Pool: p,
		}
	}
	return compositiondefinitionsv1alpha1.ControllerStatus{
		Mode: compositiondefinitionsv1alpha1.ControllerModeDedicated, Namespace: ns,
		Deployment: deploy.ControllerDeploymentKey(ns, gvr).Name,
	}
}

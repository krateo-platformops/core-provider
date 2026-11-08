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
	err := deploy.Undeploy(ctx, e.kube, deploy.UndeployOptions{
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
		ControllerNamespace:    controllerNamespace(cr),
		SkipCRD:                true,
		AuthnNamespace:         AuthnNamespace,
	})
	if err != nil {
		return fmt.Errorf("error undeploying older version of dynamic controller: %w", err)
	}
	log.Debug("Undeployed older versions of dynamic controller", "gvr", retired.String())

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
	selfName, selfNamespace string, retire func(context.Context, schema.GroupVersionResource) error,
) (reclaimed []string, failed map[string]error, kept []string, err error) {
	leaked, kept, err := e.findLeakedControllers(ctx, gvk, gvr, selfName, selfNamespace)
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

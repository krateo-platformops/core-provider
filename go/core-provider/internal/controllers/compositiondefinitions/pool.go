package compositiondefinitions

import (
	"context"
	"fmt"
	"sort"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/tools/clusterkube"
	"github.com/krateo-platformops/core-provider/internal/tools/deploy"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// poolOf is the pool a definition names, or "" for a dedicated controller.
func poolOf(cd *compositiondefinitionsv1alpha1.CompositionDefinition) string {
	if cd.Spec.Controller == nil || cd.Spec.Controller.Pool == nil {
		return ""
	}
	return *cd.Spec.Controller.Pool
}

// poolMembers is pool p's membership (docs/design/cdc-pooling.md §5c), a pure function of the
// definitions, recomputed on every reconcile of one of them, so there is no count to drift.
//
// A member is a Kind x version CONTROLLER, not a definition. Two definitions on one Kind x version
// share one controller, run by its owner (electControllerOwner); a follower runs none. So a controller
// is in p exactly when its OWNER names p, and a follower's own pool field changes nothing. Live local
// definitions only (a remote pool is refused at admission), with a served version in status: a
// definition that has not reconciled yet has no controller to pool.
func poolMembers(pool string, all []compositiondefinitionsv1alpha1.CompositionDefinition) []schema.GroupVersionResource {
	seen := map[schema.GroupVersionResource]bool{}
	var out []schema.GroupVersionResource
	for i := range all {
		cd := &all[i]
		if poolOf(cd) != pool || cd.GetDeletionTimestamp() != nil || clusterkube.IsRemote(cd.Spec.Deploy) {
			continue
		}
		gvr, ok := statusGVR(cd)
		if !ok || seen[gvr] {
			continue
		}
		gvk := cd.Status.CurrentGVK()
		chart := chartRefOf(cd.Spec.Chart)
		var sameVersion []compositiondefinitionsv1alpha1.CompositionDefinition
		for j := range all {
			if targetsVersion(&all[j], gvk, chart) {
				sameVersion = append(sameVersion, all[j])
			}
		}
		if electControllerOwner(cd, sameVersion) != nil {
			continue // a follower: its owner decides where the controller runs
		}
		seen[gvr] = true
		out = append(out, gvr)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].String() < out[b].String() })
	return out
}

var configMapsGVR = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}

// reconcilePools brings every pool this definition's controller touches to its membership (docs/
// design/cdc-pooling.md §5c, "Lifecycle, ref-counted by recomputation"): the pool it names now, and
// any pool whose index still lists its controller (so leaving one, or moving between two, removes it).
// Each is recomputed from the live definitions: ensured with its members, or, when none is left,
// deleted by this last leaver.
//
// "Could not determine" is never "empty": a failed list, or a member whose env ConfigMap cannot be
// read, returns an error and changes nothing, so a transient read failure cannot delete a pool that
// still has members (the rule every teardown path here follows).
func (e *external) reconcilePools(ctx context.Context, cr *compositiondefinitionsv1alpha1.CompositionDefinition, gvr schema.GroupVersionResource) error {
	ns := controllerNamespace(cr)
	pools := map[string]bool{}
	if p := poolOf(cr); p != "" {
		pools[p] = true
	}
	indexes, err := e.mgmtDynamic.Resource(configMapsGVR).Namespace(ns).List(ctx, metav1.ListOptions{LabelSelector: deploy.PoolLabel})
	if err != nil {
		return fmt.Errorf("listing pool indexes in %s: %w", ns, err)
	}
	key := deploy.MemberKey(gvr)
	for _, idx := range indexes.Items {
		p := idx.GetLabels()[deploy.PoolLabel]
		if p == "" || idx.GetName() != deploy.PoolNames(p).Index {
			continue // the pool's other labelled objects, not its index
		}
		if data, _, _ := unstructured.NestedStringMap(idx.Object, "data"); data[key] != "" {
			pools[p] = true
		}
	}
	if len(pools) == 0 {
		return nil
	}

	all := &compositiondefinitionsv1alpha1.CompositionDefinitionList{}
	if err := e.kube.List(ctx, all); err != nil {
		return fmt.Errorf("listing CompositionDefinitions to recompute pool membership: %w", err)
	}
	names := make([]string, 0, len(pools))
	for p := range pools {
		names = append(names, p)
	}
	sort.Strings(names)
	for _, p := range names {
		members := poolMembers(p, all.Items)
		if len(members) == 0 {
			if err := deploy.DeletePool(ctx, e.mgmtDynamic, p, ns); err != nil {
				return err
			}
			continue
		}
		rendered := make([]deploy.PoolMember, 0, len(members))
		for _, m := range members {
			cm, err := e.mgmtDynamic.Resource(configMapsGVR).Namespace(ns).Get(ctx, deploy.MemberEnvConfigMapName(m), metav1.GetOptions{})
			if err != nil {
				// Including NotFound: a member whose env ConfigMap is not there yet has not finished its
				// own deploy, and indexing it without one would start nothing. Retried on the next pass.
				return fmt.Errorf("pool %s: reading member %s's env ConfigMap: %w", p, m, err)
			}
			data, _, _ := unstructured.NestedStringMap(cm.Object, "data")
			rendered = append(rendered, deploy.PoolMember{GVR: m, EnvConfig: data})
		}
		sizing, err := deploy.LoadPoolSizing(PoolSizingPath, p)
		if err != nil {
			return err
		}
		if err := deploy.EnsurePool(ctx, deploy.PoolOptions{
			Pool: p, Namespace: ns, Members: rendered, Sizing: sizing,
			ConfigmapTemplatePath: PoolConfigmapTemplatePath, DeploymentTemplatePath: PoolDeploymentTemplatePath,
			AuthnURL: AuthnURL, DynClient: e.mgmtDynamic,
		}); err != nil {
			return err
		}
	}
	return nil
}

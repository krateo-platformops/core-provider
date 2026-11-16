package compositiondefinitions

import (
	"sort"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/tools/clusterkube"
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

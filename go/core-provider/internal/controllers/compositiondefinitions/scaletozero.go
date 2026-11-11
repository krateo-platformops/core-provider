package compositiondefinitions

import (
	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/tools/clusterkube"
)

// scaleToZeroOptedIn reports whether cr opted its controller into scale-to-zero (#393). It is the
// ONE place the opt-in is read, so Observe's dry-run, Create and Update cannot disagree about who
// writes the controller's replicas: if they did, Observe would see the helper's 0 as drift and
// Update would apply the chart value over it.
//
// Remote definitions never count: admission rejects the combination (core-provider#442), and a
// definition stored before that rule existed must not be treated as opted in either.
func scaleToZeroOptedIn(cr *compositiondefinitionsv1alpha1.CompositionDefinition) bool {
	if cr == nil || clusterkube.IsRemote(cr.Spec.Deploy) {
		return false
	}
	c := cr.Spec.Controller
	return c != nil && c.ScaleToZero != nil && *c.ScaleToZero
}

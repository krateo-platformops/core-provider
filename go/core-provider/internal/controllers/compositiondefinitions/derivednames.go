package compositiondefinitions

import (
	"context"
	"errors"
	"fmt"
	"strings"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/cdreason"
	"github.com/krateo-platformops/core-provider/internal/tools/deploy"
	"github.com/krateo-platformops/provider-runtime/pkg/reconciler"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// derivedNameRefusal is #588's check in Observe, before anything is written: when a name derived
// from gvr would be refused by the API server, or silently truncated by the controller templates
// (deploy.DerivedNameViolations), the definition is Ready=False/DerivedNameTooLong naming each name,
// its length and the problem, and a ReadyAssessed error stops Create and Update. So no CRD is
// written for a Kind whose controller cannot exist. A definition that worked before keeps what it
// has; nothing is deleted.
//
// The dedicated-controller names apply when the definition that RUNS the controller is dedicated:
// cr itself, or, for a follower, its owner (as controllerPlacement is read on the follower path).
func (e *external) derivedNameRefusal(ctx context.Context, cr *compositiondefinitionsv1alpha1.CompositionDefinition, gvk schema.GroupVersionKind, gvr schema.GroupVersionResource) (obs reconciler.ExternalObservation, err error, stop bool) {
	runner := cr
	owner, err := e.controllerOwner(ctx, cr, gvk)
	if err != nil {
		return reconciler.ExternalObservation{}, err, true
	}
	if owner != nil {
		runner = owner
	}
	dedicated := controllerPlacement(runner, gvr).Mode == compositiondefinitionsv1alpha1.ControllerModeDedicated
	violations, err := deploy.DerivedNameViolations(gvr, dedicated, ServiceTemplatePath)
	if err != nil {
		return reconciler.ExternalObservation{}, err, true
	}
	if len(violations) == 0 {
		return reconciler.ExternalObservation{}, nil, false
	}
	parts := make([]string, 0, len(violations))
	for _, v := range violations {
		parts = append(parts, v.String())
	}
	msg := fmt.Sprintf("the names core-provider derives for %s from the chart's name and Chart.yaml version do not fit Kubernetes' limits, so nothing is applied; shorten the chart name or version: %s",
		gvr.String(), strings.Join(parts, "; "))
	cr.SetConditions(cdreason.DerivedNameTooLong.WithMessage(msg))
	e.recordObservedGeneration(cr) // #644: the names come from this spec's chart
	return reconciler.ExternalObservation{}, reconciler.ReadyAssessed(errors.New(msg)), true
}

package composition

import (
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/krateo-platformops/composition-dynamic-controller/internal/chartinspector"
	unstructuredtools "github.com/krateo-platformops/unstructured-runtime/pkg/tools/unstructured"
)

// ConditionTypeChartRenderable reports whether chart-inspector could render the blueprint against
// THIS cluster. It exists because a blueprint referencing an API kind the cluster does not serve
// was indistinguishable from chart-inspector being broken: every failure arrived as
// 500/InternalError folded into Synced's message (core-provider#265).
//
// The precedent is ConditionTypeApiResolved (#251): an unresolvable apiRef used to be silent, and
// naming it as its own condition with its own reasons made it both visible and alertable. "This
// blueprint needs a CRD this cluster does not serve" earns the same treatment, and only becomes
// possible now that chart-inspector answers 422 for it.
const ConditionTypeChartRenderable = "ChartRenderable"

const (
	// reasonChartRenderable: the render succeeded.
	reasonChartRenderable = "Renderable"
	// reasonUnservedKind: chart-inspector answered 422. The blueprint is well-formed and references
	// a kind this cluster does not currently serve.
	reasonUnservedKind = "UnservedKind"
	// reasonInspectorUnavailable: chart-inspector itself failed (5xx or transport). NOT the
	// blueprint's fault, and the distinction is the entire point of this condition.
	reasonInspectorUnavailable = "InspectorUnavailable"
)

// chartRenderableFailure classifies a chart-inspector failure into a reason and message.
//
// Classified by errors.As on a typed status, never by matching message text. helm's wording is
// not ours to depend on and has changed under us before.
func chartRenderableFailure(err error) (reason, message string) {
	if se, ok := chartinspector.AsStatusError(err); ok {
		switch {
		case se.Unprocessable():
			return reasonUnservedKind, fmt.Sprintf(
				"the chart references an API kind this cluster does not serve; install the missing "+
					"CRD and this resolves without further action (chart-inspector HTTP %d)", se.Code)
		case se.ServerSide():
			return reasonInspectorUnavailable, fmt.Sprintf(
				"chart-inspector failed to render the chart (HTTP %d): %s", se.Code, se.Body)
		default:
			return reasonInspectorUnavailable, fmt.Sprintf(
				"chart-inspector refused the request (HTTP %d): %s", se.Code, se.Body)
		}
	}
	// No status at all: the request never got an answer.
	return reasonInspectorUnavailable, fmt.Sprintf("could not reach chart-inspector: %v", err)
}

// setChartRenderableCondition stamps the condition.
//
// RECOMPUTED EVERY RECONCILE AND NEVER LATCHED. A 422 means "not renderable as things stand", and
// the window between a CRD being created and chart-inspector's RESTMapper seeing it is real — so a
// composition that hit that window must recover on the next reconcile rather than carry
// UnservedKind until someone intervenes. Latching here would reproduce #261 exactly: a condition
// derived once from state nobody refreshes.
func setChartRenderableCondition(mg *unstructured.Unstructured, status metav1.ConditionStatus, reason, message string) error {
	return unstructuredtools.SetConditions(mg, metav1.Condition{
		Type:               ConditionTypeChartRenderable,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.NewTime(time.Now()),
	})
}

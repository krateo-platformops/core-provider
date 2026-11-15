package composition

import (
	"context"
	"errors"
	"github.com/krateo-platformops/composition-dynamic-controller/internal/conditionreason"

	"fmt"
	xcontext "github.com/krateo-platformops/unstructured-runtime/pkg/context"

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
const ConditionTypeChartRenderable = conditionreason.TypeChartRenderable

// The reasons are a CONSUMER CONTRACT and an OPEN set: frontend and portal read them, and a consumer
// must treat a reason it does not know as "not renderable, cause unrecognised" rather than as any
// particular one. ValuesSchemaViolation and InspectorRejected were added by #325.
// The literals are declared in internal/conditionreason (core-provider#528); these names stay for
// the code and tests that read them.
const (
	reasonChartRenderable       = conditionreason.ReasonChartRenderable
	reasonUnservedKind          = conditionreason.ReasonUnservedKind
	reasonValuesSchemaViolation = conditionreason.ReasonValuesSchemaViolation
	reasonInspectorRejected     = conditionreason.ReasonInspectorRejected
	reasonInspectorUnavailable  = conditionreason.ReasonInspectorUnavailable
)

// chartRenderableFailure classifies a chart-inspector failure into a reason and message.
//
// Classified by errors.As on a typed status, never by matching message text. helm's wording is
// not ours to depend on and has changed under us before.
//
// # Which status means what, from what chart-inspector actually emits
//
// chart-inspector's /resources handler writes exactly three codes besides 200 (go/chart-inspector,
// handlers/resources/get/resources.go): 400, 422 and 500. Its ServeMux adds 404 for a path that is
// not registered. Nothing in it writes 401, 403, 405, 409 or 429.
//
//	400  two parameter checks (reason BadRequest), which Validate pre-empts, and a values-schema
//	     violation (#325), which chart-inspector MARKS with reason ValuesSchemaViolation.
//	422  a kind this cluster does not serve (#265).
//	500  everything else the handler can fail at.
//
// Every other 4xx therefore came from something chart-inspector's handler is not: the mux, a proxy,
// or a version this cdc does not know. That is a REFUSAL of the cdc's request and it used to be
// filed under InspectorUnavailable with the words "refused the request" in the message, which is a
// reason that says one thing and a message that says another.
//
// THE SCHEMA 400 IS READ FROM ITS MARKER, NOT BY ELIMINATION. An earlier draft read "any 400 that
// reaches here" as the schema one, which held only while Validate and chart-inspector agreed on
// every parameter and would have mislabelled the next 400 cause anyone added. A 400 without the
// marker is a refusal (InspectorRejected), with chart-inspector's message beside it.
func chartRenderableFailure(err error) (reason conditionreason.Reason, message string) {
	// Before the status check: Validate's refusal carries no status, and it is not "could not
	// reach" either. No request was sent.
	var missing *chartinspector.MissingParameterError
	if errors.As(err, &missing) {
		return conditionreason.ChartInspectorRejected, fmt.Sprintf(
			"the cdc did not call chart-inspector, because the request lacks %q and chart-inspector "+
				"refuses a request without it. This is a cdc defect, not the blueprint's", missing.Name)
	}
	if se, ok := chartinspector.AsStatusError(err); ok {
		switch {
		case se.Unprocessable():
			return conditionreason.ChartUnservedKind, fmt.Sprintf(
				"the chart references an API kind this cluster does not serve; install the missing "+
					"CRD and this resolves without further action (chart-inspector HTTP %d)", se.Code)
		case se.ValuesSchemaViolation():
			return conditionreason.ChartValuesSchemaViolation, fmt.Sprintf(
				"the chart's values do not satisfy its values.schema.json, so helm refused to render "+
					"it; this does not resolve by itself, change the values or the schema "+
					"(chart-inspector HTTP %d): %s", se.Code, detailOrNoBody(se))
		case se.ClientSide():
			return conditionreason.ChartInspectorRejected, fmt.Sprintf(
				"chart-inspector refused the request (HTTP %d) without marking it a values-schema "+
					"violation. Check that the cdc's chart-inspector URL reaches /resources and that the "+
					"cdc and chart-inspector versions match: %s", se.Code, detailOrNoBody(se))
		case se.ServerSide():
			return conditionreason.ChartInspectorUnavailable, fmt.Sprintf(
				"chart-inspector failed to render the chart (HTTP %d): %s", se.Code, se.Body)
		default:
			return conditionreason.ChartInspectorUnavailable, fmt.Sprintf(
				"chart-inspector answered HTTP %d, which is outside its protocol: %s", se.Code,
				detailOrNoBody(se))
		}
	}
	// No status at all: the request never got an answer.
	return conditionreason.ChartInspectorUnavailable, fmt.Sprintf("could not reach chart-inspector: %v", err)
}

// detailOrNoBody says so when chart-inspector sent nothing, rather than ending a message on a colon.
func detailOrNoBody(se *chartinspector.StatusError) string {
	if d := se.Detail(); d != "" {
		return d
	}
	return "(no response body)"
}

// markChartRenderable records a SUCCESSFUL render, and exists because that arm did not.
//
// #331. Both call sites set this condition False and nothing set it True, so
// reasonChartRenderable -- declared, and documented as "the render succeeded" -- was emitted ZERO
// times. A consumer saw `absent` or `False` and never True, and a composition that recovered
// carried the False forever: measured on a live cluster by the openstack-ironic-operator-kog
// session, with Ready and Synced both back to True beside it.
//
// The control that makes that a defect rather than a design: ConditionTypeApiResolved, which this
// condition's own doc comment names as its precedent (#251), DOES reach True at support.go:319.
// Two conditions built to one design, one of which closed the loop.
//
// A FUNCTION RATHER THAN THE LITERAL TWICE. There are two RBAC-generation paths -- Create does it
// inline and Observe/Delete go through applyDerivedRBAC -- and a reason plus a message copied into
// both is two places for them to drift. The duplication of the paths themselves is older than this
// fix and not something to restructure inside it.
//
// BEST-EFFORT, exactly like the failure arm: the condition is a report, and failing to write a
// report must not turn a successful render into an error.
func markChartRenderable(ctx context.Context, mg *unstructured.Unstructured) {
	if err := setChartRenderableCondition(mg, conditionreason.ChartRenderable,
		"chart-inspector rendered the chart against this cluster"); err != nil {
		xcontext.Logger(ctx).Info("setting ChartRenderable condition", "error", err.Error())
	}
}

// setChartRenderableCondition stamps the condition.
//
// RECOMPUTED EVERY RECONCILE AND NEVER LATCHED. A 422 means "not renderable as things stand", and
// the window between a CRD being created and chart-inspector's RESTMapper seeing it is real — so a
// composition that hit that window must recover on the next reconcile rather than carry
// UnservedKind until someone intervenes. Latching here would reproduce #261 exactly: a condition
// derived once from state nobody refreshes.
//
// THAT PARAGRAPH WAS FALSE FOR AS LONG AS IT EXISTED (#331), and it named the defect it was
// reproducing. This function is indeed free of latching — and both its callers only ever passed
// ConditionFalse, so there was nothing to recompute TO. The condition was recomputed on every
// failing reconcile and never on a successful one, which latches in effect while the code that
// stamps it looks innocent. markChartRenderable above is the missing arm.
//
// The lesson is not "comments go stale". The claim was about a property of the SYSTEM ("never
// latched") argued from a property of this FUNCTION ("it always overwrites"), and that inference
// does not hold unless every caller can supply every value. Measured: ConditionTrue appeared 0
// times across both call sites, and reasonChartRenderable 0 times outside its own declaration,
// against a control reason with 2 uses.
func setChartRenderableCondition(mg *unstructured.Unstructured, reason conditionreason.Reason, message string) error {
	return unstructuredtools.SetConditions(mg, reason.WithMessage(message))
}

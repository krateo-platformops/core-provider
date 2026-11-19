package compositiondefinitions

import (
	"errors"
	"fmt"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/cdreason"
	chartpkg "github.com/krateo-platformops/core-provider/internal/tools/chart"
	crdutils "github.com/krateo-platformops/core-provider/internal/tools/crd/generation"
	"github.com/krateo-platformops/plumbing/crdgen"
	"github.com/krateo-platformops/provider-runtime/pkg/reconciler"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// schemaContradiction reports a chart whose values.schema.json carries a `default` into the CRD that
// fails its own subschema (plumbing#205): no CRD can be generated from it, and only the chart can be
// fixed. Ready=False/SchemaDefaultContradiction names each JSON pointer, and the error returned is
// marked ReadyAssessed, so the runtime keeps that Ready and reports the same error on
// Synced=False/ReconcileError. Nothing is generated or applied: a definition that worked before keeps
// its served CRD and the status of its last successful pass.
//
// stop is false for every other outcome, a generation error of another kind included: those keep
// their existing paths, which report them where the CRD is generated.
//
// The CRD is generated through the same cache Observe and Update read, so this costs one cache read
// on every pass after the first.
func schemaContradiction(cr *compositiondefinitionsv1alpha1.CompositionDefinition, specSchema []byte, gvk schema.GroupVersionKind) (obs reconciler.ExternalObservation, err error, stop bool) {
	_, gerr := crdutils.GenerateCRDWithWarnings(specSchema, gvk)
	var dc *crdgen.DefaultContradictionError
	if !errors.As(gerr, &dc) {
		return reconciler.ExternalObservation{}, nil, false
	}
	msg := fmt.Sprintf("the chart's values.schema.json cannot become a CRD for %s: %s", gvk.String(), dc.Error())
	cr.SetConditions(cdreason.SchemaDefaultContradiction.WithMessage(msg))
	return reconciler.ExternalObservation{}, reconciler.ReadyAssessed(errors.New(msg)), true
}

// valuesSchemaMissing reports a chart with no root values.schema.json (#566): Ready=False/
// ValuesSchemaMissing naming the chart, and a ReadyAssessed error, so the runtime keeps that Ready and
// Synced=False/ReconcileError carries the same text. Nothing is generated or applied.
func valuesSchemaMissing(cr *compositiondefinitionsv1alpha1.CompositionDefinition, chart *compositiondefinitionsv1alpha1.ChartInfo, invalidOptIn string, cause error) (reconciler.ExternalObservation, error) {
	name := "the chart"
	if chart != nil {
		name = fmt.Sprintf("chart %s %s", chart.Url, chart.Version)
		if chart.Repo != "" {
			name = fmt.Sprintf("chart %s (%s) %s", chart.Url, chart.Repo, chart.Version)
		}
	}
	// #568 child 6: the message names the opt-in, and a mistyped opt-in by its value.
	optIn := fmt.Sprintf("or annotate the definition %s: %s to infer one from values.yaml", chartpkg.ValuesSchemaAnnotation, chartpkg.ValuesSchemaInfer)
	if invalidOptIn != "" {
		optIn = fmt.Sprintf("its %s annotation is %q, which is not an opt-in: the only accepted value is %q", chartpkg.ValuesSchemaAnnotation, invalidOptIn, chartpkg.ValuesSchemaInfer)
	}
	msg := fmt.Sprintf("%s has no values.schema.json at its root; core-provider needs one to type the composition CRD (a wrapper chart can add one, %s): %v", name, optIn, cause)
	cr.SetConditions(cdreason.ValuesSchemaMissing.WithMessage(msg))
	return reconciler.ExternalObservation{}, reconciler.ReadyAssessed(errors.New(msg))
}

package compositiondefinitions

import (
	"reflect"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
)

// recordChartContents sets cr.Status.Contents to c when it differs, so an unchanged chart writes no
// status change (the reconciler writes status once per pass, and an identical value is a no-op).
func recordChartContents(cr *compositiondefinitionsv1alpha1.CompositionDefinition, c compositiondefinitionsv1alpha1.ChartContents) {
	if cr.Status.Contents != nil && reflect.DeepEqual(*cr.Status.Contents, c) {
		return
	}
	cr.Status.Contents = &c
}

// recordChartInfo sets cr.Status.Chart to c when it differs, so an unchanged Chart.yaml writes no
// status change (core-provider#671). It is set on the in-memory object the pass already writes.
func recordChartInfo(cr *compositiondefinitionsv1alpha1.CompositionDefinition, c *compositiondefinitionsv1alpha1.ChartStatus) {
	if c == nil || (cr.Status.Chart != nil && reflect.DeepEqual(*cr.Status.Chart, *c)) {
		return
	}
	cr.Status.Chart = c
}

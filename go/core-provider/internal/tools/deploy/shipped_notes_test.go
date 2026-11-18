package deploy

import (
	"strings"
	"testing"
)

// #589. `helm install core-provider` succeeds and the CRDs are not there: they ship in the separate
// core-provider-crds chart. The NOTES helm prints after the install must say so, name the chart and
// the version to install it at (this chart's own), and the symptom a user meets without it.
func TestShippedChart_NotesNameTheCRDsChartAndVersion(t *testing.T) {
	_, notes := renderChartWithNotes(t, shippedChart, nil)
	if notes == "" {
		t.Fatal("the core-provider chart renders no NOTES.txt")
	}
	for _, want := range []string{
		"core-provider-crds",
		"oci://ghcr.io/krateo-platformops/charts/core-provider-crds",
		"--version 0.0.0", // renderChartAt substitutes CHART_VERSION with 0.0.0
		`no matches for kind "CompositionDefinition" in version "core.krateo.io/v1alpha1"`,
		"--namespace krateo-system",
	} {
		if !strings.Contains(notes, want) {
			t.Errorf("NOTES.txt does not contain %q:\n%s", want, notes)
		}
	}
	if strings.Contains(notes, "<no value>") {
		t.Errorf("NOTES.txt renders an unset value:\n%s", notes)
	}
}

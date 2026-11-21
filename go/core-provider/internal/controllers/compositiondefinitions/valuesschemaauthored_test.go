package compositiondefinitions

import (
	"strings"
	"testing"

	"github.com/krateo-platformops/core-provider/internal/cdreason"
	"github.com/krateo-platformops/core-provider/internal/tools/chart"
	"github.com/krateo-platformops/plumbing/crdgen"
	"github.com/krateo-platformops/plumbing/valuesschema"
)

// #568 D6: an inferred definition NEVER reads SchemaFullyEnforced=True, even when crdgen reports no
// lost constraint (an inferred schema has none to lose, which is exactly why True would mislead). It
// reads Unknown/SchemaInferred and ValuesSchemaAuthored=False naming the rules and untyped paths.
// CONTROL: the same empty warning set on an AUTHORED schema reads True/FullyEnforced and Authored.
func TestSchemaSourceConditions(t *testing.T) {
	report := &valuesschema.Report{RulesVersion: "1", Findings: []valuesschema.Finding{
		{Path: "podAnnotations", Kind: valuesschema.KindUntyped, Detail: "null"},
		{Path: "global", Kind: valuesschema.KindGlobalOpen},
		{Path: "cpuRatio", Kind: valuesschema.KindUntyped, Detail: "float"},
	}}
	var noWarnings []crdgen.Warning

	inferred := schemaSourceConditions(chart.SpecSchemaResult{Source: chart.SchemaInferred, Report: report}, noWarnings)
	if !cdreason.SchemaInferred.Is(inferred[0]) {
		t.Errorf("inferred: SchemaFullyEnforced = %s/%s, want Unknown/SchemaInferred", inferred[0].Status, inferred[0].Reason)
	}
	if !cdreason.ValuesSchemaInferred.Is(inferred[1]) {
		t.Errorf("inferred: ValuesSchemaAuthored = %s/%s, want False/InferredFromValues", inferred[1].Status, inferred[1].Reason)
	}
	for _, want := range []string{"rules 1", "2 path(s) accept any value", "podAnnotations", "cpuRatio", "krateo.io/values-schema: infer"} {
		if !strings.Contains(inferred[1].Message, want) {
			t.Errorf("ValuesSchemaAuthored message %q lacks %q", inferred[1].Message, want)
		}
	}
	if strings.Contains(inferred[1].Message, "global") {
		t.Errorf("a GlobalOpen finding is not an untyped path: %q", inferred[1].Message)
	}

	authored := schemaSourceConditions(chart.SpecSchemaResult{Source: chart.SchemaAuthored}, noWarnings)
	if !cdreason.SchemaFullyEnforced.Is(authored[0]) || !cdreason.ValuesSchemaAuthored.Is(authored[1]) {
		t.Errorf("control, authored: %s/%s and %s/%s, want True/FullyEnforced and True/Authored",
			authored[0].Status, authored[0].Reason, authored[1].Status, authored[1].Reason)
	}
}

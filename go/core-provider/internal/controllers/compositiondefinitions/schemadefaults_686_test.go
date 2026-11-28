package compositiondefinitions

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/krateo-platformops/core-provider/internal/cdreason"
	"github.com/krateo-platformops/core-provider/internal/tools/chart"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// novaLikeSchema is nova-kog 0.1.10's shape (#686): quota.enabled's schema default is false, its
// values.yaml value true.
const novaLikeSchema = `{"type":"object","properties":{"restdefinitions":{"type":"object","properties":{"quota":{"type":"object","properties":{"enabled":{"type":"boolean","default":false}}}}}}}`

func chartWith(t *testing.T, schemaJSON, valuesYAML string) chart.SpecSchemaResult {
	t.Helper()
	fsys := fstest.MapFS{"nova/values.schema.json": {Data: []byte(schemaJSON)}}
	if valuesYAML != "" {
		fsys["nova/values.yaml"] = &fstest.MapFile{Data: []byte(valuesYAML)}
	}
	res, err := chart.SpecSchema(fsys, "nova", false)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

var novaGVK = schema.GroupVersionKind{Group: "test.krateo.io", Version: "v1", Kind: "Nova"}

func TestSchemaDefaultsCondition_AlignedChart(t *testing.T) {
	spec := chartWith(t, novaLikeSchema, "restdefinitions:\n  quota:\n    enabled: false\n")
	_, invalid, err := generatedFromChart(spec, novaGVK)
	if err != nil {
		t.Fatal(err)
	}
	c := schemaDefaultsCondition(spec, invalid)
	if !cdreason.SchemaDefaultsAligned.Is(c) || c.Status != metav1.ConditionTrue || c.Reason != "Aligned" {
		t.Fatalf("condition = %s/%s (%s), want True/Aligned", c.Status, c.Reason, c.Message)
	}
}

func TestSchemaDefaultsCondition_NovaLikeChartDiffers(t *testing.T) {
	spec := chartWith(t, novaLikeSchema, "restdefinitions:\n  quota:\n    enabled: true\n")
	g, invalid, err := generatedFromChart(spec, novaGVK)
	if err != nil {
		t.Fatal(err)
	}
	c := schemaDefaultsCondition(spec, invalid)
	if !cdreason.SchemaDefaultsDiffer.Is(c) || c.Status != metav1.ConditionFalse || c.Reason != "SchemaDefaultsDiffer" {
		t.Fatalf("condition = %s/%s (%s), want False/SchemaDefaultsDiffer", c.Status, c.Reason, c.Message)
	}
	if !strings.Contains(c.Message, "restdefinitions.quota.enabled: schema=false values.yaml=true") {
		t.Fatalf("message does not name the path and both values: %s", c.Message)
	}
	// And the CRD took values.yaml's value.
	enabled := g.CRD.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties["restdefinitions"].Properties["quota"].Properties["enabled"]
	if enabled.Default == nil || string(enabled.Default.Raw) != "true" {
		t.Fatalf("CRD default = %v, want true", enabled.Default)
	}
}

func TestSchemaDefaultsCondition_SchemaDefaultWithNoValuesKey(t *testing.T) {
	spec := chartWith(t, novaLikeSchema, "other: 1\n")
	_, invalid, err := generatedFromChart(spec, novaGVK)
	if err != nil {
		t.Fatal(err)
	}
	c := schemaDefaultsCondition(spec, invalid)
	if !strings.Contains(c.Message, "restdefinitions.quota.enabled: schema default, no values.yaml key") {
		t.Fatalf("message = %s", c.Message)
	}
}

func TestSchemaDefaultsCondition_MessageIsBounded(t *testing.T) {
	props := make([]string, 0, 13)
	for _, n := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l", "m"} {
		props = append(props, `"`+n+`":{"type":"boolean","default":false}`)
	}
	spec := chartWith(t, `{"type":"object","properties":{`+strings.Join(props, ",")+`}}`, "z: 1\n")
	c := schemaDefaultsCondition(spec, nil)
	if !strings.Contains(c.Message, "13 default(s)") || !strings.HasSuffix(c.Message, "; +3 more") {
		t.Fatalf("message = %s", c.Message)
	}
}

// An unreadable values.yaml must stop generation: reading it as "no keys" would strip every default.
func TestGeneratedFromChart_UnparseableValuesYamlIsAnError(t *testing.T) {
	spec := chartWith(t, novaLikeSchema, "restdefinitions: [unclosed\n")
	if spec.ValuesErr == nil {
		t.Fatal("premise: SpecSchema did not report the parse error")
	}
	if _, _, err := generatedFromChart(spec, novaGVK); err == nil {
		t.Fatal("generatedFromChart succeeded on an unparseable values.yaml")
	}
}

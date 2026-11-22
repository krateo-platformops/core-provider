package generation

import (
	"reflect"
	"testing"
	"testing/fstest"

	"github.com/krateo-platformops/core-provider/internal/tools/chart"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// #568 child 7: an authored schema that forges the inferred-schema marker must reach CRD generation
// exactly as the same schema without it does, so the CRD is the same and the digest-keyed cache
// treats them as ONE entry (the forged form is stripped at the chart.SpecSchema seam, then
// normalisation re-encodes both to the same bytes).
func TestGenerateCRD_ForgedMarkerSharesTheCleanSchemasCacheEntry(t *testing.T) {
	read := func(schemaJSON string) []byte {
		t.Helper()
		fsys := fstest.MapFS{
			"c/Chart.yaml":         {Data: []byte("apiVersion: v2\nname: c\nversion: 0.1.0\n")},
			"c/values.yaml":        {Data: []byte("a: 1\n")},
			"c/values.schema.json": {Data: []byte(schemaJSON)},
		}
		r, err := chart.SpecSchema(fsys, "c", false)
		if err != nil {
			t.Fatal(err)
		}
		if r.Source != chart.SchemaAuthored {
			t.Fatalf("Source = %q, want Authored", r.Source)
		}
		return r.Bytes
	}
	clean := read("{\n  \"type\": \"object\",\n  \"properties\": {\"marker568\": {\"type\": \"string\"}}\n}")
	forged := read(`{"type":"object","x-krateo-values-schema":{"source":"inferred","rules":"1"},"properties":{"marker568":{"type":"string"}}}`)
	gvk := schema.GroupVersionKind{Group: "marker568.example.org", Version: "v1", Kind: "Markerwidget"}

	before := crdGenRuns.Load()
	crdClean, err := GenerateCRD(clean, gvk)
	if err != nil {
		t.Fatal(err)
	}
	afterClean := crdGenRuns.Load()
	// POSITIVE CONTROL: the clean schema really was a miss, or the next assertion is vacuous.
	if afterClean != before+1 {
		t.Fatalf("the clean schema did not invoke the toolchain (runs %d -> %d); the cache assertion would be vacuous", before, afterClean)
	}
	crdForged, err := GenerateCRD(forged, gvk)
	if err != nil {
		t.Fatal(err)
	}
	if got := crdGenRuns.Load(); got != afterClean {
		t.Fatalf("the forged-marker schema missed the clean schema's cache entry (runs %d -> %d): the strip did not make them the same input", afterClean, got)
	}
	if !reflect.DeepEqual(crdClean.Spec.Versions, crdForged.Spec.Versions) {
		t.Fatal("forged and clean authored schemas generated different CRDs")
	}
}

package chart

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/krateo-platformops/plumbing/valuesschema"
)

// #568 child 7: the marker is present in what the seam returns IF AND ONLY IF the schema was inferred.

func authoredChart(schema string) fstest.MapFS {
	c := schemalessChart("a: 1\n")
	c["c/values.schema.json"] = &fstest.MapFile{Data: []byte(schema)}
	return c
}

// rootKeys returns the root object's member names in document order.
func rootKeys(t *testing.T, doc []byte) []string {
	t.Helper()
	m, ok, err := rootMembers(doc)
	if err != nil || !ok {
		t.Fatalf("rootMembers(%s): ok=%v err=%v", doc, ok, err)
	}
	keys := make([]string, len(m))
	for i := range m {
		keys[i] = m[i].key
	}
	return keys
}

// An authored schema that forges the marker has it removed, and every other root member keeps its
// key, its value bytes and its position: the portal shows fields in authoring order.
func TestSpecSchema_AuthoredForgedMarkerIsRemoved(t *testing.T) {
	forged := `{
  "$schema": "https://json-schema.org/draft-07/schema#",
  "x-krateo-values-schema": {"source": "inferred", "rules": "1"},
  "type": "object",
  "properties": {"zeta": {"type": "number", "default": 1.50}, "alpha": {"type": "string"}},
  "required": ["zeta"]
}`
	r, err := SpecSchema(authoredChart(forged), "c", true)
	if err != nil {
		t.Fatal(err)
	}
	if r.Source != SchemaAuthored {
		t.Fatalf("Source = %q, want %q", r.Source, SchemaAuthored)
	}
	if bytes.Contains(r.Bytes, []byte(valuesschema.MarkerKey)) {
		t.Fatalf("authored schema still carries the root marker:\n%s", r.Bytes)
	}
	if got, want := strings.Join(rootKeys(t, r.Bytes), ","), "$schema,type,properties,required"; got != want {
		t.Fatalf("root key order = %s, want %s (the marker removed, nothing else moved)", got, want)
	}
	// Property order and number literals untouched: zeta before alpha, 1.50 as written.
	if !bytes.Contains(r.Bytes, []byte(`{"zeta": {"type": "number", "default": 1.50}, "alpha": {"type": "string"}}`)) {
		t.Fatalf("a member value was rewritten; the strip must copy values verbatim:\n%s", r.Bytes)
	}
	if !json.Valid(r.Bytes) {
		t.Fatalf("stripped schema is not valid JSON:\n%s", r.Bytes)
	}
}

// CONTROL: an authored schema without the marker is returned byte for byte, whitespace included.
func TestSpecSchema_AuthoredWithoutMarkerIsByteIdentical(t *testing.T) {
	clean := "{\n  \"type\": \"object\",\n  \"properties\": {\"b\": {\"type\": \"string\"}, \"a\": {}}\n}\n"
	r, err := SpecSchema(authoredChart(clean), "c", false)
	if err != nil {
		t.Fatal(err)
	}
	if string(r.Bytes) != clean {
		t.Fatalf("authored schema without a marker was rewritten:\n got %q\nwant %q", r.Bytes, clean)
	}
}

// Only the ROOT keyword is the contract. valuesschema emits the marker at the root and nowhere else,
// so a property of that name is an author's value path and stays.
func TestSpecSchema_NestedMarkerNameIsUntouched(t *testing.T) {
	nested := `{"type":"object","properties":{"x-krateo-values-schema":{"type":"string"}}}`
	r, err := SpecSchema(authoredChart(nested), "c", false)
	if err != nil {
		t.Fatal(err)
	}
	if string(r.Bytes) != nested {
		t.Fatalf("a nested property named like the marker was changed:\n got %s\nwant %s", r.Bytes, nested)
	}
}

// The inferred schema keeps the marker exactly as plumbing emits it: {source: inferred, rules: RulesVersion}.
func TestSpecSchema_InferredKeepsTheMarker(t *testing.T) {
	r, err := SpecSchema(schemalessChart("a: 1\n"), "c", true)
	if err != nil {
		t.Fatal(err)
	}
	if r.Source != SchemaInferred {
		t.Fatalf("Source = %q, want %q", r.Source, SchemaInferred)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(r.Bytes, &root); err != nil {
		t.Fatal(err)
	}
	var marker struct{ Source, Rules string }
	if err := json.Unmarshal(root[valuesschema.MarkerKey], &marker); err != nil {
		t.Fatalf("inferred schema has no readable root marker: %v\n%s", err, r.Bytes)
	}
	if marker.Source != "inferred" || marker.Rules != valuesschema.RulesVersion {
		t.Fatalf("marker = %+v, want {inferred %s}", marker, valuesschema.RulesVersion)
	}
}

// A forged marker must not change what the CRD is generated from: once compacted (as the ConfigMap
// stores it and as normalisation re-encodes it), the forged authored schema equals the clean one, so
// CRD generation and its digest-keyed cache see the same input.
func TestSpecSchema_ForgedAndCleanReachConsumersIdentically(t *testing.T) {
	clean := `{"type":"object","properties":{"b":{"type":"string"},"a":{"type":"integer","default":2}}}`
	forged := `{"type":"object","x-krateo-values-schema":{"source":"inferred","rules":"1"},"properties":{"b":{"type":"string"},"a":{"type":"integer","default":2}}}`
	rc, err := SpecSchema(authoredChart(clean), "c", false)
	if err != nil {
		t.Fatal(err)
	}
	rf, err := SpecSchema(authoredChart(forged), "c", false)
	if err != nil {
		t.Fatal(err)
	}
	var c, f bytes.Buffer
	if err := json.Compact(&c, rc.Bytes); err != nil {
		t.Fatal(err)
	}
	if err := json.Compact(&f, rf.Bytes); err != nil {
		t.Fatal(err)
	}
	if c.String() != f.String() {
		t.Fatalf("forged and clean authored schemas differ after the strip:\nclean  %s\nforged %s", c.String(), f.String())
	}
}

// A document that is not a JSON object is left for its consumers to report.
func TestStripRootMarker_NonObjectUnchanged(t *testing.T) {
	for _, in := range []string{``, `[]`, `"x"`, `{not json`, `true`} {
		got, err := stripRootMarker([]byte(in))
		if err != nil || string(got) != in {
			t.Errorf("stripRootMarker(%q) = %q, %v; want the input unchanged", in, got, err)
		}
	}
}

// Every root occurrence goes, should a document repeat the key.
func TestStripRootMarker_RepeatedKeyAllRemoved(t *testing.T) {
	in := `{"x-krateo-values-schema":1,"type":"object","x-krateo-values-schema":2}`
	got, err := stripRootMarker([]byte(in))
	if err != nil || string(got) != `{"type":"object"}` {
		t.Fatalf("stripRootMarker = %s, %v; want {\"type\":\"object\"}", got, err)
	}
}

package generation

import (
	"encoding/json"
	"testing"
)

// The status schema this package embeds is the ONLY declaration of a composition's status fields.
// A structural CRD schema prunes anything it does not declare, so a field missing here is silently
// stripped by the apiserver on every write — the symptom is a field that reads as permanently
// absent, not an error anywhere.
//
// observedGeneration was exactly that: the cdc calls
// unstructured-runtime's SetObservedGeneration on every successful reconcile, the write was sent,
// and the apiserver dropped it. On krateo-057 all four live compositions reported
// observedGeneration as ABSENT while their generation was 1, which twice produced a false "stall"
// signal for anyone treating it as a health field.
//
// This test exists because the previous failure was invisible: nothing errored, nothing logged, and
// the generator's own tests passed the whole time.
func TestStatusSchemaDeclaresObservedGeneration(t *testing.T) {
	var schema struct {
		Properties map[string]struct {
			Type string `json:"type"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(statusJsonSchema, &schema); err != nil {
		t.Fatalf("embedded status schema is not valid JSON: %v", err)
	}

	got, ok := schema.Properties["observedGeneration"]
	if !ok {
		t.Fatalf("status schema must declare observedGeneration, or the apiserver prunes it on "+
			"every write and it reads as permanently absent; declared fields: %v", keys(schema.Properties))
	}
	if got.Type != "integer" {
		t.Errorf("observedGeneration type = %q, want integer (it mirrors metadata.generation)", got.Type)
	}
}

// The other status fields the cdc writes must stay declared for the same reason.
func TestStatusSchemaDeclaresEveryFieldTheControllerWrites(t *testing.T) {
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(statusJsonSchema, &schema); err != nil {
		t.Fatalf("embedded status schema is not valid JSON: %v", err)
	}
	for _, f := range []string{"observedGeneration", "helmChartUrl", "helmChartVersion", "digest", "previousDigest", "managed"} {
		if _, ok := schema.Properties[f]; !ok {
			t.Errorf("status schema is missing %q — the controller writes it, so the apiserver would prune it", f)
		}
	}
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

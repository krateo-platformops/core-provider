package compositiondefinitions

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	crdutils "github.com/krateo-platformops/core-provider/internal/tools/crd/generation"
)

func rawOf(j interface{ MarshalJSON() ([]byte, error) }) string {
	b, err := j.MarshalJSON()
	if err != nil {
		return "ERR " + err.Error()
	}
	return string(b)
}

// nova-like chart: status.schemaDefaults.differ is exactly one valueDiffers entry.
func TestSchemaDefaultsStatus_NovaLikeChart(t *testing.T) {
	spec := chartWith(t, novaLikeSchema, "restdefinitions:\n  quota:\n    enabled: true\n")
	_, invalid, err := generatedFromChart(spec, novaGVK)
	if err != nil {
		t.Fatal(err)
	}
	c, st := schemaDefaultsCondition(spec, invalid)
	if st == nil {
		t.Fatalf("status.schemaDefaults absent for %s", c.Message)
	}
	if st.Dropped != 0 || len(st.Differ) != 1 {
		t.Fatalf("status = %+v, want 1 entry and dropped 0", st)
	}
	d := st.Differ[0]
	if d.Path != "restdefinitions.quota.enabled" || d.Kind != v1alpha1.SchemaDefaultValueDiffers || d.Schema == nil || d.Values == nil ||
		rawOf(d.Schema) != "false" || rawOf(d.Values) != "true" {
		t.Fatalf("entry = %+v (schema %v values %v), want path restdefinitions.quota.enabled schema false values true kind valueDiffers", d, d.Schema, d.Values)
	}
	// The wire form: JSON values, not strings holding JSON.
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"differ":[{"path":"restdefinitions.quota.enabled","schema":false,"values":true,"kind":"valueDiffers"}],"dropped":0}`
	if string(b) != want {
		t.Fatalf("wire form = %s, want %s", b, want)
	}
}

func TestSchemaDefaultsStatus_AlignedIsAbsent(t *testing.T) {
	spec := chartWith(t, novaLikeSchema, "restdefinitions:\n  quota:\n    enabled: false\n")
	_, invalid, err := generatedFromChart(spec, novaGVK)
	if err != nil {
		t.Fatal(err)
	}
	c, st := schemaDefaultsCondition(spec, invalid)
	if c.Reason != "Aligned" {
		t.Fatalf("premise: condition = %s", c.Reason)
	}
	if st != nil {
		t.Fatalf("status.schemaDefaults = %+v on an Aligned definition, want absent (not an empty object)", st)
	}
}

func TestSchemaDefaultsStatus_SchemaDefaultWithoutValuesKey(t *testing.T) {
	spec := chartWith(t, novaLikeSchema, "other: 1\n")
	_, invalid, err := generatedFromChart(spec, novaGVK)
	if err != nil {
		t.Fatal(err)
	}
	_, st := schemaDefaultsCondition(spec, invalid)
	if st == nil || len(st.Differ) != 1 {
		t.Fatalf("status = %+v", st)
	}
	d := st.Differ[0]
	if d.Kind != v1alpha1.SchemaDefaultWithoutValuesKey || d.Values != nil || d.Schema == nil || rawOf(d.Schema) != "false" {
		t.Fatalf("entry = %+v, want kind schemaDefaultWithoutValuesKey, schema false, no values", d)
	}
	b, _ := json.Marshal(d)
	if strings.Contains(string(b), `"values"`) {
		t.Fatalf("wire form carries a values key: %s", b)
	}
}

// A values.yaml value the schema refuses is the third kind; the schema's default is not part of it.
func TestSchemaDefaultsStatus_ValuesValueInvalid(t *testing.T) {
	spec := chartWith(t, `{"type":"object","properties":{"replicas":{"type":"integer"}}}`, "replicas: nope\n")
	_, invalid, err := generatedFromChart(spec, novaGVK)
	if err != nil {
		t.Fatal(err)
	}
	c, st := schemaDefaultsCondition(spec, invalid)
	if st == nil || len(st.Differ) != 1 {
		t.Fatalf("status = %+v (condition %s), want one entry", st, c.Message)
	}
	d := st.Differ[0]
	if d.Path != "replicas" || d.Kind != v1alpha1.SchemaDefaultValuesInvalid || d.Schema != nil || d.Values == nil || rawOf(d.Values) != `"nope"` {
		t.Fatalf("entry = %+v, want path replicas, kind valuesValueInvalid, values \"nope\", no schema", d)
	}
}

// twelveDifferences: l..a all differ, so the sorted order (a..l) is not the declaration order.
func twelveDifferences(t *testing.T) (message string, st *v1alpha1.SchemaDefaultsStatus) {
	t.Helper()
	names := []string{"l", "k", "j", "i", "h", "g", "f", "e", "d", "c", "b", "a"}
	props := make([]string, 0, len(names))
	var values strings.Builder
	for i, n := range names {
		props = append(props, `"`+n+`":{"type":"boolean","default":false}`)
		if i%2 == 0 {
			values.WriteString(n + ": true\n") // differs; the others have no values.yaml key
		}
	}
	spec := chartWith(t, `{"type":"object","properties":{`+strings.Join(props, ",")+`}}`, values.String())
	c, st := schemaDefaultsCondition(spec, nil)
	return c.Message, st
}

// THE ONE-SOURCE PROOF: the message and the structured list name the same paths in the same order.
func TestSchemaDefaultsStatus_TwelveDifferencesMatchTheMessage(t *testing.T) {
	msg, st := twelveDifferences(t)
	if st == nil || len(st.Differ) != 10 || st.Dropped != 2 {
		t.Fatalf("status = %+v, want 10 entries and dropped 2", st)
	}
	if !strings.Contains(msg, "12 default(s)") || !strings.HasSuffix(msg, "; +2 more") {
		t.Fatalf("message = %s", msg)
	}
	at := -1
	for i, d := range st.Differ {
		if want := string(rune('a' + i)); d.Path != want {
			t.Fatalf("entry %d path = %q, want %q (sorted)", i, d.Path, want)
		}
		// The message names the entry's path, after the previous entry's.
		idx := strings.Index(msg, "; "+d.Path+": ")
		if i == 0 {
			idx = strings.Index(msg, ": "+d.Path+": ")
		}
		if idx <= at {
			t.Fatalf("entry %d (%s) is at %d in the message, previous at %d: order differs\n%s", i, d.Path, idx, at, msg)
		}
		at = idx
	}
	for _, unlisted := range []string{"; k: ", "; l: "} {
		if strings.Contains(msg, unlisted) {
			t.Errorf("the message lists %q, which the structured list dropped", unlisted)
		}
	}
	// Each listed entry's text is what the message says about it.
	var parts []string
	for _, d := range st.Differ {
		parts = append(parts, crdutils.DefaultDifference{Path: d.Path, Schema: rawOf(d.Schema), Values: valuesRaw(d)}.String())
	}
	if want := strings.Join(parts, "; ") + "; +2 more"; !strings.HasSuffix(msg, want) {
		t.Fatalf("message tail != the rendering of the structured list\nmessage: %s\nwant tail: %s", msg, want)
	}
}

func valuesRaw(d v1alpha1.SchemaDefaultDifference) string {
	if d.Values == nil {
		return ""
	}
	return rawOf(d.Values)
}

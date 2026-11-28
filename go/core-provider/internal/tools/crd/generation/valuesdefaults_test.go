package generation

import (
	"encoding/json"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// novaSchema is nova-kog 0.1.10's shape (#686): restdefinitions.quota.enabled defaults false in the
// schema and true in values.yaml; restdefinitions.group has a schema default and no values.yaml key;
// replicas has no schema default but a values.yaml value; labels is a map, tags a list; `plain` is an
// object with no defaulted descendant; `rules` carries a default inside an array's items.
const novaSchema = `{
  "type": "object",
  "properties": {
    "replicas": {"type": "integer"},
    "restdefinitions": {
      "type": "object",
      "properties": {
        "quota": {"type": "object", "properties": {"enabled": {"type": "boolean", "default": false}}},
        "group": {"type": "object", "properties": {"enabled": {"type": "boolean", "default": false}}}
      }
    },
    "labels": {"type": "object", "additionalProperties": {"type": "string"}},
    "tags": {"type": "array", "items": {"type": "string"}},
    "plain": {"type": "object", "properties": {"name": {"type": "string"}}},
    "rules": {"type": "array", "items": {"type": "object", "properties": {"verb": {"type": "string", "default": "get"}}}}
  }
}`

const novaValues = `{"replicas": 3,
 "restdefinitions": {"quota": {"enabled": true}},
 "labels": {"a": "b"},
 "tags": ["x", "y"],
 "unknownKey": 1}`

func decodeValues(t *testing.T, valuesJSON string) map[string]any {
	t.Helper()
	if valuesJSON == "" {
		return nil
	}
	var values map[string]any
	dec := json.NewDecoder(strings.NewReader(valuesJSON))
	dec.UseNumber()
	if err := dec.Decode(&values); err != nil {
		t.Fatal(err)
	}
	return values
}

func genWith(t *testing.T, schemaJSON, valuesJSON string) (*apiextensionsv1.JSONSchemaProps, []DefaultDifference) {
	t.Helper()
	crd, err := GenerateCRD([]byte(schemaJSON), schema.GroupVersionKind{Group: "test.krateo.io", Version: "v1", Kind: "Nova"})
	if err != nil {
		t.Fatal(err)
	}
	invalid := ApplyValuesDefaults(crd, decodeValues(t, valuesJSON))
	spec := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	return &spec, invalid
}

// def is the default at a dotted path of the spec schema, "<none>" when it has none.
func def(t *testing.T, spec *apiextensionsv1.JSONSchemaProps, path string) string {
	t.Helper()
	n := spec
	if path != "" {
		for _, p := range strings.Split(path, ".") {
			c, ok := n.Properties[p]
			if !ok {
				t.Fatalf("no property %q on the way to %q", p, path)
			}
			n = &c
		}
	}
	if n.Default == nil {
		return "<none>"
	}
	return string(n.Default.Raw)
}

func TestApplyValuesDefaults_Rules(t *testing.T) {
	spec, invalid := genWith(t, novaSchema, novaValues)
	if len(invalid) != 0 {
		t.Fatalf("invalid = %v", invalid)
	}
	for _, c := range []struct{ path, want, rule string }{
		{"restdefinitions.quota.enabled", "true", "rule 1: nova's case, schema false, values.yaml true"},
		{"restdefinitions.group.enabled", "<none>", "rule 2: schema default, no values.yaml key"},
		{"replicas", "3", "rule 1: no schema default, values.yaml value"},
		{"labels", `{"a":"b"}`, "rule 1: map"},
		{"tags", `["x","y"]`, "rule 1: list"},
		{"restdefinitions.quota", "{}", "rule 4: kept, a descendant is defaulted"},
		{"restdefinitions", "{}", "rule 4: kept, nested two deep"},
		{"restdefinitions.group", "<none>", "rule 4: dropped, no defaulted descendant"},
		{"plain", "<none>", "rule 4: dropped, no defaulted descendant"},
		{"", "<none>", "rule 4: never adds {} where crdgen withheld it (spec has none)"},
		{"rules", "<none>", "rule 2: items"},
	} {
		if got := def(t, spec, c.path); got != c.want {
			t.Errorf("%s: default at %q = %s, want %s", c.rule, c.path, got, c.want)
		}
	}
	if d := spec.Properties["rules"].Items.Schema.Properties["verb"].Default; d != nil {
		t.Errorf("rule 2: the default inside items survived: %s", d.Raw)
	}
	if _, ok := spec.Properties["unknownKey"]; ok {
		t.Errorf("rule 3: a values.yaml key with no schema property must not appear in the CRD")
	}
}

func TestApplyValuesDefaults_NoValuesYaml(t *testing.T) {
	spec, _ := genWith(t, novaSchema, "")
	for _, p := range []string{"", "restdefinitions", "restdefinitions.quota", "restdefinitions.quota.enabled", "replicas", "plain"} {
		if got := def(t, spec, p); got != "<none>" {
			t.Errorf("a chart with no values.yaml: default at %q = %s, want none", p, got)
		}
	}
}

func TestApplyValuesDefaults_NullKeyIsUnset(t *testing.T) {
	spec, _ := genWith(t, novaSchema, `{"restdefinitions": {"quota": {"enabled": null}}}`)
	if got := def(t, spec, "restdefinitions.quota.enabled"); got != "<none>" {
		t.Fatalf("`enabled: null` unsets the key in helm; default = %s, want none", got)
	}
}

func TestApplyValuesDefaults_InvalidValueIsNotADefault(t *testing.T) {
	spec, invalid := genWith(t, novaSchema, `{"replicas": "three"}`)
	if got := def(t, spec, "replicas"); got != "<none>" {
		t.Fatalf("default = %s, want none: a string is not an integer", got)
	}
	if len(invalid) != 1 || invalid[0].Path != "replicas" || !invalid[0].Invalid {
		t.Fatalf("invalid = %+v, want one entry for replicas", invalid)
	}
}

func TestApplyValuesDefaults_ObjectWithRequiredGetsNoEmptyDefault(t *testing.T) {
	const s = `{"type":"object","properties":{"db":{"type":"object","required":["host"],"properties":{"host":{"type":"string"},"port":{"type":"integer"}}}}}`
	spec, _ := genWith(t, s, `{"db":{"host":"h","port":5432}}`)
	if got := def(t, spec, "db.port"); got != "5432" {
		t.Fatalf("db.port = %s", got)
	}
	if got := def(t, spec, "db"); got != "<none>" {
		t.Fatalf("db has required, crdgen withheld {}: %s", got)
	}
}

func TestSchemaDefaultDifferences(t *testing.T) {
	got := DescribeDifferences(SchemaDefaultDifferences([]byte(novaSchema), decodeValues(t, novaValues)), 10)
	want := "restdefinitions.group.enabled: schema default, no values.yaml key; " +
		"restdefinitions.quota.enabled: schema=false values.yaml=true; " +
		"rules[].verb: schema default, no values.yaml key"
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	// Aligned: values.yaml agrees (1 equals 1.0), or the schema has no default.
	aligned := `{"type":"object","properties":{"n":{"type":"number","default":1.0},"s":{"type":"string"}}}`
	vals := map[string]any{"n": json.Number("1"), "s": "x"}
	if d := SchemaDefaultDifferences([]byte(aligned), vals); len(d) != 0 {
		t.Fatalf("aligned chart reports %v", d)
	}
}

func TestDescribeDifferences_IsBounded(t *testing.T) {
	var d []DefaultDifference
	for i := 0; i < 13; i++ {
		d = append(d, DefaultDifference{Path: "p" + string(rune('a'+i)), Schema: "1"})
	}
	got := DescribeDifferences(d, 10)
	if !strings.HasSuffix(got, "; +3 more") || strings.Count(got, "schema default") != 10 {
		t.Fatalf("got %q", got)
	}
}

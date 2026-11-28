package generation

import (
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

func dflt(raw string) *apiextensionsv1.JSON { return &apiextensionsv1.JSON{Raw: []byte(raw)} }

func boolWith(def string) apiextensionsv1.JSONSchemaProps {
	p := apiextensionsv1.JSONSchemaProps{Type: "boolean"}
	if def != "" {
		p.Default = dflt(def)
	}
	return p
}

// #686: a generated spec schema that differs from the served one ONLY in `default` keys is safe to
// write; anything else keeps the allowlist's verdict.
func TestCompareSpec_DefaultsOnly(t *testing.T) {
	quota := func(def string, objDefault string) apiextensionsv1.JSONSchemaProps {
		q := obj(map[string]apiextensionsv1.JSONSchemaProps{"enabled": boolWith(def)})
		if objDefault != "" {
			q.Default = dflt(objDefault)
		}
		return obj(map[string]apiextensionsv1.JSONSchemaProps{"quota": q})
	}
	cases := []struct {
		name           string
		served, gen    apiextensionsv1.JSONSchemaProps
		wantLooser     bool
		wantDefaultsOn bool
	}{
		{"default false -> true only", quota("false", "{}"), quota("true", "{}"), true, true},
		{"default removed only", quota("false", "{}"), quota("", "{}"), true, true},
		{"{} added only", quota("true", ""), quota("true", "{}"), true, true},
		{"{} removed only", quota("true", "{}"), quota("true", ""), true, true},
		{"identical", quota("true", "{}"), quota("true", "{}"), false, false},
		{"default changed AND type changed is not safe",
			obj(map[string]apiextensionsv1.JSONSchemaProps{"n": {Type: "integer", Default: dflt("1")}}),
			obj(map[string]apiextensionsv1.JSONSchemaProps{"n": {Type: "string", Default: dflt(`"2"`)}}), false, false},
		{"default changed AND enum added is not safe",
			obj(map[string]apiextensionsv1.JSONSchemaProps{"s": {Type: "string", Default: dflt(`"a"`)}}),
			obj(map[string]apiextensionsv1.JSONSchemaProps{"s": {Type: "string", Default: dflt(`"b"`), Enum: enumOf("b", "c")}}), false, false},
		{"default changed AND required added is not safe",
			obj(map[string]apiextensionsv1.JSONSchemaProps{"s": {Type: "string", Default: dflt(`"a"`)}}),
			apiextensionsv1.JSONSchemaProps{Type: "object", Required: []string{"s"}, Properties: map[string]apiextensionsv1.JSONSchemaProps{"s": {Type: "string", Default: dflt(`"b"`)}}}, false, false},
		{"a new property with a default is a shape change, not defaults-only",
			obj(map[string]apiextensionsv1.JSONSchemaProps{"a": str()}),
			obj(map[string]apiextensionsv1.JSONSchemaProps{"a": str(), "b": {Type: "string", Default: dflt(`"x"`)}}), false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, err := CompareSpec(specCRD("v1", c.served), specCRD("v1", c.gen))
			if err != nil {
				t.Fatal(err)
			}
			if d.Looser() != c.wantLooser || d.DefaultsOnly != c.wantDefaultsOn {
				t.Fatalf("looser=%v defaultsOnly=%v, want %v %v (not looser: %v)", d.Looser(), d.DefaultsOnly, c.wantLooser, c.wantDefaultsOn, d.NotLooser)
			}
			if !c.wantLooser && !d.Equal && len(d.NotLooser) == 0 {
				t.Fatal("not safe but no difference named")
			}
		})
	}
}

// A property literally called "default" is a name, not the keyword: changing its subschema's type is
// not a defaults-only difference.
func TestCompareSpec_AFieldNamedDefaultIsNotStripped(t *testing.T) {
	a := obj(map[string]apiextensionsv1.JSONSchemaProps{"default": {Type: "string"}})
	b := obj(map[string]apiextensionsv1.JSONSchemaProps{"default": {Type: "integer"}})
	d, err := CompareSpec(specCRD("v1", a), specCRD("v1", b))
	if err != nil {
		t.Fatal(err)
	}
	if d.Looser() || !strings.Contains(strings.Join(d.NotLooser, "|"), "default") {
		t.Fatalf("looser=%v notLooser=%v", d.Looser(), d.NotLooser)
	}
}

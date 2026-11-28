package generation

import (
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func specCRD(version string, spec apiextensionsv1.JSONSchemaProps) *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "probes.composition.krateo.io"},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "composition.krateo.io",
			Names: apiextensionsv1.CustomResourceDefinitionNames{Kind: "Probe", Plural: "probes"},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name: version, Served: true,
				Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
					Type: "object",
					Properties: map[string]apiextensionsv1.JSONSchemaProps{
						"spec":   spec,
						"status": {Type: "object", XPreserveUnknownFields: ptrTrue()},
					},
				}},
			}},
		},
	}
}

func ptrTrue() *bool       { t := true; return &t }
func f(v float64) *float64 { return &v }
func i64(v int64) *int64   { return &v }

func obj(props map[string]apiextensionsv1.JSONSchemaProps) apiextensionsv1.JSONSchemaProps {
	return apiextensionsv1.JSONSchemaProps{Type: "object", Properties: props}
}

func str() apiextensionsv1.JSONSchemaProps { return apiextensionsv1.JSONSchemaProps{Type: "string"} }

func enumOf(vals ...string) []apiextensionsv1.JSON {
	var out []apiextensionsv1.JSON
	for _, v := range vals {
		out = append(out, apiextensionsv1.JSON{Raw: []byte(`"` + v + `"`)})
	}
	return out
}

// core-provider#534. Each case is a served spec schema, the one generated today, and whether the
// refresh is allowed. Every loosening has a tightening twin, so a comparator that answered "looser"
// to everything (or to nothing) fails half the table.
func TestCompareSpec(t *testing.T) {
	untypedOld := apiextensionsv1.JSONSchemaProps{Type: "object", XPreserveUnknownFields: ptrTrue()}
	untypedNew := apiextensionsv1.JSONSchemaProps{XPreserveUnknownFields: ptrTrue()}
	list := func(items apiextensionsv1.JSONSchemaProps) apiextensionsv1.JSONSchemaProps {
		return apiextensionsv1.JSONSchemaProps{Type: "array", Items: &apiextensionsv1.JSONSchemaPropsOrArray{Schema: &items}}
	}
	withMax := func(n int64) apiextensionsv1.JSONSchemaProps { s := str(); s.MaxLength = i64(n); return s }
	withMin := func(n float64) apiextensionsv1.JSONSchemaProps {
		return apiextensionsv1.JSONSchemaProps{Type: "integer", Minimum: f(n)}
	}
	withEnum := func(v ...string) apiextensionsv1.JSONSchemaProps { s := str(); s.Enum = enumOf(v...); return s }
	req := func(r ...string) apiextensionsv1.JSONSchemaProps {
		o := obj(map[string]apiextensionsv1.JSONSchemaProps{"a": str(), "b": str()})
		o.Required = r
		return o
	}
	withDefault := func() apiextensionsv1.JSONSchemaProps {
		s := str()
		s.Default = &apiextensionsv1.JSON{Raw: []byte(`"x"`)}
		return s
	}
	cases := []struct {
		name       string
		served     apiextensionsv1.JSONSchemaProps
		generated  apiextensionsv1.JSONSchemaProps
		wantEqual  bool
		wantLooser bool
		wantWhy    string // a substring of the reported difference, when not looser
	}{
		{"identical", obj(map[string]apiextensionsv1.JSONSchemaProps{"a": str()}), obj(map[string]apiextensionsv1.JSONSchemaProps{"a": str()}), true, false, ""},
		{"plumbing#186: untyped items lose type object", obj(map[string]apiextensionsv1.JSONSchemaProps{"l": list(untypedOld)}), obj(map[string]apiextensionsv1.JSONSchemaProps{"l": list(untypedNew)}), false, true, ""},
		{"twin: untyped items GAIN type object", obj(map[string]apiextensionsv1.JSONSchemaProps{"l": list(untypedNew)}), obj(map[string]apiextensionsv1.JSONSchemaProps{"l": list(untypedOld)}), false, false, ".type: added"},
		{"type removed without preserve-unknown", obj(map[string]apiextensionsv1.JSONSchemaProps{"a": str()}), obj(map[string]apiextensionsv1.JSONSchemaProps{"a": {}}), false, false, ".type: removed"},
		{"type changed", obj(map[string]apiextensionsv1.JSONSchemaProps{"a": str()}), obj(map[string]apiextensionsv1.JSONSchemaProps{"a": {Type: "integer"}}), false, false, ".type: changed"},
		{"maxLength dropped", obj(map[string]apiextensionsv1.JSONSchemaProps{"a": withMax(10)}), obj(map[string]apiextensionsv1.JSONSchemaProps{"a": str()}), false, true, ""},
		{"twin: maxLength gained", obj(map[string]apiextensionsv1.JSONSchemaProps{"a": str()}), obj(map[string]apiextensionsv1.JSONSchemaProps{"a": withMax(10)}), false, false, "maxLength: added"},
		{"maxLength raised", obj(map[string]apiextensionsv1.JSONSchemaProps{"a": withMax(10)}), obj(map[string]apiextensionsv1.JSONSchemaProps{"a": withMax(20)}), false, true, ""},
		{"twin: maxLength lowered", obj(map[string]apiextensionsv1.JSONSchemaProps{"a": withMax(20)}), obj(map[string]apiextensionsv1.JSONSchemaProps{"a": withMax(10)}), false, false, "maxLength: changed"},
		{"minimum lowered", obj(map[string]apiextensionsv1.JSONSchemaProps{"a": withMin(5)}), obj(map[string]apiextensionsv1.JSONSchemaProps{"a": withMin(1)}), false, true, ""},
		{"twin: minimum raised", obj(map[string]apiextensionsv1.JSONSchemaProps{"a": withMin(1)}), obj(map[string]apiextensionsv1.JSONSchemaProps{"a": withMin(5)}), false, false, "minimum: changed"},
		{"enum widened", obj(map[string]apiextensionsv1.JSONSchemaProps{"a": withEnum("x")}), obj(map[string]apiextensionsv1.JSONSchemaProps{"a": withEnum("x", "y")}), false, true, ""},
		{"twin: enum narrowed", obj(map[string]apiextensionsv1.JSONSchemaProps{"a": withEnum("x", "y")}), obj(map[string]apiextensionsv1.JSONSchemaProps{"a": withEnum("x")}), false, false, "enum: changed"},
		{"required shrunk", obj(map[string]apiextensionsv1.JSONSchemaProps{"o": req("a", "b")}), obj(map[string]apiextensionsv1.JSONSchemaProps{"o": req("a")}), false, true, ""},
		{"twin: required grown", obj(map[string]apiextensionsv1.JSONSchemaProps{"o": req("a")}), obj(map[string]apiextensionsv1.JSONSchemaProps{"o": req("a", "b")}), false, false, "required: changed"},
		// #686: a defaults-only difference is safe to write (see onlyDefaultsDiffer).
		{"default added is defaults-only, safe (#686)", obj(map[string]apiextensionsv1.JSONSchemaProps{"a": str()}), obj(map[string]apiextensionsv1.JSONSchemaProps{"a": withDefault()}), false, true, ""},
		{"property no longer declared", obj(map[string]apiextensionsv1.JSONSchemaProps{"a": str(), "b": str()}), obj(map[string]apiextensionsv1.JSONSchemaProps{"a": str()}), false, false, ".b: no longer declared"},
		{"property newly declared", obj(map[string]apiextensionsv1.JSONSchemaProps{"a": str()}), obj(map[string]apiextensionsv1.JSONSchemaProps{"a": str(), "b": str()}), false, false, ".b: newly declared"},
		{"a validation rule added", obj(map[string]apiextensionsv1.JSONSchemaProps{"a": str()}), obj(map[string]apiextensionsv1.JSONSchemaProps{"a": {Type: "string", XValidations: apiextensionsv1.ValidationRules{{Rule: "self != ''"}}}}), false, false, "x-kubernetes-validations: added"},
		{"description only", obj(map[string]apiextensionsv1.JSONSchemaProps{"a": str()}), obj(map[string]apiextensionsv1.JSONSchemaProps{"a": {Type: "string", Description: "now documented"}}), false, true, ""},
		// installer's question: `required` moving from a parent to a child is looser at the parent
		// and TIGHTER at the child. The walk recurses into shared properties, so the child's new
		// `required` is caught and the whole move is not looser.
		{"required moved from parent to child is NOT looser",
			apiextensionsv1.JSONSchemaProps{Type: "object", Required: []string{"o"}, Properties: map[string]apiextensionsv1.JSONSchemaProps{"o": obj(map[string]apiextensionsv1.JSONSchemaProps{"a": str()})}},
			apiextensionsv1.JSONSchemaProps{Type: "object", Properties: map[string]apiextensionsv1.JSONSchemaProps{"o": {Type: "object", Required: []string{"a"}, Properties: map[string]apiextensionsv1.JSONSchemaProps{"a": str()}}}},
			false, false, "spec.properties.o.required: added"},
		{"one loosening and one tightening is NOT looser", obj(map[string]apiextensionsv1.JSONSchemaProps{"a": withMax(10), "b": str()}), obj(map[string]apiextensionsv1.JSONSchemaProps{"a": str(), "b": withMax(5)}), false, false, "maxLength: added"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, err := CompareSpec(specCRD("v0-1-0", c.served), specCRD("v0-1-0", c.generated))
			if err != nil {
				t.Fatal(err)
			}
			if d.Equal != c.wantEqual || d.Looser() != c.wantLooser {
				t.Fatalf("equal=%v looser=%v, want equal=%v looser=%v (differences: %v)", d.Equal, d.Looser(), c.wantEqual, c.wantLooser, d.NotLooser)
			}
			if c.wantWhy != "" && !strings.Contains(strings.Join(d.NotLooser, " | "), c.wantWhy) {
				t.Fatalf("the reported differences %v do not name %q", d.NotLooser, c.wantWhy)
			}
		})
	}
}

func TestCompareSpecAVersionNotServedIsEqual(t *testing.T) {
	d, err := CompareSpec(specCRD("v0-1-0", str()), specCRD("v0-2-0", obj(nil)))
	if err != nil {
		t.Fatal(err)
	}
	if !d.Equal {
		t.Fatalf("a version the live CRD does not serve has nothing to refresh, got %+v", d)
	}
}

func TestReplaceSpecWritesOnlyTheMatchingVersionsSpec(t *testing.T) {
	live := specCRD("v0-1-0", obj(map[string]apiextensionsv1.JSONSchemaProps{"a": str()}))
	live.Spec.Versions = append(live.Spec.Versions, *specCRD("v0-0-9", obj(map[string]apiextensionsv1.JSONSchemaProps{"old": str()})).Spec.Versions[0].DeepCopy())
	status := live.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["status"]
	gen := specCRD("v0-1-0", obj(map[string]apiextensionsv1.JSONSchemaProps{"a": {XPreserveUnknownFields: ptrTrue()}}))
	if !ReplaceSpec(live, gen) {
		t.Fatal("ReplaceSpec did not write the served version")
	}
	if got := live.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties["a"].Type; got != "" {
		t.Errorf("v0-1-0's spec was not replaced: a.type=%q", got)
	}
	if _, ok := live.Spec.Versions[1].Schema.OpenAPIV3Schema.Properties["spec"].Properties["old"]; !ok {
		t.Error("another version's spec schema was touched")
	}
	if got := live.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["status"]; got.Type != status.Type || got.XPreserveUnknownFields == nil {
		t.Error("the status schema was touched")
	}
}

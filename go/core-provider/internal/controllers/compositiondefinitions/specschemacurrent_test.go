package compositiondefinitions

import (
	"strings"
	"testing"

	crdutils "github.com/krateo-platformops/core-provider/internal/tools/crd/generation"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// core-provider#534: the three verdicts, each with its status, so a consumer can tell "current",
// "being written" and "kept until the next chart version" apart.
func TestSpecSchemaCurrentCondition(t *testing.T) {
	cases := []struct {
		name   string
		d      crdutils.SpecDrift
		status metav1.ConditionStatus
		reason string
		names  string
	}{
		{"equal", crdutils.SpecDrift{Version: "v0-1-0", Equal: true}, metav1.ConditionTrue, "Current", "v0-1-0"},
		{"looser", crdutils.SpecDrift{Version: "v0-1-0"}, metav1.ConditionFalse, "LooserSchemaPending", "being written"},
		{"not looser", crdutils.SpecDrift{Version: "v0-1-0", NotLooser: []string{"spec.properties.a.default: added, and not a known loosening"}},
			metav1.ConditionFalse, "GeneratedSchemaNotLooser", "spec.properties.a.default"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := specSchemaCurrentCondition(c.d)
			if string(got.Type) != "SpecSchemaCurrent" || got.Status != c.status || string(got.Reason) != c.reason {
				t.Fatalf("got %s=%s/%s, want SpecSchemaCurrent=%s/%s", got.Type, got.Status, got.Reason, c.status, c.reason)
			}
			if !strings.Contains(got.Message, c.names) {
				t.Fatalf("message %q does not name %q", got.Message, c.names)
			}
		})
	}
}

// The decision Observe acts on. Only a provably looser render routes to Update; a tighter one and
// an identical one do not, so a mutant routing on "differs" or never routing fails here.
func TestSpecDriftDecision(t *testing.T) {
	preserve := true
	crdWith := func(items apiextensionsv1.JSONSchemaProps) *apiextensionsv1.CustomResourceDefinition {
		return &apiextensionsv1.CustomResourceDefinition{Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{Name: "v0-2-33",
				Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
					Type: "object", Properties: map[string]apiextensionsv1.JSONSchemaProps{
						"spec": {Type: "object", Properties: map[string]apiextensionsv1.JSONSchemaProps{
							"l": {Type: "array", Items: &apiextensionsv1.JSONSchemaPropsOrArray{Schema: &items}}}},
						"status": {Type: "object"}}}}}}}}
	}
	narrowed := apiextensionsv1.JSONSchemaProps{Type: "object", XPreserveUnknownFields: &preserve}
	anyValue := apiextensionsv1.JSONSchemaProps{XPreserveUnknownFields: &preserve}
	for _, c := range []struct {
		name          string
		live, gen     apiextensionsv1.JSONSchemaProps
		wantRefresh   bool
		wantStatus    metav1.ConditionStatus
		wantReasonSub string
	}{
		{"looser routes to Update", narrowed, anyValue, true, metav1.ConditionFalse, "LooserSchemaPending"},
		{"tighter is reported, not written", anyValue, narrowed, false, metav1.ConditionFalse, "GeneratedSchemaNotLooser"},
		{"identical is current", anyValue, anyValue, false, metav1.ConditionTrue, "Current"},
	} {
		t.Run(c.name, func(t *testing.T) {
			cond, refresh, _, err := specDriftDecision(crdWith(c.live), crdWith(c.gen))
			if err != nil {
				t.Fatal(err)
			}
			if refresh != c.wantRefresh || cond.Status != c.wantStatus || !strings.Contains(string(cond.Reason), c.wantReasonSub) {
				t.Fatalf("refresh=%v %s/%s, want refresh=%v %s/%s", refresh, cond.Status, cond.Reason, c.wantRefresh, c.wantStatus, c.wantReasonSub)
			}
		})
	}
}

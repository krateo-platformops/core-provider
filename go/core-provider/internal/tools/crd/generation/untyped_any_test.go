package generation

import (
	"testing"

	apiext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// plumbing#186 through core-provider's own generator (plumbing >= v1.81.0). trust-manager v0.20.0's
// app.targetNamespaces is `{"type":"array","items":{}}` behind $ref/$defs. Before v1.81.0 the
// generated CRD typed the items `object`, and the apiserver refused installer's list of namespace
// names ("must be of type object"). This pins the fix at the layer that generates the CRDs
// core-provider applies, so a plumbing downgrade or regression fails here, not on a cluster.
func TestGeneratedCRDAdmitsAnyValueAtAnUntypedNode(t *testing.T) {
	const trustManagerShape = `{"type":"object","properties":{"app":{"$ref":"#/$defs/helm-values.app"}},
	  "$defs":{"helm-values.app":{"type":"object","properties":{"targetNamespaces":{"$ref":"#/$defs/helm-values.app.targetNamespaces"}}},
	           "helm-values.app.targetNamespaces":{"type":"array","items":{}}}}`
	g, err := GenerateCRDWithWarnings([]byte(trustManagerShape), schema.GroupVersionKind{Group: "composition.krateo.io", Version: "v0-20-0", Kind: "TrustManager"})
	if err != nil {
		t.Fatal(err)
	}
	var internal apiext.JSONSchemaProps
	if err := apiextv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(g.CRD.Spec.Versions[0].Schema.OpenAPIV3Schema, &internal, nil); err != nil {
		t.Fatal(err)
	}
	v, _, err := validation.NewSchemaValidator(&internal)
	if err != nil {
		t.Fatal(err)
	}
	body := func(items ...any) map[string]any {
		return map[string]any{"spec": map[string]any{"app": map[string]any{"targetNamespaces": items}}}
	}
	if errs := validation.ValidateCustomResource(field.NewPath(""), body("cert-manager", "krateo-system"), v); len(errs) != 0 {
		t.Errorf("a list of namespace names was refused: %v", errs)
	}
	if errs := validation.ValidateCustomResource(field.NewPath(""), body(map[string]any{"x": "y"}), v); len(errs) != 0 {
		t.Errorf("an object item, admitted before the fix, is now refused: %v", errs)
	}
	// Control: the validator judges. A declared type still refuses a wrong kind.
	typed, err := GenerateCRDWithWarnings([]byte(`{"type":"object","properties":{"l":{"type":"array","items":{"type":"string"}}}}`),
		schema.GroupVersionKind{Group: "composition.krateo.io", Version: "v0-20-0", Kind: "TrustManagerControl"})
	if err != nil {
		t.Fatal(err)
	}
	var ti apiext.JSONSchemaProps
	if err := apiextv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(typed.CRD.Spec.Versions[0].Schema.OpenAPIV3Schema, &ti, nil); err != nil {
		t.Fatal(err)
	}
	tv, _, _ := validation.NewSchemaValidator(&ti)
	if errs := validation.ValidateCustomResource(field.NewPath(""), map[string]any{"spec": map[string]any{"l": []any{int64(1)}}}, tv); len(errs) == 0 {
		t.Error("an integer was admitted into a string list: the validator is not judging")
	}
}

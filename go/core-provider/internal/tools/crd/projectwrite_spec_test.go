package crd

import (
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func pwCRD(version string, items apiextensionsv1.JSONSchemaProps) *apiextensionsv1.CustomResourceDefinition {
	preserve := true
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "probes.composition.krateo.io"},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "composition.krateo.io",
			Names: apiextensionsv1.CustomResourceDefinitionNames{Kind: "Probe", Plural: "probes", ListKind: "ProbeList", Singular: "probe"},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name: version, Served: true, Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
					Type: "object",
					Properties: map[string]apiextensionsv1.JSONSchemaProps{
						"spec": {Type: "object", Properties: map[string]apiextensionsv1.JSONSchemaProps{
							"targetNamespaces": {Type: "array", Items: &apiextensionsv1.JSONSchemaPropsOrArray{Schema: &items}},
						}},
						"status": {Type: "object", XPreserveUnknownFields: &preserve},
					},
				}},
			}},
		},
	}
}

func itemsOf(t *testing.T, crd *apiextensionsv1.CustomResourceDefinition, version string) apiextensionsv1.JSONSchemaProps {
	t.Helper()
	for _, v := range crd.Spec.Versions {
		if v.Name == version {
			return *v.Schema.OpenAPIV3Schema.Properties["spec"].Properties["targetNamespaces"].Items.Schema
		}
	}
	t.Fatalf("version %s not served", version)
	return apiextensionsv1.JSONSchemaProps{}
}

// core-provider#534, through the function that computes every CRD write. A served version takes a
// PROVABLY looser generated spec schema (the plumbing#186 shape that left six definitions narrowed on
// krateo-057), and keeps its own when the generated one is tighter.
func TestProjectWriteRefreshesAServedVersionsSpecOnlyWhenLooser(t *testing.T) {
	preserve := true
	narrowed := apiextensionsv1.JSONSchemaProps{Type: "object", XPreserveUnknownFields: &preserve}
	anyValue := apiextensionsv1.JSONSchemaProps{XPreserveUnknownFields: &preserve}

	t.Run("looser: the served version is refreshed", func(t *testing.T) {
		projected, mutated, err := ProjectWrite(pwCRD("v0-2-33", narrowed), pwCRD("v0-2-33", anyValue), nil)
		if err != nil {
			t.Fatal(err)
		}
		if !mutated {
			t.Fatal("a provably looser spec schema was not written: the write is skipped and the version stays narrowed")
		}
		if got := itemsOf(t, projected, "v0-2-33").Type; got != "" {
			t.Fatalf("the served version still types the items %q", got)
		}
	})
	t.Run("tighter: the served version is kept", func(t *testing.T) {
		live := pwCRD("v0-2-33", anyValue)
		projected, mutated, err := ProjectWrite(live, pwCRD("v0-2-33", narrowed), nil)
		if err != nil {
			t.Fatal(err)
		}
		if mutated {
			t.Fatal("a TIGHTER spec schema was written over the served one: stored values could start being refused")
		}
		if got := itemsOf(t, projected, "v0-2-33").Type; got != "" {
			t.Fatalf("the served schema changed to type %q", got)
		}
	})
	t.Run("control: identical render writes nothing", func(t *testing.T) {
		_, mutated, err := ProjectWrite(pwCRD("v0-2-33", anyValue), pwCRD("v0-2-33", anyValue), nil)
		if err != nil {
			t.Fatal(err)
		}
		if mutated {
			t.Fatal("an identical render produced a write")
		}
	})
}

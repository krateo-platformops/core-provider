package v1alpha1

import (
	"os"
	"strings"
	"testing"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"sigs.k8s.io/yaml"
)

// docs/design/cdc-pooling.md, step 1: core-provider runs no admission webhook, so the CRD schema is
// the only gate on spec.controller.pool. This runs the API server's own validator over the schema as
// GENERATED, read from where the API server reads it, so a lost marker or a moved field fails here.
func TestPoolSchemaAcceptsOnlyDNS1123Labels(t *testing.T) {
	raw, err := os.ReadFile("../../../crds/core.krateo.io_compositiondefinitions.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatal(err)
	}
	if len(crd.Spec.Versions) == 0 {
		t.Fatal("the generated CRD has no versions")
	}
	for _, v := range crd.Spec.Versions {
		root := v.Schema.OpenAPIV3Schema
		pool, ok := root.Properties["spec"].Properties["controller"].Properties["pool"]
		if !ok {
			t.Fatalf("version %s: spec.controller.pool is not in the generated schema", v.Name)
		}
		var internal apiextensions.JSONSchemaProps
		if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(&pool, &internal, nil); err != nil {
			t.Fatal(err)
		}
		validator, _, err := validation.NewSchemaValidator(&internal)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"frontend-pages", "a", "a1", "0", strings.Repeat("a", 63)} {
			if r := validator.Validate(name); !r.IsValid() {
				t.Errorf("version %s: %q must be accepted: %v", v.Name, name, r.Errors)
			}
		}
		for _, name := range []string{"", "Frontend", "-a", "a-", "a_b", "a.b", "a b", strings.Repeat("a", 64)} {
			if r := validator.Validate(name); r.IsValid() {
				t.Errorf("version %s: %q must be refused (not a DNS-1123 label)", v.Name, name)
			}
		}
	}
}

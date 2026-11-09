package main

import (
	"os"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// spec.apiRef.delegation must accept exactly what authn's ServiceAccount mapping accepts (#405):
// `allowed` required whenever delegation is set, and maxDepth an int32 in [1, 8]. If this CRD
// accepted more (maxDepth 0 or 9, a delegation with no `allowed`), the apiserver would reject the
// controller's MAPPING update instead, far from the definition that caused it, and the mapping
// would stay stale. Read from the generated CRD, so a dropped kubebuilder marker fails here.
func TestApiRefDelegationHasAuthnsBounds(t *testing.T) {
	raw, err := os.ReadFile("crds/core.krateo.io_compositiondefinitions.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatal(err)
	}
	if len(crd.Spec.Versions) == 0 {
		t.Fatal("control: the CRD has no versions, so nothing below would be checked")
	}
	for _, v := range crd.Spec.Versions {
		d, ok := v.Schema.OpenAPIV3Schema.Properties["spec"].Properties["apiRef"].Properties["delegation"]
		if !ok {
			t.Fatalf("%s: spec.apiRef.delegation is missing", v.Name)
		}
		if len(d.Required) != 1 || d.Required[0] != "allowed" {
			t.Errorf("%s: delegation.required = %v, want [allowed] (authn requires it whenever delegation is set)", v.Name, d.Required)
		}
		if a := d.Properties["allowed"]; a.Type != "boolean" {
			t.Errorf("%s: delegation.allowed type = %q, want boolean", v.Name, a.Type)
		}
		m := d.Properties["maxDepth"]
		if m.Type != "integer" || m.Format != "int32" || m.Minimum == nil || *m.Minimum != 1 || m.Maximum == nil || *m.Maximum != 8 {
			t.Errorf("%s: delegation.maxDepth = type %q format %q min %v max %v, want an int32 in [1, 8]", v.Name, m.Type, m.Format, m.Minimum, m.Maximum)
		}
	}
}

package v1alpha1

import (
	"os"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// core-provider#623: kubebuilder markers take literals, so status.migratedValues' maxItems cannot name
// MigratedValuesMaxInstances or MigratedValuesMaxFields. This pins the GENERATED schema to the
// constants the writer caps with: if either side moves alone, the writer could emit a list the
// apiserver refuses (status write fails) or the schema would admit more than the writer ever writes.
func TestMigratedValuesSchemaMaxItemsEqualTheWritersCaps(t *testing.T) {
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
	checked := 0
	for _, v := range crd.Spec.Versions {
		mv, ok := v.Schema.OpenAPIV3Schema.Properties["status"].Properties["migratedValues"]
		if !ok {
			continue // a version that predates the field (the vacuum hub carries no status schema)
		}
		checked++
		maxItems := func(path string, p apiextensionsv1.JSONSchemaProps) int64 {
			t.Helper()
			if p.MaxItems == nil {
				t.Fatalf("version %s: %s has no maxItems", v.Name, path)
			}
			return *p.MaxItems
		}
		if got := maxItems("refused", mv.Properties["refused"]); got != MigratedValuesMaxInstances {
			t.Errorf("version %s: refused maxItems=%d, MigratedValuesMaxInstances=%d", v.Name, got, MigratedValuesMaxInstances)
		}
		if got := maxItems("unchecked", mv.Properties["unchecked"]); got != MigratedValuesMaxInstances {
			t.Errorf("version %s: unchecked maxItems=%d, MigratedValuesMaxInstances=%d", v.Name, got, MigratedValuesMaxInstances)
		}
		fields := mv.Properties["refused"].Items.Schema.Properties["fields"]
		if got := maxItems("refused[].fields", fields); got != MigratedValuesMaxFields {
			t.Errorf("version %s: refused[].fields maxItems=%d, MigratedValuesMaxFields=%d", v.Name, got, MigratedValuesMaxFields)
		}
	}
	// CONTROL: the loop must have reached the field, or "no mismatch" proves nothing.
	if checked == 0 {
		t.Fatal("status.migratedValues is in no version of the generated CRD")
	}
}

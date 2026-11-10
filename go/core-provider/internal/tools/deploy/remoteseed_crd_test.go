package deploy

import (
	"testing"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextinstall "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/install"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/pruning"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"
)

// pruneWith runs the apiserver's own pruning (the step that drops unknown fields on write) on obj
// against the given CompositionDefinition CRD bytes, and returns what survives.
func pruneWith(t *testing.T, crdBytes []byte, obj map[string]any) map[string]any {
	t.Helper()
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(crdBytes, &crd); err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	apiextinstall.Install(scheme)
	var internal apiextensions.JSONSchemaProps
	if err := scheme.Convert(crd.Spec.Versions[0].Schema.OpenAPIV3Schema, &internal, nil); err != nil {
		t.Fatal(err)
	}
	s, err := structuralschema.NewStructural(&internal)
	if err != nil {
		t.Fatal(err)
	}
	out := runtime.DeepCopyJSON(obj)
	pruning.Prune(out, s, true)
	return out
}

// #435. The CompositionDefinition CRD core-provider projects onto a REMOTE target must keep the fields
// a definition declares, or the spoke's apiserver prunes them from the shadow definition. Measured
// against the apiserver's own pruning with the embedded CRD core-provider actually projects.
func TestRemoteSeedCRDKeepsTheDefinitionsFields(t *testing.T) {
	obj := map[string]any{
		"apiVersion": "core.krateo.io/v1alpha1", "kind": "CompositionDefinition",
		"metadata": map[string]any{"name": "d", "namespace": "ns"},
		"spec": map[string]any{
			"chart":         map[string]any{"url": "oci://example/chart", "version": "0.1.0"},
			"upgradePolicy": "Manual",
			"controller":    map[string]any{"workers": int64(3)},
			"apiRef":        map[string]any{"name": "ra", "namespace": "ns", "delegation": map[string]any{"allowed": true}},
		},
	}
	got := pruneWith(t, compositionDefinitionCRDBytes, obj)
	u := &unstructured.Unstructured{Object: got}
	for _, path := range [][]string{
		{"spec", "chart", "url"}, // control: an old field that was always there
		{"spec", "upgradePolicy"},
		{"spec", "controller", "workers"},
		{"spec", "apiRef", "delegation", "allowed"},
	} {
		if _, found, _ := unstructured.NestedFieldNoCopy(u.Object, path...); !found {
			t.Errorf("the remote-seed CRD prunes %v from a shadow definition", path)
		}
	}
}

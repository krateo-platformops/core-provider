package main

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/krateo-platformops/core-provider/internal/tools/servedkinds"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// #589. The startup check (servedkinds.Required) must cover every CRD core-provider-crds ships, at
// every version it serves, and nothing else: a CRD added to crds/ but not to Required would again
// fail late with controller-runtime's generic message, and a Kind in Required that no chart ships
// would refuse every install.
func TestServedKindsRequiredIsEveryGeneratedCRD(t *testing.T) {
	files, err := filepath.Glob("crds/*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no generated CRDs under crds/: the test would pass vacuously")
	}
	var generated []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var crd apiextensionsv1.CustomResourceDefinition
		if err := yaml.Unmarshal(b, &crd); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		for _, v := range crd.Spec.Versions {
			if v.Served {
				generated = append(generated, crd.Spec.Group+"/"+v.Name+", Kind="+crd.Spec.Names.Kind)
			}
		}
	}
	var required []string
	for _, gvk := range servedkinds.Required {
		required = append(required, gvk.String())
	}
	sort.Strings(generated)
	sort.Strings(required)
	if len(generated) != len(required) {
		t.Fatalf("crds/ serves %v; servedkinds.Required is %v", generated, required)
	}
	for i := range generated {
		if generated[i] != required[i] {
			t.Errorf("crds/ serves %v; servedkinds.Required is %v", generated, required)
			break
		}
	}
	t.Logf("compared %d served Kinds from %d generated CRDs", len(generated), len(files))
}

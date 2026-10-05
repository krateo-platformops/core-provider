//go:build crddiff

// Differential harness (NOT committed): generates a CRD from each REAL published chart schema in
// CRDDIFF_IN and writes the YAML to CRDDIFF_OUT. Run on two revisions and diff the outputs; any
// difference is a change in generated CRDs caused by the dependency move, which is the only
// question a build-and-test run cannot answer.
package generation

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"
)

func TestCRDDiffHarness(t *testing.T) {
	in, out := os.Getenv("CRDDIFF_IN"), os.Getenv("CRDDIFF_OUT")
	if in == "" || out == "" {
		t.Skip("CRDDIFF_IN/CRDDIFF_OUT unset")
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(in, "*.schema.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no schemas found in %s (err=%v)", in, err)
	}
	// Assert the harness FOUND something: a run over zero schemas would otherwise produce two
	// empty directories that diff clean, which is indistinguishable from "no change".
	t.Logf("schemas discovered: %d", len(files))

	generated := 0
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".schema.json")
		spec, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		gvk := schema.GroupVersionKind{Group: "composition.krateo.io", Version: "v1alpha1", Kind: "Demo"}
		crd, err := GenerateCRD(spec, gvk)
		if err != nil {
			// Record the failure as an artifact rather than aborting: a schema that fails to
			// generate on BOTH revisions is prior art, not a regression. The diff decides.
			_ = os.WriteFile(filepath.Join(out, name+".ERROR"), []byte(err.Error()), 0o644)
			t.Logf("%s: generate error recorded: %v", name, err)
			continue
		}
		b, err := yaml.Marshal(crd)
		if err != nil {
			t.Fatalf("%s: marshal: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(out, name+".crd.yaml"), b, 0o644); err != nil {
			t.Fatal(err)
		}
		generated++
	}
	if generated == 0 {
		t.Fatalf("harness generated 0 CRDs from %d schemas; two empty output dirs would diff clean for the wrong reason", len(files))
	}
	t.Logf("CRDs generated: %d/%d", generated, len(files))
}

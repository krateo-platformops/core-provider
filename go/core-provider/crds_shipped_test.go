package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// The CRDs users install come from the core-provider-crds chart, a hand-kept copy of what
// `make generate` writes to crds/. A field added to the API but not copied there is pruned by the API
// server on every install, silently: the field "exists" in the code and in tests and never in a
// cluster. This requires the chart to ship exactly the generated CRDs, every one of them.
func TestShippedCRDChartIsTheGeneratedCRDs(t *testing.T) {
	generated, err := filepath.Glob("crds/*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(generated) == 0 {
		t.Fatal("no generated CRDs under crds/: the test would pass vacuously")
	}
	const chart = "../../helm/core-provider-crds/templates"
	shipped, err := filepath.Glob(filepath.Join(chart, "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(shipped) != len(generated) {
		t.Errorf("the chart ships %d CRD files, crds/ has %d generated", len(shipped), len(generated))
	}
	for _, g := range generated {
		want, err := os.ReadFile(g)
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(chart, filepath.Base(g)))
		if err != nil {
			t.Errorf("%s is generated but not shipped: %v", filepath.Base(g), err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s in the chart differs from the generated one; copy crds/%s into %s",
				filepath.Base(g), filepath.Base(g), chart)
		}
	}
	t.Logf("compared %d generated CRDs against the chart", len(generated))
}

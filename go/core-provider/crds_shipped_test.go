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

// #435. core-provider projects the CompositionDefinition CRD onto REMOTE targets from an embedded
// copy (internal/tools/deploy/assets, //go:embed in remoteseed.go). It was hand-kept, had not changed
// since the monorepo fold, and differed from crds/ by 227 diff lines, so a spoke's shadow definition
// lost spec.controller, upgradePolicy, claims and apiRef.delegation. make generate now writes it; this
// requires it to stay byte-identical to the generated CRD.
func TestRemoteSeedCRDIsTheGeneratedCRD(t *testing.T) {
	want, err := os.ReadFile("crds/core.krateo.io_compositiondefinitions.yaml")
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile("internal/tools/deploy/assets/compositiondefinitions-crd.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(want) == 0 {
		t.Fatal("the generated CRD is empty: the comparison would pass vacuously")
	}
	if !bytes.Equal(got, want) {
		t.Error("the CRD projected onto remote targets differs from crds/core.krateo.io_compositiondefinitions.yaml; run make generate")
	}
}

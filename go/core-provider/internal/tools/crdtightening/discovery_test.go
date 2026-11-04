package crdtightening

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// shippedCRDDir is the canonical source, per the Makefile ("Regenerate CRDs into ./crds"). The
// relative path is from this package to the module root.
const shippedCRDDir = "../../../crds"

// Discovery over the REAL shipped files, not a fixture. A fixture would prove the parser works on
// input I wrote, which is the one input that cannot surprise me -- and the whole value of this gate
// is that it reads what the repo actually ships.
func TestDiscoverCRDs_ReadsWhatTheRepoShips(t *testing.T) {
	crds, err := DiscoverCRDs(shippedCRDDir)
	if err != nil {
		t.Fatalf("DiscoverCRDs(%s): %v", shippedCRDDir, err)
	}

	// THE DENOMINATOR, asserted rather than printed. Zero CRDs or zero versions would make every
	// comparison downstream report "no tightenings" over an empty set.
	if len(crds) == 0 {
		t.Fatal("discovered 0 CRDs from the shipped directory")
	}
	versions := 0
	for _, c := range crds {
		if c.Name == "" || c.Group == "" || c.Kind == "" {
			t.Errorf("a discovered CRD is missing identity: name=%q group=%q kind=%q (from %s). "+
				"A finding that cannot name the CRD it is about is not actionable",
				c.Name, c.Group, c.Kind, c.Source)
		}
		versions += len(c.Schemas)
		for v, s := range c.Schemas {
			if len(s) == 0 {
				t.Errorf("%s/%s has an EMPTY schema map. Comparing an empty schema against "+
					"anything finds no tightenings, so this would pass while measuring nothing",
					c.Name, v)
			}
		}
	}
	if versions == 0 {
		t.Fatalf("discovered %d CRD(s) but 0 versions with schemas", len(crds))
	}
	t.Logf("discovered %d CRD(s), %d version(s) with schemas", len(crds), versions)
}

// A ZERO IS AN ERROR, NOT AN EMPTY RESULT. Both arms, because the two failures reach the same
// decorative outcome by different routes: a wrong path, and a right path with nothing generated
// into it.
func TestDiscoverCRDs_RefusesToFindNothing(t *testing.T) {
	t.Run("an empty directory errors", func(t *testing.T) {
		dir := t.TempDir()
		if _, err := DiscoverCRDs(dir); err == nil {
			t.Error("DiscoverCRDs over an empty directory returned no error. Discovery finding " +
				"nothing means the path is wrong or the generator did not run, and a comparison " +
				"over an empty set reports 'no tightenings' for the wrong reason")
		}
	})

	t.Run("a missing directory errors", func(t *testing.T) {
		if _, err := DiscoverCRDs(filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
			t.Error("DiscoverCRDs over a missing directory returned no error")
		}
	})

	t.Run("yaml files that are not CRDs error", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "a.yaml"),
			[]byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := DiscoverCRDs(dir)
		if err == nil {
			t.Error("a directory of yaml files with no CRD in them returned no error, so a gate " +
				"pointed at the wrong directory would run clean")
		} else if !strings.Contains(err.Error(), "CustomResourceDefinition") {
			t.Errorf("the error should say no CRD was found, got: %v", err)
		}
	})
}

func crd(name string, schemas map[string]map[string]any) ShippedCRD {
	return ShippedCRD{Source: "test", Name: name, Group: "g", Kind: "K", Schemas: schemas}
}

func schema(props map[string]any) map[string]any {
	return map[string]any{"type": "object", "properties": props}
}

// Compare's arms, by input state. The two that matter are both "no findings": one because nothing
// got stricter, and one because nothing was compared. They must not be the same outcome.
func TestCompare_DistinguishesCleanFromMeasuredNothing(t *testing.T) {
	t.Run("a real tightening is reported", func(t *testing.T) {
		base := []ShippedCRD{crd("x.g", map[string]map[string]any{
			"v1": schema(map[string]any{"a": map[string]any{"type": "string"}}),
		})}
		// The new one adds `required`, which admits strictly less.
		newer := map[string]any{"type": "object", "required": []any{"a"},
			"properties": map[string]any{"a": map[string]any{"type": "string"}}}
		ship := []ShippedCRD{crd("x.g", map[string]map[string]any{"v1": newer})}

		rep, err := Compare(ship, base)
		if err != nil {
			t.Fatalf("Compare: %v", err)
		}
		if rep.Pairs != 1 {
			t.Fatalf("Pairs = %d, want 1 — the comparison did not run over the pair", rep.Pairs)
		}
		if len(rep.Findings) == 0 {
			t.Error("a newly-required property was not reported as a tightening")
		}
		if rep.Clean() {
			t.Error("Clean() is true with findings present")
		}
	})

	t.Run("no tightening over a real pair is CLEAN", func(t *testing.T) {
		s := schema(map[string]any{"a": map[string]any{"type": "string"}})
		rep, err := Compare(
			[]ShippedCRD{crd("x.g", map[string]map[string]any{"v1": s})},
			[]ShippedCRD{crd("x.g", map[string]map[string]any{"v1": s})},
		)
		if err != nil {
			t.Fatalf("Compare: %v", err)
		}
		if !rep.Clean() {
			t.Errorf("an identical schema over 1 pair is not clean: %s", rep.Summary())
		}
	})

	// THE ARM THIS WHOLE DESIGN EXISTS FOR. Every shipped version is new, so nothing was compared.
	// Findings is empty and that empty slice says NOTHING about whether anything got stricter.
	t.Run("zero pairs is an ERROR and not clean", func(t *testing.T) {
		rep, err := Compare(
			[]ShippedCRD{crd("x.g", map[string]map[string]any{"v2": schema(nil)})},
			[]ShippedCRD{crd("x.g", map[string]map[string]any{"v1": schema(nil)})},
		)
		if err == nil {
			t.Fatal("Compare returned no error having compared 0 pairs. An empty Findings slice " +
				"over an empty comparison is indistinguishable from a clean run, which is the " +
				"single most likely way for this gate to become decorative")
		}
		if rep.Clean() {
			t.Error("Clean() is true with Pairs == 0")
		}
		if len(rep.NewVersions) != 1 || len(rep.DroppedVersions) != 1 {
			t.Errorf("new=%v dropped=%v — both should be reported so the zero is explainable",
				rep.NewVersions, rep.DroppedVersions)
		}
	})

	// A dropped version is NOT a finding: core-provider prunes stale served versions deliberately
	// (#207). Asserted beside a real pair so the comparison is not vacuous.
	t.Run("a dropped version is reported but is not a finding", func(t *testing.T) {
		s := schema(map[string]any{"a": map[string]any{"type": "string"}})
		rep, err := Compare(
			[]ShippedCRD{crd("x.g", map[string]map[string]any{"v2": s})},
			[]ShippedCRD{crd("x.g", map[string]map[string]any{"v1": s, "v2": s})},
		)
		if err != nil {
			t.Fatalf("Compare: %v", err)
		}
		if rep.Pairs != 1 {
			t.Fatalf("Pairs = %d, want 1", rep.Pairs)
		}
		if len(rep.Findings) != 0 {
			t.Errorf("pruning a served version was reported as a tightening: %v. core-provider "+
				"prunes them on purpose, so this would fail the gate on correct behaviour",
				rep.Findings)
		}
		if len(rep.DroppedVersions) != 1 {
			t.Errorf("the dropped version was not reported: %s", rep.Summary())
		}
	})
}

// A bare `---` inside a block scalar is CONTENT. Splitting on the string rather than on a line that
// is exactly a separator cuts a schema in half, and the halves fail to parse as a CRD — which this
// package would then report as "no CRD in the baseline", a false negative that reads exactly like
// the truth.
func TestSplitYAMLDocuments_DoesNotCutInsideABlockScalar(t *testing.T) {
	raw := []byte("apiVersion: v1\nkind: ConfigMap\ndata:\n  note: |\n    ---\n    still the same doc\n")
	if got := len(splitYAMLDocuments(raw)); got != 1 {
		t.Errorf("split into %d documents, want 1: an indented --- inside a block scalar is "+
			"content, not a separator", got)
	}

	// CONTROL: a real separator must still split, or the function above passes by never splitting.
	two := []byte("kind: A\n---\nkind: B\n")
	if got := len(splitYAMLDocuments(two)); got != 2 {
		t.Errorf("split into %d documents, want 2 — a line that IS a separator must split, "+
			"otherwise the test above proves only that nothing ever splits", got)
	}
}

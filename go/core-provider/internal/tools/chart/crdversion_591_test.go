package chart

import (
	"fmt"
	"strings"
	"testing"

	"github.com/krateo-platformops/plumbing/crdgen"
)

// #591: core-provider's chart.ChartGroupVersionKind, chartfs.GroupVersionKind and adoption's spec clause derive a chart version's CRD version with crdgen.NormalizeVersionName, the call crdgen
// itself makes on the version it is handed, so every derived name is the version the CRD serves.
// oldCRDVersion is the derivation they used before; it is the control arm, never the thing under
// test. The rows are the same in core-provider's internal/tools/chart and the cdc's
// internal/tools/archive: the two modules must name the same version identically.
func oldCRDVersion(v string) string { return "v" + strings.ReplaceAll(v, ".", "-") }

// For every plain version the new derivation is the old one, so no definition on a plain version is
// re-pointed at a different CRD version by the fix.
func TestNormalizeVersionName_591_PlainVersionIdentity(t *testing.T) {
	vs := []string{"0.0.0", "0.0.1", "1.0.0", "0.2.45", "2.13.118", "10.20.30", "1.0", "1", "123.456.7890"}
	for a := 0; a <= 12; a++ {
		for b := 0; b <= 12; b++ {
			for c := 0; c <= 12; c++ {
				vs = append(vs, fmt.Sprintf("%d.%d.%d", a, b, c))
			}
		}
	}
	for _, v := range vs {
		if got, want := crdgen.NormalizeVersionName(v), oldCRDVersion(v); got != want {
			t.Errorf("NormalizeVersionName(%q) = %q, the old derivation gave %q: a working definition would be re-pointed", v, got, want)
		}
	}
	if len(vs) < 2000 {
		t.Fatalf("only %d versions compared", len(vs))
	}
	t.Logf("%d plain versions: NormalizeVersionName == old derivation", len(vs))
}

// The versions #591 measured, and the one deliberate CHANGE. The old derivation named a version the
// CRD does not serve; the new one names the served one (asserted against a real apiserver in
// core-provider's TestEnvtest_591_StatusNamesServedVersion). 1.0.0 is the control: both agree on it.
//
// "v1.0.0" is a valid Helm chart version (helm's chart.Metadata.Validate accepts it). The old
// derivation gave "vv1-0-0", and crdgen served that; NormalizeVersionName gives "v1-0-0". So a
// definition whose Chart.yaml version starts with "v" is served under a NEW CRD version name after
// the fix. Asserted, so the change is on record rather than discovered: it is the one population
// whose names move (0 on krateo-057, whose 79 definitions are all [0-9.]).
func TestNormalizeVersionName_591_NonPlainVersions(t *testing.T) {
	for _, c := range []struct{ chart, want, old string }{
		{"1.0.0", "v1-0-0", "v1-0-0"}, // control
		{"1.0.0-RC1", "v1-0-0-rc1", "v1-0-0-RC1"},
		{"1.0.0+build.5", "v1-0-0-build-5", "v1-0-0+build-5"},
		{"2.0.0-Beta.2", "v2-0-0-beta-2", "v2-0-0-Beta-2"},
		{"1.0.0--rc", "v1-0-0-rc", "v1-0-0--rc"}, // valid semver; crdgen collapses the run
		{"v1.0.0", "v1-0-0", "vv1-0-0"},          // the CHANGE: served was vv1-0-0
	} {
		if got := oldCRDVersion(c.chart); got != c.old {
			t.Fatalf("control arm: old(%q) = %q, want %q", c.chart, got, c.old)
		}
		if got := crdgen.NormalizeVersionName(c.chart); got != c.want {
			t.Errorf("NormalizeVersionName(%q) = %q, want the served %q (old derivation: %q)", c.chart, got, c.want, c.old)
		}
	}
}

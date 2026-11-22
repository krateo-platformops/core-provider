package chart

import (
	"fmt"
	"strings"
	"testing"

	"github.com/krateo-platformops/plumbing/versionname"
)

// #591: core-provider's chart.ChartGroupVersionKind, chartfs.GroupVersionKind and adoption's spec clause derive a chart version's CRD version with versionname.ForChart, which is
// Normalize("v" + chartVersion). The rows are the same in core-provider's internal/tools/chart and the
// cdc's internal/tools/archive: the two modules must name a version identically.
//
// servedBefore is the name the CRD was SERVED under before the fix: core-provider handed crdgen
// gvk.Version = fmt.Sprintf("v%s", strings.ReplaceAll(v, ".", "-")) and crdgen normalized it
// (crdgen's normalizeVersion in plumbing v1.102.0 is versionname.Normalize line for line; v1.104.0's
// crdgen delegates to it). statusBefore is the same expression UN-normalized: what status.apiVersion
// and the five derivation sites used. Both are control arms, never the thing under test.
func servedBefore(v string) string {
	return versionname.Normalize(fmt.Sprintf("v%s", strings.ReplaceAll(v, ".", "-")))
}

func statusBefore(v string) string { return fmt.Sprintf("v%s", strings.ReplaceAll(v, ".", "-")) }

func shapes591() (plain, other []string) {
	plain = []string{"0.0.0", "0.0.1", "1.0.0", "0.2.45", "2.13.118", "10.20.30", "1.0", "1", "123.456.7890"}
	for a := 0; a <= 12; a++ {
		for b := 0; b <= 12; b++ {
			for c := 0; c <= 12; c++ {
				plain = append(plain, fmt.Sprintf("%d.%d.%d", a, b, c))
			}
		}
	}
	other = []string{
		"v1.0.0", "V1.0.0", "v0.2.45", "V2.13.118", // v/V-prefixed
		"1.0.0-RC1", "1.0.0+build.5", "2.0.0-Beta.2", "1.0.0--rc", "1.0.0-rc.1", "1.0.0-alpha+001", "1.0.0_x", // non-plain
	}
	return plain, other
}

// Every definition keeps the CRD version it is served under today, whatever its version's shape
// (Diego, 2026-11-22: preserve served names). Only status and the derived names move, onto it.
func TestForChart_591_PreservesTheServedName(t *testing.T) {
	plain, other := shapes591()
	n := 0
	for _, v := range append(append([]string{}, plain...), other...) {
		if got, want := versionname.ForChart(v), servedBefore(v); got != want {
			t.Errorf("ForChart(%q) = %q, but the CRD has served it as %q: the definition would be re-pointed", v, got, want)
		}
		n++
	}
	if n < 2000 || len(other) < 10 {
		t.Fatalf("only %d shapes compared (%d non-plain)", n, len(other))
	}
	t.Logf("%d shapes (%d plain, %d v-prefixed or non-plain): ForChart == the served name", n, len(plain), len(other))
}

// CONTROL: the old status-side expression agrees with the served name exactly for plain versions and
// disagrees for every non-plain one, which is #591. Without this the identity above could hold over
// inputs that never exercised the defect.
func TestForChart_591_StatusSideOldExpressionDiffers(t *testing.T) {
	plain, other := shapes591()
	for _, v := range plain {
		if statusBefore(v) != servedBefore(v) {
			t.Errorf("control: plain %q: status-side %q != served %q", v, statusBefore(v), servedBefore(v))
		}
	}
	differ := 0
	for _, v := range other {
		if statusBefore(v) != servedBefore(v) {
			differ++
		}
	}
	// v-prefixed lowercase shapes ("v1.0.0" -> "vv1-0-0") are already normalized, so they agree; every
	// shape with an uppercase letter or a character other than [0-9a-z.] differs.
	for _, c := range []struct {
		v      string
		differ bool
	}{{"v1.0.0", false}, {"v0.2.45", false}, {"V1.0.0", true}, {"1.0.0-RC1", true}, {"1.0.0+build.5", true}, {"2.0.0-Beta.2", true}, {"1.0.0--rc", true}, {"1.0.0_x", true}, {"1.0.0-rc.1", false}} {
		if got := statusBefore(c.v) != servedBefore(c.v); got != c.differ {
			t.Errorf("control: %q status-side %q vs served %q: differ=%v, want %v", c.v, statusBefore(c.v), servedBefore(c.v), got, c.differ)
		}
	}
	if differ == 0 {
		t.Fatal("control: no non-plain shape differs, so the identity test exercised nothing #591 is about")
	}
	t.Logf("%d of %d v-prefixed/non-plain shapes: status-side old expression != served name", differ, len(other))
}

// The versions #591 measured, by literal. 1.0.0 is the control. v1.0.0 (valid in Helm) keeps vv1-0-0.
func TestForChart_591_Rows(t *testing.T) {
	for _, c := range []struct{ chart, want string }{
		{"1.0.0", "v1-0-0"}, // control
		{"1.0.0-RC1", "v1-0-0-rc1"},
		{"1.0.0+build.5", "v1-0-0-build-5"},
		{"2.0.0-Beta.2", "v2-0-0-beta-2"},
		{"1.0.0--rc", "v1-0-0-rc"},
		{"v1.0.0", "vv1-0-0"},
	} {
		if got := versionname.ForChart(c.chart); got != c.want {
			t.Errorf("ForChart(%q) = %q, want %q", c.chart, got, c.want)
		}
	}
}

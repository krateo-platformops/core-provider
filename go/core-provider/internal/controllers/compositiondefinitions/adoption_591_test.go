package compositiondefinitions

import (
	"testing"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
)

// #591 in adoption's spec clause. #210's window: the definition that owns a version has been created
// but not reconciled yet, so only its SPEC says which version it is for; an older reconciled
// definition of the same chart is on another version. An orphan on the new version must go to the
// unreconciled owner. Before the fix the spec clause derived "v" + dots-as-dashes ("1.0.0-RC1" ->
// v1-0-0-RC1), which never equals the served version label (v1-0-0-rc1), so the clause was dead for
// any non-plain chart and the orphan went to the older definition of the Kind instead.
//
// The served column is literal, measured on a real apiserver (TestEnvtest_591_StatusNamesServedVersion).
// 1.0.0 is the control row. The CONTROL column asks for a version neither definition targets, which
// must still fall through to the oldest of the Kind, so the rows cannot pass by the election always
// picking the newcomer.
func TestAdopterFor_591_UnreconciledOwnerOfNonPlainVersion(t *testing.T) {
	for _, c := range []struct{ chartVersion, served string }{
		{"1.0.0", "v1-0-0"}, // control
		{"1.0.0-RC1", "v1-0-0-rc1"},
		{"1.0.0+build.5", "v1-0-0-build-5"},
		{"2.0.0-Beta.2", "v2-0-0-beta-2"},
		{"1.0.0--rc", "v1-0-0-rc"},
		{"v1.0.0", "vv1-0-0"}, // valid in Helm; its served name is preserved
	} {
		older := reconciledOn("krateo-system", "widget-old", 0, refGK, refURL, "0.9.0")
		owner := chartedCD("team", "widget", 1, refURL, c.chartVersion) // no status yet
		cds := []compositiondefinitionsv1alpha1.CompositionDefinition{older, owner}

		if got := adopterFor(cds, refGK, c.served); got == nil || got.Name != "widget" {
			name := "<nil>"
			if got != nil {
				name = got.Namespace + "/" + got.Name
			}
			t.Errorf("%s: an orphan on %s goes to %s, want team/widget (its spec targets chart %s)", c.chartVersion, c.served, name, c.chartVersion)
		}
		if got := adopterFor(cds, refGK, "v7-7-7"); got == nil || got.Name != "widget-old" {
			t.Errorf("%s control: an orphan on v7-7-7 (nobody targets it) goes to %v, want the oldest of the Kind, widget-old", c.chartVersion, got)
		}
	}
}

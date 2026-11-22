package composition

import (
	"testing"

	"github.com/krateo-platformops/composition-dynamic-controller/internal/tools/archive"
	compositionMeta "github.com/krateo-platformops/composition-dynamic-controller/pkg/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// #591: a genuine delete of a PARENT-RENDERED composition of a non-plain chart is not a migration
// handover, so the uninstall runs. Before the fix the definition's chart version was normalized as
// "v" + dots-as-dashes ("1.0.0-RC1" -> v1-0-0-RC1) while the composition is on the served version
// (v1-0-0-rc1): they always differed, every delete read as a hand-over, and Delete returned before
// the uninstall, orphaning the release.
//
// The served column is literal (core-provider's TestEnvtest_591_StatusNamesServedVersion measures
// it). 1.0.0 is the control row. The CONTROL column is a real migration: the definition has moved
// on to another version, which must still read as a hand-over, so the genuine-delete assertion
// cannot pass by the function answering false for everything.
func TestIsVersionMigrationHandover_591(t *testing.T) {
	for _, c := range []struct{ chartVersion, served string }{
		{"1.0.0", "v1-0-0"}, // control
		{"1.0.0-RC1", "v1-0-0-rc1"},
		{"1.0.0+build.5", "v1-0-0-build-5"},
		{"2.0.0-Beta.2", "v2-0-0-beta-2"},
		{"1.0.0--rc", "v1-0-0-rc"},
	} {
		mg := &unstructured.Unstructured{}
		mg.SetGroupVersionKind(schema.GroupVersionKind{Group: "composition.krateo.io", Version: c.served, Kind: "Widget"})
		mg.SetName("w")
		mg.SetAnnotations(map[string]string{compositionMeta.AnnotationKeyHelmReleaseName: "w"})
		mg.SetLabels(map[string]string{compositionMeta.CompositionVersionLabel: c.served})

		if isVersionMigrationHandover(mg, &archive.Info{Version: c.chartVersion, DefinitionVersion: c.chartVersion}) {
			t.Errorf("%s: a genuine delete (definition still on %s, composition on %s) reads as a migration handover: the uninstall would be skipped", c.chartVersion, c.chartVersion, c.served)
		}
		if !isVersionMigrationHandover(mg, &archive.Info{Version: c.chartVersion, DefinitionVersion: "3.0.0-RC9"}) {
			t.Errorf("%s control: the definition moved to 3.0.0-RC9 and the composition on %s does not read as a migration handover", c.chartVersion, c.served)
		}
	}
}

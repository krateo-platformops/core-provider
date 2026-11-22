package archive

import (
	"strings"
	"testing"

	"github.com/krateo-platformops/unstructured-runtime/pkg/logging"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// #591. The definition as core-provider writes it (measured on a real apiserver in core-provider's
// TestEnvtest_591_StatusNamesServedVersion): spec.chart.version keeps the chart's spelling, and the
// CRD, status.managed.versionInfo, the controller's own version and every composition-version label
// use the SERVED version. The served column is literal, not derived here, so a derivation that drifts
// from what the CRD serves fails rather than agreeing with itself. 1.0.0 is the control.
var served591 = []struct{ chartVersion, served string }{
	{"1.0.0", "v1-0-0"}, // control
	{"1.0.0-RC1", "v1-0-0-rc1"},
	{"1.0.0+build.5", "v1-0-0-build-5"},
	{"2.0.0-Beta.2", "v2-0-0-beta-2"},
	{"1.0.0--rc", "v1-0-0-rc"},
}

func cd591(chartVersion, specURL string, recorded map[string]string) *unstructured.Unstructured {
	infos := []any{}
	for version, url := range recorded {
		infos = append(infos, map[string]any{"version": version, "chart": map[string]any{"url": url, "version": chartVersion}})
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"spec":   map[string]any{"chart": map[string]any{"url": specURL, "version": chartVersion}},
		"status": map[string]any{"managed": map[string]any{"versionInfo": infos}},
	}}
}

// The current-version controller (own == served) renders spec.chart, whether or not core-provider has
// recorded its versionInfo entry yet. Before the fix, "v" + dots-as-dashes of spec.chart.version
// never equalled the served version for a non-plain chart, so the current controller took the
// retiring-version branch: errNoChartForVersion before the entry existed, the entry's chart after.
func TestChartForVersion_591_CurrentControllerRendersSpec(t *testing.T) {
	for _, c := range served591 {
		for _, rec := range []map[string]string{nil, {c.served: "oci://x/widget"}} {
			rc, err := chartForVersion(cd591(c.chartVersion, "oci://x/widget", rec), c.served)
			if err != nil || rc.source != "spec.chart" {
				t.Errorf("%s (own %s, versionInfo recorded %v): source %q err %v, want spec.chart", c.chartVersion, c.served, rec != nil, rc.source, err)
			}
		}
	}
}

// A same-version change to spec.chart.url (or repo, credentials, TLS flag) reaches the current
// controller. versionInfo's chart is written only when its entry is first appended, so before the fix
// a non-plain chart kept rendering the OLD url for ever.
//
// CONTROL, in the same table: a RETIRING controller (own = another version) still renders its own
// recorded chart, so the assertion is about which branch is taken, not that every path returns spec.
func TestChartForVersion_591_SameVersionSpecChangeIsSeen(t *testing.T) {
	for _, c := range served591 {
		cd := cd591(c.chartVersion, "oci://NEW/widget", map[string]string{c.served: "oci://OLD/widget", "v0-9-0": "oci://RETIRING/widget"})
		if rc, err := chartForVersion(cd, c.served); err != nil || rc.url != "oci://NEW/widget" {
			t.Errorf("%s: the current controller renders %q (source %q, err %v), want spec's oci://NEW/widget", c.chartVersion, rc.url, rc.source, err)
		}
		if rc, err := chartForVersion(cd, "v0-9-0"); err != nil || rc.url != "oci://RETIRING/widget" {
			t.Errorf("%s control: the retiring v0-9-0 controller renders %q (err %v), want its recorded oci://RETIRING/widget", c.chartVersion, rc.url, err)
		}
	}
}

// The definition lookup with two same-Kind definitions in the namespace and no definition-ref labels.
// A CONTRACT test, not a regression of cdc code (the search is unchanged): it pins that once
// status.apiVersion names the served version, which core-provider now writes, the lookup resolves.
// The pre-fix status (the chart's own spelling) is kept as the differential: the instrument must
// still fail on it, or the passing rows prove nothing.
func TestSearch_591_StatusOnServedVersionResolves(t *testing.T) {
	for _, c := range served591 {
		for _, pre := range []bool{false, true} {
			status := c.served
			if pre {
				status = "v" + strings.ReplaceAll(c.chartVersion, ".", "-")
			}
			defs := []runtime.Object{
				newSearchTestCD("widget", "krateo-system", status, "Widget"),
				newSearchTestCD("widget-other", "krateo-system", "v9-9-9", "Widget"),
			}
			dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{searchTestCDGVR: "CompositionDefinitionList"}, defs...)
			g := &dynamicGetter{dynamicClient: dyn, logger: logging.NewNopLogger()}
			got, err := g.searchCompositionDefinition(searchTestCDGVR, newSearchTestComposition("Widget", c.served, nil))
			found := err == nil && got != nil && got.GetName() == "widget"
			switch {
			case !pre && !found:
				t.Errorf("%s: status.apiVersion %s, label %s: not found (err %v)", c.chartVersion, status, c.served, err)
			case pre && status != c.served && found:
				t.Errorf("%s differential: the pre-fix status %s resolved, so this instrument cannot tell the fix from its absence", c.chartVersion, status)
			case pre && status == c.served && !found:
				t.Errorf("%s control: plain version, pre-fix spelling is the served one, and it did not resolve (err %v)", c.chartVersion, err)
			}
		}
	}
}

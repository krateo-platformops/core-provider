package composition

import (
	"testing"

	"github.com/krateo-platformops/composition-dynamic-controller/internal/tools/archive"
	helmconfig "github.com/krateo-platformops/plumbing/helm"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// crWithVersion builds a composition CR whose apiVersion encodes crVersion (e.g. "v0-1-7"), plus any
// extra labels. isVersionMigrationHandover derives the CR version from the apiVersion alone, so the
// labels are deliberately irrelevant — the tests below prove the handover is detected with and
// without the krateo.io/composition-definition-* labels the umbrella normally stamps.
func crWithVersion(crVersion string, labels map[string]string) *unstructured.Unstructured {
	mg := &unstructured.Unstructured{}
	mg.SetAPIVersion("composition.krateo.io/" + crVersion)
	mg.SetKind("KrateoObservability")
	mg.SetNamespace("krateo-system")
	mg.SetName("krateo-observability")
	if labels != nil {
		mg.SetLabels(labels)
	}
	return mg
}

// fullDefinitionLabels is the complete label set a healthy instance carries.
var fullDefinitionLabels = map[string]string{
	"krateo.io/composition-definition-group":     "core.krateo.io",
	"krateo.io/composition-definition-version":   "v1alpha1",
	"krateo.io/composition-definition-resource":  "compositiondefinitions",
	"krateo.io/composition-definition-namespace": "krateo-system",
	"krateo.io/composition-definition-name":      "krateo-observability",
	"krateo.io/composition-version":              "v0-1-7",
	"krateo.io/release-name":                     "krateo-observability",
}

// pkgWithChartVersion builds the package info the getter produces: pkg.Version is the owning
// CompositionDefinition's current spec.chart.version (resolved robustly, independent of labels).
func pkgWithChartVersion(chartVersion string) *archive.Info {
	return &archive.Info{Version: chartVersion}
}

func TestIsVersionMigrationHandover(t *testing.T) {
	cases := []struct {
		name       string
		cr         *unstructured.Unstructured
		pkg        *archive.Info
		wantResult bool
	}{
		{
			name:       "migration: CD advanced to a newer version than the pruned CR",
			cr:         crWithVersion("v0-1-7", fullDefinitionLabels),
			pkg:        pkgWithChartVersion("0.1.8"),
			wantResult: true, // handover -> skip uninstall
		},
		{
			name:       "genuine deletion: CD version equals the CR version",
			cr:         crWithVersion("v0-1-7", fullDefinitionLabels),
			pkg:        pkgWithChartVersion("0.1.7"),
			wantResult: false, // uninstall
		},
		{
			// The #89 regression: a failed install never stamped the composition-definition-*
			// labels, then the next version bump prunes the old-GVK CR. The handover MUST still be
			// detected (from the apiVersion + resolved pkg) so the destructive uninstall is skipped.
			name:       "failed install (NO definition labels): version bump still detected as handover",
			cr:         crWithVersion("v0-1-7", nil),
			pkg:        pkgWithChartVersion("0.1.8"),
			wantResult: true,
		},
		{
			name:       "genuine deletion with NO definition labels: same version -> uninstall",
			cr:         crWithVersion("v0-1-7", nil),
			pkg:        pkgWithChartVersion("0.1.7"),
			wantResult: false,
		},
		{
			name:       "nil package info -> safe default (uninstall)",
			cr:         crWithVersion("v0-1-7", fullDefinitionLabels),
			pkg:        nil,
			wantResult: false,
		},
		{
			name:       "empty CD chart version -> safe default (uninstall)",
			cr:         crWithVersion("v0-1-7", fullDefinitionLabels),
			pkg:        pkgWithChartVersion(""),
			wantResult: false,
		},
		{
			name:       "CR with no apiVersion version -> safe default (uninstall)",
			cr:         &unstructured.Unstructured{},
			pkg:        pkgWithChartVersion("0.1.8"),
			wantResult: false,
		},
		{
			name:       "multi-digit versions normalize correctly (0.2.193 vs v0-2-190)",
			cr:         crWithVersion("v0-2-190", nil),
			pkg:        pkgWithChartVersion("0.2.193"),
			wantResult: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isVersionMigrationHandover(tc.cr, tc.pkg)
			if got != tc.wantResult {
				t.Errorf("isVersionMigrationHandover = %v, want %v", got, tc.wantResult)
			}
		})
	}
}

// TestIsIncompleteHelmOperation guards the set of release statuses the stuck-operation recovery
// watches. The #89 regression: StatusUninstalling must be included so a release stuck mid-uninstall
// self-recovers instead of wedging forever; the three pending-* statuses must stay in; every settled
// status must stay out (so a healthy release is never needlessly rolled back).
func TestIsIncompleteHelmOperation(t *testing.T) {
	cases := []struct {
		status helmconfig.Status
		want   bool
	}{
		{helmconfig.StatusPendingInstall, true},
		{helmconfig.StatusPendingUpgrade, true},
		{helmconfig.StatusPendingRollback, true},
		{helmconfig.StatusUninstalling, true}, // #89: newly covered
		{helmconfig.StatusDeployed, false},
		{helmconfig.StatusFailed, false},
		{helmconfig.StatusUninstalled, false},
		{helmconfig.StatusSuperseded, false},
		{helmconfig.StatusUnknown, false},
	}
	for _, tc := range cases {
		t.Run(string(tc.status), func(t *testing.T) {
			if got := isIncompleteHelmOperation(tc.status); got != tc.want {
				t.Errorf("isIncompleteHelmOperation(%q) = %v, want %v", tc.status, got, tc.want)
			}
		})
	}
}

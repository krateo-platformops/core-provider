package composition

import (
	"testing"

	"github.com/krateo-platformops/composition-dynamic-controller/internal/tools/archive"
	compositionMeta "github.com/krateo-platformops/composition-dynamic-controller/pkg/meta"
	helmconfig "github.com/krateo-platformops/plumbing/helm"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// crWithVersion builds a PARENT-RENDERED composition CR whose apiVersion encodes crVersion (e.g.
// "v0-1-7"), plus any extra labels. The krateo.io/composition-definition-* labels are deliberately
// irrelevant to the decision — the tests below prove the handover is detected with and without
// them. The helm ownership annotation is NOT irrelevant: it is what makes this object one that a
// parent re-renders, which is the precondition for a handover existing at all (#199). Standalone
// objects are built by crStandalone.
//
// The CR carries NO origin annotation, which exercises the pre-#168 fallback path (derive the
// version from the delivered apiVersion). crWithOrigin covers the stamped case.
func crWithVersion(crVersion string, labels map[string]string) *unstructured.Unstructured {
	mg := &unstructured.Unstructured{}
	mg.SetAPIVersion("composition.krateo.io/" + crVersion)
	mg.SetKind("KrateoObservability")
	mg.SetNamespace("krateo-system")
	mg.SetName("krateo-observability")
	if labels != nil {
		mg.SetLabels(labels)
	}
	// Rendered as a child of the umbrella release, like every composition in the handover scenario
	// these cases describe.
	mg.SetAnnotations(map[string]string{compositionMeta.AnnotationKeyHelmReleaseName: "installer-platform"})
	return mg
}

// crWithOrigin builds a parent-rendered CR DELIVERED at deliveredVersion but stamped as originating
// at originVersion — the state that exists for the whole window between a CRD re-point and the old
// object being pruned, and the state in which #168 destroyed a release.
func crWithOrigin(deliveredVersion, originVersion string) *unstructured.Unstructured {
	mg := crWithVersion(deliveredVersion, fullDefinitionLabels)
	ann := mg.GetAnnotations()
	ann[compositionMeta.AnnotationKeyOriginVersion] = originVersion
	mg.SetAnnotations(ann)

	// krateo.io/composition-version equals the ORIGIN here, which is what a real object in this
	// state carries and what fullDefinitionLabels' fixed "v0-1-7" was silently misrepresenting.
	//
	// The version MutatingAdmissionPolicy stamps that label on CREATE and UPDATE — on a WRITE to
	// the object. Re-pointing the CRD to a new served version is neither, so the label does not
	// move: the object is DELIVERED at the new version while still labelled with the one it was
	// written at. That is precisely the race these cases describe.
	//
	// An object whose label has moved past its origin is a different state entirely — it was
	// written again, i.e. migrated in place rather than recreated — and the tests for that set the
	// label explicitly (withServedLabel, handover_stalestamp_test.go).
	l := mg.GetLabels()
	l2 := make(map[string]string, len(l))
	for k, v := range l {
		l2[k] = v
	}
	l2[compositionMeta.CompositionVersionLabel] = originVersion
	mg.SetLabels(l2)
	return mg
}

// crStandalone builds a composition that NOTHING renders: no helm ownership annotation and no
// parent-composition label. On krateo-057 this is the Installer umbrella itself and the 17
// BuilderPublish objects created by the portal's publish flow — 24 of 83 compositions.
//
// A bump re-points their CRD underneath them rather than replacing them, so they are never deleted
// by a migration and their write-once origin stamp drifts arbitrarily far behind the CD. Both facts
// point the same way: a delete here is always a real teardown.
func crStandalone(deliveredVersion, originVersion string) *unstructured.Unstructured {
	mg := &unstructured.Unstructured{}
	mg.SetAPIVersion("composition.krateo.io/" + deliveredVersion)
	mg.SetKind("Installer")
	mg.SetNamespace("krateo-system")
	mg.SetName("installer")
	if originVersion != "" {
		mg.SetAnnotations(map[string]string{compositionMeta.AnnotationKeyOriginVersion: originVersion})
	}
	return mg
}

// crChildComposition builds a composition rendered by a PARENT COMPOSITION rather than by the
// umbrella directly: no helm annotation, but the krateo.io/composition-name label the post-renderer
// stamps on every child manifest. It must be treated as parent-rendered, because it is — this is
// the case where relying on the helm annotation alone would wrongly conclude "standalone" and
// destroy a release mid-handover.
func crChildComposition(deliveredVersion, originVersion string) *unstructured.Unstructured {
	mg := crStandalone(deliveredVersion, originVersion)
	mg.SetLabels(map[string]string{compositionMeta.ParentCompositionNameLabel: "portal"})
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

		// --- #168: the origin stamp, and the race it closes.
		{
			// THE PORTAL FAILURE, reproduced. portal 1.8.70 -> 1.8.71: the CRD re-point landed
			// before the old-version controller processed the Delete, so the object came back
			// DELIVERED at v1-8-71 while having been written at v1-8-70. Pre-#168 this read
			// cdVersion == crVersion and uninstalled the release and all its children.
			//
			// With the origin stamp the comparison uses where the object came FROM, so the bump is
			// still recognised and the uninstall is still skipped — whoever wins the race.
			name:       "LOST RACE: delivered at the NEW version but stamped at the old -> handover",
			cr:         crWithOrigin("v1-8-71", "v1-8-70"),
			pkg:        pkgWithChartVersion("1.8.71"),
			wantResult: true,
		},
		{
			// The same object before the re-point — the frontend 1.8.1 -> 1.8.2 case, which won
			// the race and worked even pre-#168. It must keep working.
			name:       "WON RACE: delivered at the old version and stamped at the old -> handover",
			cr:         crWithOrigin("v1-8-70", "v1-8-70"),
			pkg:        pkgWithChartVersion("1.8.71"),
			wantResult: true,
		},
		{
			// The stamp must not turn every deletion into a handover: a composition created at the
			// CD's current version and genuinely deleted still has to uninstall, or releases leak
			// on every ordinary teardown.
			name:       "genuine deletion of a stamped CR at the current version -> uninstall",
			cr:         crWithOrigin("v1-8-71", "v1-8-71"),
			pkg:        pkgWithChartVersion("1.8.71"),
			wantResult: false,
		},
		{
			// The stamp WINS over the delivered apiVersion. Without this the test above could pass
			// on the fallback path alone and prove nothing about precedence.
			name:       "origin stamp takes precedence: stamped==CD but delivered differs -> uninstall",
			cr:         crWithOrigin("v1-8-70", "v1-8-71"),
			pkg:        pkgWithChartVersion("1.8.71"),
			wantResult: false,
		},
		{
			name:       "empty origin annotation falls back to the delivered apiVersion",
			cr:         crWithOrigin("v0-1-7", ""),
			pkg:        pkgWithChartVersion("0.1.8"),
			wantResult: true,
		},

		// --- #199: a standalone composition can never be handed over, so it must always uninstall.
		{
			// THE LEAK, reproduced. The Installer umbrella is standalone: created 2026-08-24,
			// generation 374, same uid across many versions, no owner, no helm annotation. It took
			// the stamp on one reconcile and froze there while the CD kept advancing. Before this
			// fix cd != origin read as a handover forever, so a user deleting the composition
			// orphaned every release the umbrella owns.
			name:       "LEAK: standalone, CD advanced one bump past the frozen stamp -> uninstall",
			cr:         crStandalone("v0-3-454", "v0-3-453"),
			pkg:        pkgWithChartVersion("0.3.454"),
			wantResult: false,
		},
		{
			// The same object after many more releases. Nothing self-corrects, which is why the
			// severity grows rather than decays.
			name:       "LEAK: standalone, CD advanced many bumps past the stamp -> uninstall",
			cr:         crStandalone("v0-3-460", "v0-3-453"),
			pkg:        pkgWithChartVersion("0.3.460"),
			wantResult: false,
		},
		{
			name:       "standalone at the CD's current version -> uninstall",
			cr:         crStandalone("v0-3-453", "v0-3-453"),
			pkg:        pkgWithChartVersion("0.3.453"),
			wantResult: false,
		},
		{
			// The 17 BuilderPublish objects: standalone and still unstamped, so they take the
			// pre-#168 fallback. Under that fallback a bumped CD read as a handover; standalone
			// objects are not handed over, so this must uninstall too.
			name:       "standalone and UNSTAMPED with the CD ahead -> uninstall",
			cr:         crStandalone("v1-8-84", ""),
			pkg:        pkgWithChartVersion("1.8.90"),
			wantResult: false,
		},
		{
			// The safe-direction requirement. No helm annotation, but the post-renderer's
			// parent-composition label says it HAS a parent. Treating this as standalone would
			// uninstall a release mid-handover — the #168 failure, reintroduced through the new
			// gate. Either signal present means parent-rendered.
			name:       "child of a parent COMPOSITION (no helm annotation) is still a handover",
			cr:         crChildComposition("v1-8-71", "v1-8-70"),
			pkg:        pkgWithChartVersion("1.8.71"),
			wantResult: true,
		},
		{
			name:       "child of a parent composition at the current version -> uninstall",
			cr:         crChildComposition("v1-8-71", "v1-8-71"),
			pkg:        pkgWithChartVersion("1.8.71"),
			wantResult: false,
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

// TestHandoverIsOrderingIndependent asserts the property #168 exists to provide, as a property
// rather than as two rows that happen to agree: the outcome must not depend on whether the CRD
// re-point beat the Delete.
//
// That ordering is the whole bug. The old-version controller's Delete can arrive BEFORE the
// re-point (the object still reads its old apiVersion — frontend 1.8.1 -> 1.8.2, which worked) or
// AFTER it (the object already reads the NEW apiVersion — portal 1.8.70 -> 1.8.71, which did not,
// and the release was uninstalled). Only the DELIVERED version differs between the two; the object,
// its origin and the CD are the same.
//
// #168 passed its own tests while being race-dependent because its table exercised one ordering per
// row and nothing compared them. This compares them, so a future change that reintroduces the
// dependence fails here even if every row still passes individually.
//
// Deliberately NOT asserting on the delivered apiVersion itself: the cdc logs that value on
// entering Delete, and it IS the bug's mechanism rather than an independent record of it, so a test
// anchored on it would assert the symptom. The assertion is on the decision.
func TestHandoverIsOrderingIndependent(t *testing.T) {
	const (
		originVersion = "v1-8-70" // where the object was actually written
		oldDelivered  = "v1-8-70" // Delete arrives BEFORE the re-point (race won)
		newDelivered  = "v1-8-71" // Delete arrives AFTER the re-point (race lost)
	)

	cases := []struct {
		name  string
		cd    string
		want  bool
		build func(delivered string) *unstructured.Unstructured
	}{
		{
			name:  "parent-rendered, CD bumped: handover in both orderings",
			cd:    "1.8.71",
			want:  true,
			build: func(d string) *unstructured.Unstructured { return crWithOrigin(d, originVersion) },
		},
		{
			name:  "parent-rendered, CD unchanged: uninstall in both orderings",
			cd:    "1.8.70",
			want:  false,
			build: func(d string) *unstructured.Unstructured { return crWithOrigin(d, originVersion) },
		},
		{
			// A standalone object is never deleted by a bump, so neither ordering is reachable for
			// it in production — but the decision must still be stable, or the gate is doing its
			// job by accident.
			name:  "standalone: uninstall in both orderings",
			cd:    "1.8.71",
			want:  false,
			build: func(d string) *unstructured.Unstructured { return crStandalone(d, originVersion) },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pkg := pkgWithChartVersion(tc.cd)
			raceWon := isVersionMigrationHandover(tc.build(oldDelivered), pkg)
			raceLost := isVersionMigrationHandover(tc.build(newDelivered), pkg)

			if raceWon != raceLost {
				t.Fatalf("the decision depends on who won the race: delivered=%s -> %v, delivered=%s -> %v\n"+
					"this is core-provider#168: the re-point landing before the Delete must not change the outcome",
					oldDelivered, raceWon, newDelivered, raceLost)
			}
			if raceWon != tc.want {
				t.Fatalf("ordering-independent but wrong: got %v, want %v", raceWon, tc.want)
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

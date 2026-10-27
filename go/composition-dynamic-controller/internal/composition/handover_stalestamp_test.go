package composition

import (
	"testing"

	"github.com/krateo-platformops/composition-dynamic-controller/internal/tools/archive"
	compositionMeta "github.com/krateo-platformops/composition-dynamic-controller/pkg/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// withServedLabel stamps krateo.io/composition-version, which the version MutatingAdmissionPolicy
// writes on EVERY write to the object — so it always names the version the object is served at now.
func withServedLabel(mg *unstructured.Unstructured, served string) *unstructured.Unstructured {
	l := mg.GetLabels()
	if l == nil {
		l = map[string]string{}
	}
	l[compositionMeta.CompositionVersionLabel] = served
	mg.SetLabels(l)
	return mg
}

// The #199 residual.
//
// #199's early return exempts parent-rendered compositions from the stale-stamp problem, on the
// reasoning that a parent re-renders them at each bump so helm recreates and re-stamps them. That
// holds for a bump delivered BY the parent. It does not hold for one delivered out of band, past
// the umbrella's pin: core-provider then re-points the CRD under the object it already has,
// exactly as for a standalone composition, and the write-once stamp freezes while the served
// version walks away from it.
//
// IsParentRendered still says true, so the stale stamp is consulted after all, and a genuine
// delete reads as a migration and skips the uninstall — orphaning the release. The #199 defect, in
// the one population #199 exempted.
func TestIsVersionMigrationHandover_StaleStampOnAnOutOfBandBumpIsNotAHandover(t *testing.T) {
	// Stamped at v0-1-7, but the served label says the object now sits at v0-1-9: it was
	// re-pointed in place, twice, without ever being recreated.
	mg := withServedLabel(crWithOrigin("v0-1-9", "v0-1-7"), "v0-1-9")
	pkg := &archive.Info{Version: "0.1.9"}

	if isVersionMigrationHandover(mg, pkg) {
		t.Fatal("a genuine delete of a re-pointed parent-rendered composition read as a migration " +
			"hand-over: the uninstall is skipped and the helm release is orphaned")
	}
}

// The control arm, and the behaviour that must NOT change: the ordinary hand-over.
//
// A parent-rendered child that WAS recreated at the bump carries a stamp equal to its served
// label, because Create stamped it at the version it was created at. The CD has moved on, so this
// delete really is the old half of a migration and must not uninstall.
//
// Without this, the test above would pass against a function that had simply stopped detecting
// hand-overs at all — which would uninstall live releases during every version migration.
func TestIsVersionMigrationHandover_RecreatedChildIsStillAHandover(t *testing.T) {
	// Created at v0-1-7 and still served at v0-1-7 — the old object, about to be pruned, while
	// the CD has advanced to 0.1.9.
	mg := withServedLabel(crWithOrigin("v0-1-7", "v0-1-7"), "v0-1-7")
	pkg := &archive.Info{Version: "0.1.9"}

	if !isVersionMigrationHandover(mg, pkg) {
		t.Fatal("the ordinary migration hand-over is no longer detected; every version bump would " +
			"uninstall the release it is handing over")
	}
}

// Absence of the label is not evidence of staleness. An object written before the policy existed,
// or on a cluster without it, has no freshness signal either way — and inventing one is the
// mistake this file's own history is made of (a field a resource does not carry is evidence of
// nothing). The guard must yield and leave the prior behaviour exactly as it was.
func TestIsVersionMigrationHandover_MissingServedLabelChangesNothing(t *testing.T) {
	pkg := &archive.Info{Version: "0.1.9"}

	// No label at all: same inputs as the hand-over case above, minus the freshness evidence.
	if !isVersionMigrationHandover(crWithOrigin("v0-1-7", "v0-1-7"), pkg) {
		t.Error("an unlabelled object stopped being a hand-over; the guard is inventing staleness " +
			"from an absent field")
	}
	// And an unlabelled object whose stamp happens to differ from its delivered apiVersion is
	// still judged by the old rule, not by the new one.
	if !isVersionMigrationHandover(crWithOrigin("v0-1-9", "v0-1-7"), pkg) {
		t.Error("an unlabelled object with a differing stamp changed verdict; without the label " +
			"there is no evidence to change it on")
	}
}

// An empty label is the same as an absent one — a policy that ran but wrote nothing is not a
// claim about the version.
func TestIsVersionMigrationHandover_EmptyServedLabelIsTreatedAsAbsent(t *testing.T) {
	mg := withServedLabel(crWithOrigin("v0-1-7", "v0-1-7"), "")
	if !isVersionMigrationHandover(mg, &archive.Info{Version: "0.1.9"}) {
		t.Fatal("an empty served label was read as evidence of staleness")
	}
}

// The live shape on krateo-057, as measured: krateo-system/installer, stamped v0-3-453 and now
// served at v0-3-459 — a gap of six that grows by one every installer release and never closes.
//
// It is STANDALONE, so #199's early return already catches it and this guard never runs. Pinned
// here so that if anyone ever relaxes IsParentRendered, the object that actually exists on the
// platform cluster is the thing that fails first.
func TestIsVersionMigrationHandover_TheLiveInstallerStampIsNeverAHandover(t *testing.T) {
	mg := withServedLabel(crStandalone("v0-3-459", "v0-3-453"), "v0-3-459")

	if isVersionMigrationHandover(mg, &archive.Info{Version: "0.3.459"}) {
		t.Fatal("the Installer composition read as a migration hand-over; a delete of it would " +
			"orphan every release the umbrella owns")
	}
}

package compositiondefinitions

import (
	"testing"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/tools/deploy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var adoptGK = schema.GroupKind{Group: "composition.krateo.io", Kind: "SelfserviceKrateo"}

const adoptURL = "oci://ghcr.io/krateo-blueprints/charts/selfservice-krateo"

// reconciledCD is a definition whose status says it is on version (chart chartVersion) and that has
// managed every version in served.
func reconciledCD(namespace, name string, ageRank int, version, chartVersion string, served ...string) compositiondefinitionsv1alpha1.CompositionDefinition {
	cd := ownerCD(namespace, name, ageRank)
	cd.Spec.Chart = &compositiondefinitionsv1alpha1.ChartInfo{Url: adoptURL, Version: chartVersion}
	cd.Status.ApiVersion = adoptGK.Group + "/" + version
	cd.Status.Kind = adoptGK.Kind
	cd.Status.Managed.Group = adoptGK.Group
	cd.Status.Managed.Kind = adoptGK.Kind
	for _, v := range served {
		cd.Status.Managed.VersionInfo = append(cd.Status.Managed.VersionInfo, compositiondefinitionsv1alpha1.VersionDetail{
			Version: v, Served: true,
			Chart: &compositiondefinitionsv1alpha1.ChartInfoProps{Url: adoptURL, Version: "x"},
		})
	}
	return cd
}

// unreconciledCD has a spec and no status at all: created, never reconciled (or paused, or erroring).
func unreconciledCD(namespace, name string, ageRank int, chartVersion string) compositiondefinitionsv1alpha1.CompositionDefinition {
	cd := ownerCD(namespace, name, ageRank)
	cd.Spec.Chart = &compositiondefinitionsv1alpha1.ChartInfo{Url: adoptURL, Version: chartVersion}
	return cd
}

func instance(name, refNamespace, refName, version string) unstructured.Unstructured {
	u := unstructured.Unstructured{}
	u.SetNamespace("tenant-" + name)
	u.SetName(name)
	u.SetLabels(map[string]string{
		deploy.CompositionDefinitionNamespaceLabel: refNamespace,
		deploy.CompositionDefinitionNameLabel:      refName,
		deploy.CompositionVersionLabel:             version,
	})
	return u
}

func names(us []unstructured.Unstructured) []string {
	out := make([]string, 0, len(us))
	for _, u := range us {
		out = append(out, u.GetName())
	}
	return out
}

var servedBoth = map[string]bool{"v0-2-45": true, "v0-2-46": true}

// The live case of #215: selfservice-krateo was moved from krateo-system to its own namespace by a
// hand-over, then bumped 0.2.45 -> 0.2.46. The instances still name the deleted definition and sit
// on v0-2-45. The moved definition must adopt them, whichever side of its own status write the
// question is asked from: Update adopts before that write, Observe asks after it.
func TestOrphanedBy_RehomeThenBump_StableAcrossTheStatusWrite(t *testing.T) {
	insts := []unstructured.Unstructured{
		instance("acme", "krateo-system", "selfservice-krateo", "v0-2-45"),
		instance("globex", "krateo-system", "selfservice-krateo", "v0-2-45"),
	}
	for _, tc := range []struct {
		name   string
		status string
	}{
		{"during Update, status still on the old version", "v0-2-45"},
		{"after Update, status on the new version", "v0-2-46"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			moved := reconciledCD("krateo-bp-selfservice-krateo", "selfservice-krateo", 5, tc.status, "0.2.46", "v0-2-45", "v0-2-46")
			cds := []compositiondefinitionsv1alpha1.CompositionDefinition{moved}
			got := orphanedBy(&moved, cds, insts, adoptGK, servedBoth)
			if len(got) != 2 {
				t.Fatalf("adopted %v, want [acme globex]", names(got))
			}
		})
	}
}

// Correctly owned instances, and instances of a definition that is being deleted (it hands over
// first), are not orphans.
func TestOrphanedBy_OnlyMissingDefinitionsOrphan(t *testing.T) {
	owner := reconciledCD("krateo-bp-x", "x", 5, "v0-2-46", "0.2.46", "v0-2-46")
	leaving := deleting(reconciledCD("krateo-system", "x", 0, "v0-2-46", "0.2.46", "v0-2-46"))
	cds := []compositiondefinitionsv1alpha1.CompositionDefinition{owner, leaving}
	insts := []unstructured.Unstructured{
		instance("own", "krateo-bp-x", "x", "v0-2-46"),
		instance("leaving", "krateo-system", "x", "v0-2-46"),
		instance("gone", "elsewhere", "x", "v0-2-46"),
	}
	got := orphanedBy(&owner, cds, insts, adoptGK, servedBoth)
	if n := names(got); len(n) != 1 || n[0] != "gone" {
		t.Fatalf("adopted %v, want [gone]", n)
	}
}

// An instance without definition-ref labels, or on a version the CRD no longer serves, is not
// adopted: there is no endpoint to write it through at its encoded version, and the served-version
// prune accounts for it instead.
func TestOrphanedBy_SkipsUnlabelledAndUnserved(t *testing.T) {
	owner := reconciledCD("krateo-bp-x", "x", 5, "v0-2-46", "0.2.46", "v0-2-45", "v0-2-46")
	cds := []compositiondefinitionsv1alpha1.CompositionDefinition{owner}
	unlabelled := instance("unlabelled", "", "", "v0-2-45")
	insts := []unstructured.Unstructured{unlabelled, instance("unserved", "gone", "x", "v0-2-44")}
	if got := orphanedBy(&owner, cds, insts, adoptGK, servedBoth); len(got) != 0 {
		t.Fatalf("adopted %v, want none", names(got))
	}
}

// An owner that has not reconciled yet has no status. It must still be recognised from its spec,
// or the orphans of its version go to an older definition on ANOTHER version, whose migration
// would then move them there: live instances re-pointed at the wrong definition.
func TestAdopterFor_UnreconciledOwnerOfTheVersionWins(t *testing.T) {
	other := reconciledCD("team-a", "x", 0, "v0-2-46", "0.2.46", "v0-2-46")
	fresh := unreconciledCD("krateo-bp-x", "x", 5, "0.2.45")
	cds := []compositiondefinitionsv1alpha1.CompositionDefinition{other, fresh}

	if got := adopterFor(cds, adoptGK, "v0-2-45"); ownerName(got) != "krateo-bp-x/x" {
		t.Fatalf("adopter of a v0-2-45 orphan = %s, want the unreconciled krateo-bp-x/x that targets v0-2-45", ownerName(got))
	}
	if got := orphanedBy(&other, cds, []unstructured.Unstructured{instance("acme", "gone", "x", "v0-2-45")}, adoptGK, servedBoth); len(got) != 0 {
		t.Fatalf("team-a/x must not adopt v0-2-45 orphans while krateo-bp-x/x targets v0-2-45; adopted %v", names(got))
	}
}

// With no definition on the orphans' version, the oldest live local definition of the Kind adopts
// them (its migration then moves them to a version it serves). Deleting and remote definitions
// never adopt.
func TestAdopterFor_FallsBackToTheOldestOfTheKind(t *testing.T) {
	older := reconciledCD("team-a", "x", 1, "v0-2-47", "0.2.47", "v0-2-47")
	newer := reconciledCD("team-b", "x", 2, "v0-2-46", "0.2.46", "v0-2-46")
	oldest := deleting(reconciledCD("team-c", "x", 0, "v0-2-46", "0.2.46", "v0-2-46"))
	remote := remoteCD(reconciledCD("team-d", "x", 0, "v0-2-47", "0.2.47", "v0-2-47"))
	cds := []compositiondefinitionsv1alpha1.CompositionDefinition{newer, oldest, remote, older}

	if got := adopterFor(cds, adoptGK, "v0-2-45"); ownerName(got) != "team-a/x" {
		t.Fatalf("adopter = %s, want team-a/x (oldest live local of the Kind)", ownerName(got))
	}
	if got := adopterFor(cds, adoptGK, "v0-2-46"); ownerName(got) != "team-b/x" {
		t.Fatalf("adopter at v0-2-46 = %s, want team-b/x (the live definition on that version)", ownerName(got))
	}
}

// A definition of another Kind never adopts, and with no definition of the Kind left nobody does.
func TestAdopterFor_OtherKindNever(t *testing.T) {
	other := ownerCD("team-a", "y", 0)
	other.Spec.Chart = &compositiondefinitionsv1alpha1.ChartInfo{Url: "oci://ghcr.io/x/charts/other", Version: "1.0.0"}
	other.Status.ApiVersion = "composition.krateo.io/v1-0-0"
	other.Status.Kind = "Other"
	other.Status.Managed.Group, other.Status.Managed.Kind = "composition.krateo.io", "Other"
	if got := adopterFor([]compositiondefinitionsv1alpha1.CompositionDefinition{other}, adoptGK, "v0-2-45"); got != nil {
		t.Fatalf("adopter = %s, want none", ownerName(got))
	}
}

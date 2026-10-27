package compositiondefinitions

import (
	"testing"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
)

// deletingSelf is the definition whose Delete is running: it carries a deletionTimestamp by then.
func deletingSelf(namespace, name string, ageRank int, version, chartVersion string) compositiondefinitionsv1alpha1.CompositionDefinition {
	return deleting(reconciledCD(namespace, name, ageRank, version, chartVersion, version))
}

// #218: a helm upgrade that MOVES a Kind and BUMPS it in one apply.
//
// CD-old in krateo-system on chart 0.2.45 (v0-2-45) is deleted; CD-new in its own namespace on
// 0.2.46 (v0-2-46) is created in the same second and has not reconciled. Delete's instance gate is
// `successor == nil && len(lst) == 1`, and BOTH terms are version-specific, so both are satisfied —
// nothing targets v0-2-45 any more. The instances were uninstalled a reconcile before #216's
// adoption, which runs in Update on CD-new, could have taken them.
func TestAdopterOfServed_MoveAndBumpLeavesTheInstancesToTheSuccessor(t *testing.T) {
	self := deletingSelf("krateo-system", "selfservice-krateo", 0, "v0-2-45", "0.2.45")
	incoming := unreconciledCD("selfservice", "selfservice-krateo", 1, "0.2.46")
	cds := []compositiondefinitionsv1alpha1.CompositionDefinition{self, incoming}

	got := adopterOfServed(cds, adoptGK, "v0-2-45", servedBoth)
	if got == nil {
		t.Fatal("no adopter found for the v0-2-45 instances, so Delete destroys them — this is #218: " +
			"the successor exists, it just has not reconciled yet")
	}
	if got.Name != "selfservice-krateo" || got.Namespace != "selfservice" {
		t.Errorf("adopter = %s/%s, want selfservice/selfservice-krateo", got.Namespace, got.Name)
	}
}

// The control arm, and the behaviour that must NOT change: the genuinely last definition on a Kind
// still cleans its instances up. Without this, the test above would pass against a gate that had
// simply stopped deleting anything.
func TestAdopterOfServed_LastDefinitionStillCleansUp(t *testing.T) {
	self := deletingSelf("krateo-system", "selfservice-krateo", 0, "v0-2-45", "0.2.45")
	cds := []compositiondefinitionsv1alpha1.CompositionDefinition{self}

	if got := adopterOfServed(cds, adoptGK, "v0-2-45", servedBoth); got != nil {
		t.Fatalf("the last definition on the Kind found an adopter (%s/%s), so deleting it would "+
			"leave its instances alive with nothing reconciling them", got.Namespace, got.Name)
	}
}

// The case that makes this predicate narrower than "is the Kind served elsewhere", and the reason
// it was chosen over it.
//
// orphanedBy will not adopt an instance whose version is no longer SERVED. So a sibling existing
// is not enough: on an unserved version nothing can adopt and no controller reconciles, and
// preserving those instances would strand them alive, unowned and unreconcilable, where #189's
// unaccounted-instance gate would then block the served-version prune for good. They go with the
// definition.
func TestAdopterOfServed_UnservedVersionHasNoAdopterEvenWithASiblingPresent(t *testing.T) {
	self := deletingSelf("krateo-system", "selfservice-krateo", 0, "v0-2-45", "0.2.45")
	sibling := reconciledCD("selfservice", "selfservice-krateo", 1, "v0-2-46", "0.2.46", "v0-2-46")
	cds := []compositiondefinitionsv1alpha1.CompositionDefinition{self, sibling}

	// Control arm first: while v0-2-45 IS served, this very population does elect an adopter.
	// That is what makes the assertion below about servedness and not about the population.
	if adopterOfServed(cds, adoptGK, "v0-2-45", servedBoth) == nil {
		t.Fatal("control arm: no adopter even while the version is served, so the next assertion " +
			"would pass for the wrong reason")
	}

	onlyNewServed := map[string]bool{"v0-2-46": true}
	if got := adopterOfServed(cds, adoptGK, "v0-2-45", onlyNewServed); got != nil {
		t.Fatalf("elected %s/%s to adopt instances on an UNSERVED version: nothing can adopt them, "+
			"so they would be kept alive unowned and unreconcilable forever",
			got.Namespace, got.Name)
	}
}

// A deliberate consequence, recorded so it is a decision rather than a side effect: two
// definitions coexisting on one Kind at different versions, one deleted. Its instances are now
// inherited rather than destroyed, because an adopter genuinely exists for them.
//
// This differs from the documented behaviour at compositiondefinitions.go:1659 and was chosen on
// the grounds that the recoverable failure (instances left alive, deletable by hand) is the right
// one to prefer over the unrecoverable one.
func TestAdopterOfServed_CoexistingSiblingInheritsRatherThanLoses(t *testing.T) {
	self := deletingSelf("krateo-system", "selfservice-krateo", 0, "v0-2-45", "0.2.45")
	sibling := reconciledCD("other", "selfservice-krateo", 1, "v0-2-46", "0.2.46", "v0-2-46")
	cds := []compositiondefinitionsv1alpha1.CompositionDefinition{self, sibling}

	got := adopterOfServed(cds, adoptGK, "v0-2-45", servedBoth)
	if got == nil {
		t.Fatal("a live sibling on the Kind did not inherit the deleted definition's instances")
	}
	if got.Namespace != "other" {
		t.Errorf("adopter namespace = %q, want \"other\"", got.Namespace)
	}
}

// Definitions that are themselves going away, and remote ones whose instances live on a spoke,
// are not adopters. Two definitions deleted together must both clean up: if each named the other,
// neither would, and the instances would leak with no controller.
func TestAdopterOfServed_TerminatingAndRemoteSiblingsAreNotAdopters(t *testing.T) {
	self := deletingSelf("krateo-system", "selfservice-krateo", 0, "v0-2-45", "0.2.45")

	for _, tc := range []struct {
		name    string
		sibling compositiondefinitionsv1alpha1.CompositionDefinition
	}{
		{"terminating sibling", deleting(reconciledCD("ns", "going", 1, "v0-2-46", "0.2.46", "v0-2-46"))},
		{"terminating, unreconciled", deleting(unreconciledCD("ns", "going-new", 1, "0.2.46"))},
		{"remote sibling", remoteCD(reconciledCD("ns", "spoke", 1, "v0-2-46", "0.2.46", "v0-2-46"))},
		{"remote, unreconciled", remoteCD(unreconciledCD("ns", "spoke-new", 1, "0.2.46"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cds := []compositiondefinitionsv1alpha1.CompositionDefinition{self, tc.sibling}
			if got := adopterOfServed(cds, adoptGK, "v0-2-45", servedBoth); got != nil {
				t.Fatalf("elected %s/%s, which is not a live local definition", got.Namespace, got.Name)
			}
		})
	}
}

// The definition being deleted must never elect itself: it would hold its own instances back from
// a cleanup that is supposed to happen, and nothing would ever reconcile them.
func TestAdopterOfServed_SelfIsNeverItsOwnAdopter(t *testing.T) {
	self := deletingSelf("krateo-system", "selfservice-krateo", 0, "v0-2-45", "0.2.45")
	cds := []compositiondefinitionsv1alpha1.CompositionDefinition{self}

	if got := adopterOfServed(cds, adoptGK, "v0-2-45", servedBoth); got != nil {
		t.Fatalf("the definition being deleted elected itself (%s/%s) to adopt its own instances",
			got.Namespace, got.Name)
	}
}

// The truth table of the destructive decision itself. adopterOfServed above tests the PREDICATE;
// this tests the WIRING, which is the thing that actually spares or destroys the instances.
//
// It exists because the wiring was briefly untested while the predicate was not: removing the
// adopter term from Delete's conjunction broke nothing, since no CI job runs this package's
// integration or e2e suites (the e2e job is scoped to ./internal/tools/crd/...). A fix whose last
// mile is uncovered is not a fix, and a test that only runs on a cluster nobody points CI at is
// not coverage.
func TestDeletesInstances_TruthTable(t *testing.T) {
	some := &compositiondefinitionsv1alpha1.CompositionDefinition{}

	for _, tc := range []struct {
		name                   string
		successor, adopter     *compositiondefinitionsv1alpha1.CompositionDefinition
		remote                 bool
		sameVersionDefinitions int
		want                   bool
		why                    string
	}{
		{
			name: "last definition on the version, local", sameVersionDefinitions: 1, want: true,
			why: "the base case: nothing survives this version, so its instances go with it",
		},
		{
			name: "controller handed over to a successor", successor: some, sameVersionDefinitions: 1,
			want: false, why: "#212: a hand-over removes nothing of the Kind's",
		},
		{
			name: "instances will be adopted", adopter: some, sameVersionDefinitions: 1,
			want: false, why: "#218: a move that also bumps leaves no same-version definition, " +
				"but the instances have a successor waiting",
		},
		{
			name: "another definition reports this exact version", sameVersionDefinitions: 2,
			want: false, why: "not the last definition on the version",
		},
		{
			name: "remote target, nothing else on the version", remote: true, sameVersionDefinitions: 1,
			want: true, why: "a spoke's instances are dedicated to this definition",
		},
		{
			name: "remote target, siblings counted on the management cluster", remote: true,
			sameVersionDefinitions: 3, want: true,
			why: "remote deliberately overrides the counting, which may not see the spoke's status",
		},
		{
			name: "remote target with an adopter", remote: true, adopter: some, sameVersionDefinitions: 1,
			want: false, why: "an adopter still wins: instancesWillBeAdopted returns nil for remote, " +
				"so a non-nil adopter here means the caller contradicted itself and the safe answer is to keep",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := deletesInstances(tc.successor, tc.adopter, tc.remote, tc.sameVersionDefinitions)
			if got != tc.want {
				t.Errorf("deletesInstances = %v, want %v — %s", got, tc.want, tc.why)
			}
		})
	}
}

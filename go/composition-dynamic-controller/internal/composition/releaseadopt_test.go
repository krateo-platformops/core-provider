package composition

import (
	"errors"
	"strings"
	"testing"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
)

// #303. A composition CR that is RECREATED gets a new UID, so a release name minted from that UID
// matches no release — Observe then reports ResourceExists=false and the runtime INSTALLS A SECOND
// RELEASE beside the first. Nothing uninstalls the first, and the component's objects split across
// the two by mutability: the workload rejoins the new release while the Services, mutable but owned
// by the failed old one, keep selecting a release nothing carries.
//
// Diego's statement of the requirement: when a new version is served in a generated CRD,
// core-provider should not uninstall and reinstall but do a helm upgrade.
//
// THE IDENTITY CHANGED AFTER DIEGO REFUTED THE FIRST ONE. The original decision matched the
// release NAME by shape (prefix + a suffix of the generator's length). He pointed out that two
// compositions can share a name in one namespace — different CompositionDefinitions are different
// CRDs — and that the name can be overridden outright. Both break a name-shaped identity, and the
// tests below are written against the cases he named rather than around them.
//
// These tests cover the DECISION, which is the half that can be wrong in a way that costs
// workloads. The listing above it is I/O.

// mariadb and postgres are two compositions SHARING A NAME in one namespace, which is the
// configuration the name-shaped identity could not represent.
var (
	mariadb  = compositionIdentity{Group: "composition.krateo.io", Resource: "mariadbs", Name: "db", Namespace: "openstack"}
	postgres = compositionIdentity{Group: "composition.krateo.io", Resource: "postgresqls", Name: "db", Namespace: "openstack"}
)

// owns builds a candidate release that records the given identity, as helm stores it after
// plumbing's post-renderer has stamped every object.
func owns(releaseName string, ids ...compositionIdentity) releaseCandidate {
	return releaseCandidate{Name: releaseName, Identities: ids, Decoded: true}
}

// THE FIX. A release minted for the OLD UID must be adopted by the CR bearing the NEW one, so the
// reconcile becomes an upgrade of that release instead of an install of a second.
func TestPickAdoptableRelease_AdoptsThePriorReleaseAfterTheCRIsRecreated(t *testing.T) {
	got, err := pickAdoptableRelease([]releaseCandidate{owns("db-oldoldol", mariadb)}, mariadb, "db-newnewne")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "db-oldoldol" {
		t.Errorf("picked %q, want the existing release %q. Returning the minted name is what "+
			"installs a second release beside the first (#303)", got, "db-oldoldol")
	}
}

// DIEGO'S CASE, AND THE ONE THE OLD DESIGN GOT CATASTROPHICALLY WRONG. mariadb has been recreated
// and its own release is GONE (a version-migration handover that lost its race uninstalled it).
// postgres, sharing the name `db` in this namespace, still has its release.
//
// Under the name-shaped identity, `db-ppppppp1` matched `db-` + 8 chars, so it was the SINGLE
// candidate and was adopted: a helm upgrade of postgres's release with mariadb's chart, replacing
// one composition's workloads with another's. Exactly what ErrAmbiguousRelease was written to
// prevent, reached through the arm that does not fire.
func TestPickAdoptableRelease_DoesNotAdoptASameNamedCompositionsRelease(t *testing.T) {
	got, err := pickAdoptableRelease([]releaseCandidate{owns("db-ppppppp1", postgres)}, mariadb, "db-mmmmmmm1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "db-mmmmmmm1" {
		t.Errorf("picked %q, want the minted name %q. The only existing release belongs to a "+
			"DIFFERENT composition that merely shares the name `db`; adopting it upgrades "+
			"postgres's release with mariadb's chart", got, "db-mmmmmmm1")
	}
}

// And the improvement, not merely the absence of the bug: with BOTH releases present, the recorded
// identity resolves it. The name-shaped identity saw two candidates here and refused — safe, but
// the composition could then never converge again.
func TestPickAdoptableRelease_PicksItsOwnWhenASameNamedCompositionAlsoHasARelease(t *testing.T) {
	got, err := pickAdoptableRelease([]releaseCandidate{
		owns("db-ppppppp1", postgres),
		owns("db-oldoldol", mariadb),
	}, mariadb, "db-mmmmmmm1")
	if err != nil {
		t.Fatalf("unexpected error: %v. Two releases whose names are both shaped like this "+
			"composition's is not an ambiguity once the identity is recorded", err)
	}
	if got != "db-oldoldol" {
		t.Errorf("picked %q, want %q", got, "db-oldoldol")
	}
}

// DIEGO'S OTHER CASE: the release name can be OVERRIDDEN. SetReleaseName is write-once, so a
// krateo.io/release-name on the object wins and need not begin with the CR name at all. A recreated
// CR has lost that label, so a name-shaped matcher filters out the composition's OWN release and
// mints — leaving #303 unfixed for precisely those compositions. A recorded identity never looks at
// the name.
func TestPickAdoptableRelease_AdoptsAReleaseWhoseNameWasOverridden(t *testing.T) {
	got, err := pickAdoptableRelease([]releaseCandidate{owns("legacy-stack-db", mariadb)}, mariadb, "db-mmmmmmm1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "legacy-stack-db" {
		t.Errorf("picked %q, want the overridden name %q: nothing about an adopted release's name "+
			"has to resemble the CR's", got, "legacy-stack-db")
	}
}

// The collision Diego named for safeReleaseName=false, where the release name IS the CR name. Both
// compositions compute `db`, and the second to arrive would UPGRADE the first one's release with
// its own chart. Refusing is the only safe answer: installing destroys postgres's workloads, and
// silently minting a different name is #303 under a new cause.
//
// This is the arm the deleted `if !h.safeReleaseName { return minted }` early return skipped.
func TestPickAdoptableRelease_RefusesWhenTheMintedNameBelongsToAnotherComposition(t *testing.T) {
	_, err := pickAdoptableRelease([]releaseCandidate{owns("db", postgres)}, mariadb, "db")
	if !errors.Is(err, ErrReleaseNameTaken) {
		t.Fatalf("err = %v, want ErrReleaseNameTaken. Returning the name would upgrade postgres's "+
			"release with mariadb's chart", err)
	}
	if !strings.Contains(err.Error(), "postgresqls") {
		t.Errorf("error does not name the composition holding it, so an operator cannot act on "+
			"it: %v", err)
	}
}

// Two stored releases both CLAIMING this composition. Nothing produces that today, so the point is
// that the answer is an error rather than a guess about which one holds the live objects.
func TestPickAdoptableRelease_RefusesWhenTwoReleasesClaimIt(t *testing.T) {
	_, err := pickAdoptableRelease([]releaseCandidate{
		owns("db-oldoldol", mariadb),
		owns("db-oldero1d", mariadb),
	}, mariadb, "db-mmmmmmm1")
	if !errors.Is(err, ErrAmbiguousRelease) {
		t.Fatalf("err = %v, want ErrAmbiguousRelease", err)
	}
}

// The ordinary first install, asserted so that none of the refusals above can be satisfied by a
// decision that refuses everything.
func TestPickAdoptableRelease_MintsWhenNothingExists(t *testing.T) {
	got, err := pickAdoptableRelease(nil, mariadb, "db-mmmmmmm1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "db-mmmmmmm1" {
		t.Errorf("picked %q, want the minted name %q", got, "db-mmmmmmm1")
	}
}

// NO EVIDENCE IS NOT A NON-MATCH, and this arm is why Decoded and Identities are separate fields.
// A release under this exact name carrying no composition labels at all was installed before the
// post-renderer stamped them, or is a hooks-only chart whose recorded manifest is empty by
// construction — helm splits hooks out of Manifest, which is how #311 happened.
//
// Returning minted here is the behaviour from before adoption existed (Create would GetRelease on
// the name, find it, and upgrade), kept deliberately because there is nothing better to go on. It
// is asserted so nobody later reads the absence of evidence as evidence of absence.
func TestPickAdoptableRelease_MintsForAReleaseUnderItsNameWithNoRecordedIdentity(t *testing.T) {
	got, err := pickAdoptableRelease([]releaseCandidate{{Name: "db-mmmmmmm1", Decoded: true}}, mariadb, "db-mmmmmmm1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "db-mmmmmmm1" {
		t.Errorf("picked %q, want %q", got, "db-mmmmmmm1")
	}
}

// ALL FOUR LABELS OR NOTHING. A partial identity is not a weaker match, it is a different claim:
// composition-name alone matches every composition of that name in the namespace, which is the
// confusion the type exists to remove.
func TestIdentityFromLabels_RequiresEveryField(t *testing.T) {
	full := map[string]string{
		labelCompositionGroup:     "composition.krateo.io",
		labelCompositionResource:  "mariadbs",
		labelCompositionName:      "db",
		labelCompositionNamespace: "openstack",
	}
	if got, ok := identityFromLabels(full); !ok || got != mariadb {
		t.Fatalf("identityFromLabels(full) = %v, %v; want %v, true", got, ok, mariadb)
	}

	for _, missing := range []string{labelCompositionGroup, labelCompositionResource, labelCompositionName, labelCompositionNamespace} {
		partial := map[string]string{}
		for k, v := range full {
			if k != missing {
				partial[k] = v
			}
		}
		if _, ok := identityFromLabels(partial); ok {
			t.Errorf("identityFromLabels accepted an identity missing %q; a partial identity "+
				"matches more compositions than it names", missing)
		}
	}
}

// THE TWO LABELS THAT MUST NOT BE IN THE IDENTITY, asserted because each would look like
// precision and each defeats the entire purpose. composition-id is the UID, which a recreated CR
// does not have; composition-installed-version changes on every bump, and a bump is the event
// adoption exists for. A match including either would never fire in the case it was built for.
//
// AND THE CONSEQUENCE IS STRONGER THAN "ADOPTION WOULD NOT FIRE", measured on a0-openstack-d4
// pre-#322: a chart's workload selector is DERIVED FROM THE RELEASE NAME, and a selector is
// immutable. So an identity that changes across a bump renames the release, and the renamed
// release renders a selector Kubernetes REFUSES:
//
//	DaemonSet.apps "openvswitch" is invalid: spec.selector: Invalid value:
//	  {"matchLabels":{"app.kubernetes.io/instance":"openvswitch-54xd86b6", ...}}: field is immutable
//
// against live objects carrying instance=openvswitch-d2z2v8c9 from the surviving release. The
// second release therefore cannot work at all -- it is not a tidy duplicate, it is a permanent
// upgrade failure. This test is what keeps the identity stable enough for the rendered selector to
// match what is already in the cluster.
func TestCompositionIdentity_ExcludesTheUIDAndTheVersion(t *testing.T) {
	labels := map[string]string{
		labelCompositionGroup:     "composition.krateo.io",
		labelCompositionResource:  "mariadbs",
		labelCompositionName:      "db",
		labelCompositionNamespace: "openstack",
		// Present on every real object, and deliberately not read.
		"krateo.io/composition-id":                "9c847c79-5555-6666-7777-888888888888",
		"krateo.io/composition-installed-version": "v0-3-28",
	}
	got, ok := identityFromLabels(labels)
	if !ok {
		t.Fatal("identityFromLabels rejected a complete set of labels")
	}
	if got != mariadb {
		t.Fatalf("identity = %v, want %v", got, mariadb)
	}

	// The same composition after a bump and a recreation: new UID, new served version, SAME
	// identity. If either field were read, this would differ and adoption would mint.
	labels["krateo.io/composition-id"] = "aaaaaaaa-0000-0000-0000-000000000000"
	labels["krateo.io/composition-installed-version"] = "v0-3-29"
	after, ok := identityFromLabels(labels)
	if !ok {
		t.Fatal("identityFromLabels rejected the post-bump labels")
	}
	if after != got {
		t.Errorf("identity changed across a bump and a recreation (%v -> %v); whichever of "+
			"composition-id or composition-installed-version leaked into it, adoption can never "+
			"fire in the case it exists for", got, after)
	}
}

// candidateFromRelease reads helm's OWN record of what it installed. Asserted against a manifest
// in the shape plumbing's post-renderer produces, and against an empty one — the hooks-only chart,
// which must come back decoded-but-unidentified rather than as a non-match.
func TestCandidateFromRelease_ReadsTheRecordedIdentity(t *testing.T) {
	manifest := `apiVersion: v1
kind: Service
metadata:
  name: db
  namespace: openstack
  labels:
    krateo.io/composition-group: composition.krateo.io
    krateo.io/composition-resource: mariadbs
    krateo.io/composition-name: db
    krateo.io/composition-namespace: openstack
    krateo.io/composition-id: 9c847c79-5555-6666-7777-888888888888
---
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: db
  namespace: openstack
  labels:
    krateo.io/composition-group: composition.krateo.io
    krateo.io/composition-resource: mariadbs
    krateo.io/composition-name: db
    krateo.io/composition-namespace: openstack
`
	cand := candidateFromRelease(&helmconfig.Release{Name: "db-oldoldol", Manifest: manifest})
	if !cand.Decoded {
		t.Fatal("candidateFromRelease could not decode a well-formed manifest")
	}
	if len(cand.Identities) != 1 {
		t.Fatalf("identities = %v, want exactly one (two objects of the same composition must "+
			"not count twice)", cand.Identities)
	}
	if !cand.Owns(mariadb) {
		t.Errorf("candidate does not claim %v; identities = %v", mariadb, cand.Identities)
	}
	if cand.Owns(postgres) {
		t.Errorf("candidate claims %v, which it must not", postgres)
	}

	// The hooks-only chart. helm keeps hooks OUT of Manifest by construction, so the recorded
	// manifest is empty and there is no identity to read. Decoded with no identities is the
	// no-evidence case; reporting it as a non-match would be a claim the record cannot support.
	empty := candidateFromRelease(&helmconfig.Release{Name: "db-hookonly", Manifest: ""})
	if len(empty.Identities) != 0 {
		t.Errorf("an empty manifest yielded identities %v", empty.Identities)
	}
	if empty.Owns(mariadb) {
		t.Error("an empty manifest claims this composition; no evidence must never read as a match")
	}
}

// Raised by the openstack-as-a-service session, who predicted exactly that simplification. It is
// the shape three sessions spent today hunting — a comment asserting a safety property no test
// covers — and this file had one within an hour of the comment being written.
//
// Asserted per state with its own reason, so a failure says which state was dropped and why it
// mattered, rather than "the mask changed".
func TestAdoptableStates_IncludeTheStatesAReleaseCanBeStuckIn(t *testing.T) {
	for _, tc := range []struct {
		state helmconfig.ListStates
		name  string
		why   string
	}{
		{helmconfig.ListFailed, "ListFailed",
			"the release that caused #303's outage was `failed` at revision 6 — dropping this is " +
				"the single change that makes adoption blind to the case it exists for"},
		{helmconfig.ListDeployed, "ListDeployed",
			"the ordinary case: a healthy release being upgraded"},
		{helmconfig.ListPendingInstall, "ListPendingInstall",
			"an install interrupted mid-flight still has objects, and recovery runs on exactly this"},
		{helmconfig.ListPendingUpgrade, "ListPendingUpgrade",
			"an upgrade interrupted mid-flight likewise"},
		{helmconfig.ListPendingRollback, "ListPendingRollback",
			"a rollback interrupted mid-flight likewise"},
		{helmconfig.ListSuperseded, "ListSuperseded",
			"a superseded revision means the release exists and has history; adopting is still right"},
	} {
		if adoptableStates&tc.state == 0 {
			t.Errorf("adoptableStates omits %s: %s", tc.name, tc.why)
		}
	}

	// THE OTHER DIRECTION, and it is not symmetry. Adopting an uninstalled release's name would
	// turn the install into a failure rather than an upgrade: its objects are gone and helm cannot
	// upgrade a release with no deployed revision. So these two must stay OUT.
	for _, tc := range []struct {
		state helmconfig.ListStates
		name  string
	}{
		{helmconfig.ListUninstalled, "ListUninstalled"},
		{helmconfig.ListUninstalling, "ListUninstalling"},
	} {
		if adoptableStates&tc.state != 0 {
			t.Errorf("adoptableStates includes %s: its objects are gone, and helm cannot upgrade a "+
				"release with no deployed revision, so adopting that name turns the install into a "+
				"failure instead of an upgrade", tc.name)
		}
	}
}

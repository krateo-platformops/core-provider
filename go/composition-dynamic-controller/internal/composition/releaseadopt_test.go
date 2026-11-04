package composition

import (
	"errors"
	"strings"
	"testing"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	compositionMeta "github.com/krateo-platformops/composition-dynamic-controller/pkg/meta"
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
// These tests cover the DECISION, which is the half that can be wrong in a way that costs
// workloads. The listing above it is I/O.

func crNamed(name string, uid types.UID) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetName(name)
	u.SetNamespace("openstack")
	u.SetUID(uid)
	u.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "composition.krateo.io", Version: "v0-3-28", Kind: "MariaDB",
	})
	return u
}

// THE FIX. A release minted for the OLD UID must be adopted by the CR bearing the NEW one, so the
// reconcile becomes an upgrade of that release instead of an install of a second.
func TestPickAdoptableRelease_AdoptsThePriorReleaseAfterTheCRIsRecreated(t *testing.T) {
	old := crNamed("mariadb", types.UID("92wd4czz-1111-2222-3333-444444444444"))
	new := crNamed("mariadb", types.UID("9c847c79-5555-6666-7777-888888888888"))

	oldRelease := compositionMeta.CalculateReleaseName(old, true)
	minted := compositionMeta.CalculateReleaseName(new, true)

	// PRECONDITION, and the whole defect in one assertion: the two UIDs produce DIFFERENT names.
	// If they did not, there would be nothing to fix and every arm below would pass vacuously.
	if oldRelease == minted {
		t.Fatalf("precondition: two different UIDs produced the same release name %q, so this "+
			"suite cannot exercise the recreation case", oldRelease)
	}

	got, err := pickAdoptableRelease([]string{oldRelease}, new.GetName(), minted, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != oldRelease {
		t.Errorf("picked %q, want the existing release %q. Returning the minted name %q is what "+
			"makes Observe report ResourceExists=false and install a SECOND release", got, oldRelease, minted)
	}
}

// THE DESTRUCTIVE DIRECTION, and the arm I care about most: a release belonging to a DIFFERENT
// composition whose name merely starts with this one's must never be adopted. Adopting it hands
// this composition another component's workloads, which is strictly worse than the split.
func TestPickAdoptableRelease_DoesNotAdoptAnotherCompositionsRelease(t *testing.T) {
	// `mariadb-extra` is a real naming shape, and its release name begins with "mariadb-".
	sibling := compositionMeta.CalculateReleaseName(
		crNamed("mariadb-extra", types.UID("aaaaaaaa-1111-2222-3333-444444444444")), true)
	if !strings.HasPrefix(sibling, "mariadb-") {
		t.Fatalf("precondition: %q does not start with \"mariadb-\", so this arm tests nothing", sibling)
	}

	me := crNamed("mariadb", types.UID("9c847c79-5555-6666-7777-888888888888"))
	minted := compositionMeta.CalculateReleaseName(me, true)

	got, err := pickAdoptableRelease([]string{sibling}, me.GetName(), minted, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == sibling {
		t.Errorf("adopted %q, which belongs to composition \"mariadb-extra\". A prefix match alone "+
			"is not identity; the suffix length is what separates them", sibling)
	}
	if got != minted {
		t.Errorf("picked %q, want the minted name %q: with no release of its own, this really is a "+
			"first install", got, minted)
	}
}

// AMBIGUITY MUST REFUSE, not guess in either direction. Adopting one of two arbitrarily could take
// over the wrong workloads; minting a fresh name would silently reproduce the split.
func TestPickAdoptableRelease_RefusesWhenTwoReleasesCouldBelongToIt(t *testing.T) {
	a := compositionMeta.CalculateReleaseName(crNamed("mariadb", types.UID("11111111-a-b-c-d")), true)
	b := compositionMeta.CalculateReleaseName(crNamed("mariadb", types.UID("22222222-a-b-c-d")), true)
	if a == b {
		t.Fatalf("precondition: both UIDs produced %q, so there is no ambiguity to test", a)
	}

	me := crNamed("mariadb", types.UID("33333333-a-b-c-d"))
	got, err := pickAdoptableRelease([]string{a, b}, me.GetName(),
		compositionMeta.CalculateReleaseName(me, true), true)

	if !errors.Is(err, ErrAmbiguousRelease) {
		t.Errorf("error %v does not wrap ErrAmbiguousRelease, so a caller cannot tell this apart "+
			"from a transient failure", err)
	}
	if got != "" {
		t.Errorf("returned %q alongside the error; a name returned on an ambiguous answer is one a "+
			"caller may use", got)
	}
}

// CONTROL: with no existing releases at all, the minted name is returned unchanged. Without this,
// every arm above could pass against a function that always returned its input.
func TestPickAdoptableRelease_MintsWhenNothingExists(t *testing.T) {
	me := crNamed("mariadb", types.UID("9c847c79-5555-6666-7777-888888888888"))
	minted := compositionMeta.CalculateReleaseName(me, true)

	got, err := pickAdoptableRelease(nil, me.GetName(), minted, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != minted {
		t.Errorf("picked %q, want %q", got, minted)
	}

	// And an unrelated release in the same namespace must not change that.
	other := compositionMeta.CalculateReleaseName(
		crNamed("memcached", types.UID("bbbbbbbb-1111-2222-3333-444444444444")), true)
	got, err = pickAdoptableRelease([]string{other}, me.GetName(), minted, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != minted {
		t.Errorf("picked %q with only an unrelated release present, want %q", got, minted)
	}
}

// safeReleaseName=false makes the release name the CR name, so a recreated CR already resolves to
// the same release and there is nothing to adopt. Asserted so that the adoption path cannot start
// firing for the unsafe mode, where the name carries no suffix to distinguish siblings.
func TestPickAdoptableRelease_UnsafeModeResolvesByNameAlone(t *testing.T) {
	me := crNamed("mariadb", types.UID("9c847c79-5555-6666-7777-888888888888"))
	minted := compositionMeta.CalculateReleaseName(me, false)
	if minted != "mariadb" {
		t.Fatalf("precondition: safeReleaseName=false produced %q, not the bare CR name", minted)
	}

	got, err := pickAdoptableRelease([]string{"mariadb", "mariadb-extra"}, me.GetName(), minted, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "mariadb" {
		t.Errorf("picked %q, want \"mariadb\"", got)
	}
}

// The matcher's own contract, asserted against the GENERATOR rather than against a restated format.
// If CalculateReleaseName's shape ever changes, this fails rather than the matcher silently ceasing
// to recognise what the generator produces.
func TestReleaseNameCouldBelongTo_RoundTripsWithTheGenerator(t *testing.T) {
	for _, name := range []string{"mariadb", "a", "keystone-api", "x-y-z"} {
		cr := crNamed(name, types.UID("9c847c79-5555-6666-7777-888888888888"))
		generated := compositionMeta.CalculateReleaseName(cr, true)

		if !compositionMeta.ReleaseNameCouldBelongTo(generated, name, true) {
			t.Errorf("the matcher does not recognise %q, which the generator just produced for %q",
				generated, name)
		}
		// And it must not recognise it as belonging to a DIFFERENT composition.
		if compositionMeta.ReleaseNameCouldBelongTo(generated, name+"-sibling", true) {
			t.Errorf("%q matched composition %q", generated, name+"-sibling")
		}
		if name != "mariadb" && compositionMeta.ReleaseNameCouldBelongTo(generated, "mariadb", true) {
			t.Errorf("%q (for %q) matched composition \"mariadb\"", generated, name)
		}
	}

	// Explicit negatives that a prefix-only matcher would get wrong.
	for _, tc := range []struct{ release, crName string }{
		{"mariadb-extra-9c847c79", "mariadb"}, // sibling's release, longer suffix
		{"mariadb-", "mariadb"},               // empty suffix
		{"mariadb-9c847c7", "mariadb"},        // one char short
		{"mariadb-9c847c799", "mariadb"},      // one char long
		{"mariadb", "mariadb"},                // no suffix at all, in safe mode
		{"other-9c847c79", "mariadb"},         // different composition entirely
	} {
		if compositionMeta.ReleaseNameCouldBelongTo(tc.release, tc.crName, true) {
			t.Errorf("%q must not be treated as a release of %q", tc.release, tc.crName)
		}
	}
}

// THE STATE MASK IS A TRIP-WIRE, not a comment.
//
// adoptableStates carries a comment saying ListDeployed alone would miss the case adoption exists
// for — the release that caused #303's outage was `failed` at revision 6. But the mask lives in the
// I/O half of this file, so nothing exercised it: a future simplification to `ListDeployed` on the
// reasonable-sounding grounds that "a deployed release is the only one worth adopting" would break
// no test and silently restore the defect.
//
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

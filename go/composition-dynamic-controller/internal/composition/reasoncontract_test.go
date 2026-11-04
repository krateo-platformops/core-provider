package composition

import (
	"testing"

	"github.com/krateo-platformops/unstructured-runtime/pkg/tools/unstructured/condition"
)

// The reason STRINGS this rollup emits are a cross-repo contract, and consolidating them onto
// unstructured-runtime's constants (which this package now does) removes one hazard and introduces
// another:
//
//	BEFORE  the cdc wrote "ZeroEvidence" as a bare literal in 6 places while the runtime already
//	        defined ReasonZeroEvidence. The two could drift apart silently.
//	AFTER   there is one string -- but a RENAME IN THE RUNTIME now propagates into the cdc on a
//	        recompile, with nothing failing here, and lands as a silent break in a third repo.
//
// So this asserts the VALUE, not just the identifier. If the runtime renames its constant, this
// test goes red in the repo that emits the condition, which is where someone can see who breaks.
//
// WHO BREAKS, confirmed by the installer session against their own call sites:
//
//	krateo-platformops/installer  chart/templates/_helpers.tpl:299
//	  inst.readyFromConditions accepts Ready=True, OR Unknown with reason EXACTLY "ZeroEvidence",
//	  with a strict else. A different Unknown reason reads as not-ready and inst.depsReady holds
//	  back every dependent.
//	krateo-platformops/installer  hack/ready-classify.sh:25-31
//	  same acceptance, same strict else.
//
// Failing closed is what those gates want -- a genuinely converging child also reports Unknown and
// MUST block. The consequence is that a rename turns every zero-evidence composition into a blocked
// dependency chain, SILENTLY, until someone notices dependents never appeared.
//
// If this test fails, the fix is NOT to update the expected string. It is to change the installer
// gates in the same change and tell the installer session before it ships.
func TestZeroEvidenceReasonStringIsStable(t *testing.T) {
	const expected = "ZeroEvidence"
	if condition.ReasonZeroEvidence != expected {
		t.Fatalf("condition.ReasonZeroEvidence = %q, want %q.\n\n"+
			"This is a CROSS-REPO BREAKING CHANGE, not a test to update. Two installer gates match "+
			"this string exactly and fail closed on anything else (_helpers.tpl:299 and "+
			"hack/ready-classify.sh:25-31). A rename blocks every dependent of every zero-evidence "+
			"composition, silently. Change the gates in the same change, and tell the installer "+
			"session before shipping.",
			condition.ReasonZeroEvidence, expected)
	}
}

// The partial-coverage reason is this package's OWN and is read by consumers for the same kind of
// bucketing, so it gets the same treatment. It is deliberately NOT in the runtime: nothing outside
// the child-health rollup can produce it.
func TestAvailablePartialCoverageReasonStringIsStable(t *testing.T) {
	const expected = "AvailablePartialCoverage"
	if ReasonAvailablePartialCoverage != expected {
		t.Errorf("ReasonAvailablePartialCoverage = %q, want %q: the reason is what a dashboard or "+
			"gate reads to tell Ready=True-over-most from Ready=True-over-a-minority, and the "+
			"whole point of #297 was that consumers must not have to parse the message prose",
			ReasonAvailablePartialCoverage, expected)
	}
}

// The reasons krateoReady treats as UNEVALUATED are the ones that mean "nobody assessed this child",
// as opposed to "this child is unhealthy" or "this child is still coming up". Pinned as a SET so
// that adding one is a deliberate edit here rather than a quiet widening of the branch.
//
// ObserveFailed is deliberately ABSENT: unstructured-runtime emits it as of its #43, but
// core-provider pins v1.7.5 which predates the constant. The mapping lands in the same change that
// bumps the pin -- so no cluster can produce a reason this rollup does not understand. Recorded
// here rather than in a message so the sequencing is visible to whoever does the bump.
func TestUnevaluatedReasonsAreTheExpectedSet(t *testing.T) {
	unevaluated := map[string]bool{
		condition.ReasonZeroEvidence: true,
		"NotObserved":                true, // provider-runtime v1.8.x, apis/common/v1
	}

	for reason := range unevaluated {
		if got := krateoReady(krateoCR("Unknown", reason, true)); got != childUnevaluated {
			t.Errorf("reason %q maps to %d, want childUnevaluated (%d)", reason, got, childUnevaluated)
		}
	}

	// CONTROL: a reason that must NOT be unevaluated, or the assertions above would pass against a
	// branch that swallowed everything.
	for _, reason := range []string{"Creating", "Unavailable"} {
		if got := krateoReady(krateoCR("Unknown", reason, true)); got == childUnevaluated {
			t.Errorf("reason %q maps to childUnevaluated; a converging or failing child must keep "+
				"holding its parent back", reason)
		}
	}

	// And the one that is coming, asserted as NOT YET mapped so the bump cannot forget it: when
	// this starts failing, the pin has been raised and the mapping is owed in the same change.
	if got := krateoReady(krateoCR("Unknown", "ObserveFailed", true)); got == childUnevaluated {
		t.Error("ObserveFailed now maps to childUnevaluated -- if that was deliberate, add it to " +
			"the set above and delete this arm. If the unstructured-runtime pin was just raised " +
			"without adding the mapping, that is the gap this arm exists to catch")
	}
}

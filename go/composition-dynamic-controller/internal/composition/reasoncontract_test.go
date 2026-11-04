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

// ChildrenForbidden is matched as an EXACT STRING by three consumers in installer, and unlike
// ZeroEvidence it was allowlisted there BEFORE anything emitted it — the safe ordering, since a
// reason nothing emits is never matched.
//
// WHO BREAKS, from the installer session's own merge (their #646, 8bbeb81):
//
//	krateo-platformops/installer  chart/templates/_helpers.tpl   inst.readyFromConditions
//	krateo-platformops/installer  hack/ready-classify.sh         the Unknown arm
//	krateo-platformops/installer  .github/scripts/check-ready-gate.py  the assertion
//
// Their test asserts THREE must-block contrasts beside the accept, and the one that matters here is
// `Unknown/Forbidden` — a near-miss spelling BLOCKS rather than silently claiming readiness. So a
// rename on this side does not fail loudly over there; it fails as a dependency chain that never
// gets created, which is the same silent shape as a ZeroEvidence rename and the reason this file
// pins values rather than identifiers.
//
// If this test fails, the fix is NOT to update the expected string.
func TestChildrenForbiddenReasonStringIsStable(t *testing.T) {
	const expected = "ChildrenForbidden"
	if ReasonChildrenForbidden != expected {
		t.Fatalf("ReasonChildrenForbidden = %q, want %q.\n\n"+
			"This is a CROSS-REPO BREAKING CHANGE. installer allowlists this exact string in three "+
			"places and their own test asserts that the near-miss \"Forbidden\" must BLOCK, so a "+
			"rename here does not surface as an error over there -- it surfaces as every "+
			"dependent of an RBAC-gapped composition never being created. Change all three "+
			"consumers in the same change, and tell the installer session before shipping.",
			ReasonChildrenForbidden, expected)
	}
	// And it must not collide with the reason it sits beside: two reasons that mean different
	// things to an operator (the children published nothing / we were refused a read) must not be
	// the same string, or the distinction #318 exists to create is unobservable.
	if ReasonChildrenForbidden == condition.ReasonZeroEvidence {
		t.Error("ChildrenForbidden and ZeroEvidence are the same string, so the cause of a " +
			"composition's missing evidence is no longer readable")
	}
}

// The reasons krateoReady treats as UNEVALUATED are the ones that mean "nobody assessed this child",
// as opposed to "this child is unhealthy" or "this child is still coming up". Pinned as a SET so
// that adding one is a deliberate edit here rather than a quiet widening of the branch.
//
// ObserveFailed is deliberately ABSENT, and an earlier version of this comment justified that with
// a clause that was FALSE ON THE DAY IT WAS WRITTEN (core-provider#320):
//
//	"core-provider pins v1.7.5 which predates the constant ... so no cluster can produce a
//	 reason this rollup does not understand."
//
// The pin was and is real, and v1.7.5 genuinely lacks the constant. But THIS ROLLUP READS CHILDREN,
// and a child's conditions are written by THE CHILD'S OWN CONTROLLER -- a RestDefinition-managed
// resource, a provider. core-provider's pin governs what the cdc writes about itself and governs
// nothing about what reasons its children carry. unstructured-runtime v1.7.6 is released and emits
// ObserveFailed from worker.go:649, so any component that has bumped is producing it on composition
// children today.
//
// So the deferral was never load-bearing -- which is worse than a premise that expired, because an
// expired premise leaves a date to check and this one named a real pin and a real absence and so
// read as verified. What was wrong was the INFERENCE, and an inference is the part nobody rereads.
//
// ObserveFailed stays unmapped as a DECISION, not a deferral: it means "the handler's Observe
// errored", which can be transient or persistent, and the reason does not say which. The fix
// belongs at the source -- rest-dynamic-controller has IsAuthorizationDenied and can emit a
// terminal reason for a permanent refusal instead of letting the runtime write the generic one
// (oasgen-provider#436). Once it does, ObserveFailed honestly means "we do not know", and the
// converging default is the right fail-safe for not knowing.
//
// Note that ObserveFailed is NOT the same claim as NotObserved: NotObserved means the runtime never
// got an answer, ObserveFailed means the handler's Observe returned an error. Collapsing them would
// make ObserveFailed mean whichever one the reader assumed.
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
	for _, reason := range []string{"Creating", "Unavailable", "AuthorizationDenied"} {
		if got := krateoReady(krateoCR("Unknown", reason, true)); got == childUnevaluated {
			t.Errorf("reason %q maps to childUnevaluated; a converging or failing child must keep "+
				"holding its parent back", reason)
		}
	}

	// AuthorizationDenied must be FAILED, not merely not-unevaluated — and the distinction is the
	// whole point of core-provider#320. It is rest-dynamic-controller's reason for a refusal that
	// will not be retried; before this case existed it fell to the converging default, so the
	// condition built to say "stop, this is permanent" reported as "still working on it" forever.
	//
	// Asserted separately from the loop above because "not unevaluated" is satisfied by converging
	// too, which is exactly the bucket it was wrongly in.
	if got := krateoReady(krateoCR("False", "AuthorizationDenied", true)); got != childFailed {
		t.Errorf("AuthorizationDenied maps to %d, want childFailed (%d). A 403 with no rate-limit "+
			"signal is a decision; reporting the parent as Creating says the opposite, indefinitely",
			got, childFailed)
	}

	// And the generic reason still buckets the same way, so the fix above did not come at the cost
	// of the case that already worked: a provider that reports Unavailable rather than a specific
	// reason must keep failing its parent.
	if got := krateoReady(krateoCR("False", "Unavailable", true)); got != childFailed {
		t.Errorf("Unavailable maps to %d, want childFailed (%d)", got, childFailed)
	}

	// And the one that is coming, asserted as NOT YET mapped so the bump cannot forget it: when
	// this starts failing, the pin has been raised and the mapping is owed in the same change.
	if got := krateoReady(krateoCR("Unknown", "ObserveFailed", true)); got == childUnevaluated {
		t.Error("ObserveFailed now maps to childUnevaluated -- if that was deliberate, add it to " +
			"the set above and delete this arm. If the unstructured-runtime pin was just raised " +
			"without adding the mapping, that is the gap this arm exists to catch")
	}
}

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
	for _, reason := range []string{"Creating", "Unavailable", "AuthorizationDenied", "CreateFailed", "UpdateFailed", "DriftNotConverging", "Updating", "Pending"} {
		if got := krateoReady(krateoCR("Unknown", reason, true)); got == childUnevaluated {
			t.Errorf("reason %q maps to childUnevaluated; a converging or failing child must keep "+
				"holding its parent back", reason)
		}
	}

	// AuthorizationDenied must be FAILED, not merely not-unevaluated — and the distinction is the
	// whole point of core-provider#320. VERIFIED FIRST-HAND, at krateo-platformops/oasgen-provider,
	// go/rest-dynamic-controller/internal/controllers/condition/condition.go:136-150 --
	// Type=TypeReady, Status=False, Reason=ReasonAuthorizationDenied. A 403 with no rate-limit
	// signal is a decision about this credential, so the controller stops issuing the verb; a 401
	// is deliberately NOT in the family, because that is what a credential being rotated under us
	// looks like and retrying it is how rotation is tolerated (their #313). Before this case
	// existed it fell to the converging default, so the condition built to say "stop, this is
	// permanent" reported as "still working on it" forever.
	//
	// WHERE THAT VERIFICATION FIRST WENT WRONG, because the mistake is reusable. I searched
	// krateo-platformops/rest-dynamic-controller -- the right NAME -- and found the string zero
	// times in a 21-line condition.go whose only reason is ReasonPending, on every branch and in
	// two other orgs. That repo is real and frozen: its last commit is 2026-08-07 and its last tag
	// 0.20.0, which is when RDC MOVED INTO the oasgen-provider monorepo as a subdirectory. So the
	// measurement was correct and the conclusion was correct about the repository searched, and
	// still wrong, because in a monorepo the MODULE path and the REPOSITORY name come apart. A
	// frozen former home returns the shape of a true negative.
	//
	// And the instrument note that made it a question rather than a refutation: GitHub's
	// code-search API returns 0 for the control "ReasonPending" too, in the same repo where grep
	// finds it three times. A zero from code search is a statement about the instrument. Pairing
	// every negative with a control the search MUST hit is what kept this from shipping as
	// "your reason does not exist".
	//
	// Asserted separately from the loop above because "not unevaluated" is satisfied by converging
	// too, which is exactly the bucket it was wrongly in.
	if got := krateoReady(krateoCR("False", "AuthorizationDenied", true)); got != childFailed {
		t.Errorf("AuthorizationDenied maps to %d, want childFailed (%d). A 403 with no rate-limit "+
			"signal is a decision; reporting the parent as Creating says the opposite, indefinitely",
			got, childFailed)
	}

	// CreateFailed and UpdateFailed must be FAILED, mapped BEFORE rest-dynamic-controller ships
	// them (oasgen-provider is landing them in 0.53.0). That ordering is the correction to this
	// file's own earlier mistake: the ObserveFailed deferral reasoned from core-provider's pin when
	// the reason arrives from the CHILD's controller, so waiting for a reason to appear is waiting
	// for the defect. An unmapped reason falls to converging — the exact inversion #320 is about.
	//
	// The verb is in the reason because the distinction used to live in prose the portal cannot
	// branch on; see childhealth.go's comment for the #42/#433 resolution behind it.
	for _, reason := range []string{"CreateFailed", "UpdateFailed"} {
		if got := krateoReady(krateoCR("False", reason, true)); got != childFailed {
			t.Errorf("reason %q maps to %d, want childFailed (%d). A create or update that FAILED "+
				"is not one still in progress; reporting the parent as Creating says the opposite, "+
				"indefinitely", reason, got, childFailed)
		}
	}

	// DriftNotConverging must be FAILED, and this is the one arm of the three that was wrong about
	// a LIVE cluster rather than a future one. It has shipped since oasgen-provider 0.33.0 (latest
	// 0.50.0), introduced by 156bc83 on 2026-10-26, so for that whole window every composition
	// with a child in this state was reported by this rollup as converging. Verified first-hand in
	// the monorepo, not taken on report; see the provenance note above for why the first attempt
	// measured a frozen repository instead.
	if got := krateoReady(krateoCR("False", "DriftNotConverging", true)); got != childFailed {
		t.Errorf("DriftNotConverging maps to %d, want childFailed (%d). The controller has STOPPED "+
			"issuing the update because it changed nothing; the remedy is a declaration change, "+
			"which is a human's. Reporting the parent as converging promises progress that nothing "+
			"will make", got, childFailed)
	}

	// The two that must STAY converging, pinned because they are live reasons from the same file
	// and the same releases. Without them, a change that widened the failing arm "to cover RDC's
	// Ready=False reasons" would pass: Pending and Updating are genuinely in progress, and failing
	// a parent for them would report a transient as terminal — the mirror of the defect above.
	for _, reason := range []string{"Pending", "Updating"} {
		if got := krateoReady(krateoCR("False", reason, true)); got != childConverging {
			t.Errorf("reason %q maps to %d, want childConverging (%d). Updating replaced the "+
				"runtime's Creating on the update path precisely because a resource that has "+
				"existed for days is not being created (oasgen-provider#213); it is still a "+
				"resource on its way, and must hold the parent back without failing it",
				reason, got, childConverging)
		}
	}

	// DeleteFailed must NOT be in that arm, and its absence is a decision. A refused delete holds
	// the finalizer (#261); a terminal-looking Ready reason invites the finalizer removal that
	// orphans a paid resource. Asserted so that someone extending the arm "for symmetry" has to
	// argue with this rather than discover it.
	if got := krateoReady(krateoCR("False", "DeleteFailed", true)); got == childFailed {
		t.Error("DeleteFailed maps to childFailed. A refused delete must hold its finalizer, so it " +
			"must not acquire a terminal Ready reason here -- see #261. If rest-dynamic-controller " +
			"ever emits this, it needs its own decision, not this arm")
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

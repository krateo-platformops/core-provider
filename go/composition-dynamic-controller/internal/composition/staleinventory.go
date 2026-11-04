package composition

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
)

// inventoryStaleReason answers one question: is status.managed still the inventory of what the
// current release renders? It returns the empty string when the inventory can be shown to match,
// and a human-readable reason when it cannot.
//
// #273. This is the inverse of the absentChildren check. That one catches a child RECORDED but
// missing from the cluster; this catches a child RENDERED but missing from the record, which had no
// symptom at all: status.digest is rewritten by Observe every reconcile so it tracks the CURRENT
// release, while status.managed is written only by Create/Update from whatever release THEY
// decoded. Nothing tied the two together, so a render that grew left the inventory a strict subset
// of reality with every other status field independently correct — and the health rollup, which can
// only iterate what the inventory lists, reported Available over children it had never heard of.
//
// THE RELEASE IS PASSED, NOT A BOOLEAN, and not inferred from a non-empty currentDigest. The
// inference looks free and is wrong — MEASURED, not assumed: ComputeReleaseDigest returns "" for an
// empty manifest, so `currentDigest != ""` means "a release exists AND renders something", and a
// hooks-only release (helm keeps hooks out of Manifest by construction, #311) would be read as no
// release at all. A boolean would have been worse than either: a literal at the call site that any
// edit can flip with nothing failing, which is what a surviving mutant demonstrated.
//
// rel is consulted ONLY for existence, never for content, so it cannot disagree with currentDigest
// about anything this function uses.
//
// THE COVERAGE TABLE, BY INPUT STATE rather than by the arms that exist. This form is the
// openstack-as-a-service session's, and the reason for it is the defect it exposes: reasoning about
// the arms reads only the branches someone wrote, while enumerating the inputs shows the row with
// no answer. The missing row was an EARLY RETURN:
//
//	managed ABSENT,  release exists     -> drift     #326  (this was the missing row)
//	managed ABSENT,  no release         -> silent          may not be created yet
//	managed present, digest ''          -> drift     #273  the unverifiable case
//	managed present, digest != current  -> drift     #273  the stale case
//	managed present, digest == current  -> silent          verified current
//
// The #273 digest arms had shipped, were tested, and were cited as covering exactly the case they
// could not see: the early return on an absent inventory sat in FRONT of them, so the digest
// comparison was unreachable precisely for the population with no digest. A check that is correct,
// shipped, tested, and structurally unable to reach its own case.
//
// So when someone says a check already covers a case -- including when the someone is me -- the
// question is not which arms exist but WHAT RETURNS BEFORE IT, AND ON WHAT CONDITION.
//
// It is deliberately a pure function of the stored status, the current digest and that object: Observe cannot
// afford a DecodeMinRelease per reconcile (15-27x the digest-only path, up to 125MB of allocation
// for a 5MB manifest — the measurement that settled #261), and it does not need one. The digest
// stamped beside the inventory at record time is sufficient to detect divergence in BOTH
// directions, a grown render and a shrunk one alike.
//
// It lives in its own function rather than inline in Observe because Observe cannot be exercised
// without a chart fixture this repository does not have, and a guard that cannot be tested directly
// is a guard that asserts its own mention (#232). The caller contributes nothing but the current
// digest.
func inventoryStaleReason(mg *unstructured.Unstructured, currentDigest string, rel *helmconfig.Release) (string, error) {
	if mg == nil {
		return "", fmt.Errorf("composition is nil")
	}

	_, hasManaged, err := unstructured.NestedSlice(mg.Object, "status", "managed")
	if err != nil {
		return "", fmt.Errorf("reading status.managed: %w", err)
	}
	if !hasManaged {
		// AN ABSENT INVENTORY USED TO RETURN HERE UNCONDITIONALLY, and the rationale was that
		// "never recorded" is already reported as Unknown rather than as healthy, so reporting
		// drift would only dispatch an Update for a composition that may simply not have been
		// created yet.
		//
		// That is sound for a composition with no release and FALSE for one that has a release,
		// and the two were not distinguished — which made the state self-perpetuating (#326):
		//
		//	status.managed absent -> no drift reported -> Observe says up-to-date -> no Update
		//	-> Update is the only writer of status.managed -> absent forever
		//
		// Observe passes resources: nil and recordManaged: false precisely so it cannot overwrite
		// the list (#248's mirror), so nothing in the system repaired it. #248 made this state
		// VISIBLE — it used to report Ready=True over objects it had never heard of — and left it
		// unrepairable. Measured on d4: heat holding one deployed release and 12 running pods,
		// reporting Ready=Unknown/ZeroEvidence, with magnum never emitted because its only
		// unsatisfied dependency was heat.
		//
		// Note the asymmetry that hid it. The present-but-UNSTAMPED arm below already reports
		// drift, justified as "the one Update it costs, once per composition, is precisely what
		// rewrites an inventory that has been frozen for weeks". The absent case wanted the same
		// treatment and was the only one excluded.
		if rel == nil {
			return "", nil
		}
		return "a release exists but no inventory was ever recorded, so the health rollup has " +
			"nothing to iterate", nil
	}

	// unstructured.NestedString, NOT maps.NestedString: the latter treats an ABSENT key as an access
	// error ("<nil> is of the type <nil>, expected string") rather than reporting absence, and the
	// absent case is the common one here — it is every composition recorded before this stamp
	// existed. Erroring on it would make Observe fail on every reconcile for exactly the population
	// the fix is for. The unit test for the unstamped case is what caught that.
	recordedDigest, found, err := unstructured.NestedString(mg.Object, "status", "managedDigest")
	if err != nil {
		return "", fmt.Errorf("reading status.managedDigest: %w", err)
	}

	switch {
	case !found || recordedDigest == "":
		// An inventory with no stamp is the UNVERIFIABLE state, and it is exactly the shape of every
		// composition recorded before this stamp existed — including the live #273 case. It must
		// report drift rather than be assumed fine: the one Update it costs, once per composition, is
		// precisely what rewrites an inventory that has been frozen for weeks. After that Update the
		// stamp is present and this branch never fires for that composition again.
		//
		// Assuming the unverifiable case is healthy would make the fix silent on the only population
		// that currently has the bug.
		return "the recorded set carries no digest, so it cannot be shown to match the current release", nil
	case recordedDigest != currentDigest:
		return fmt.Sprintf("the recorded set was decoded from release digest %s, the current release is %s",
			recordedDigest, currentDigest), nil
	}

	return "", nil
}

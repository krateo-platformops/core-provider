package composition

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
// It is deliberately a pure function of the stored status and the current digest: Observe cannot
// afford a DecodeMinRelease per reconcile (15-27x the digest-only path, up to 125MB of allocation
// for a 5MB manifest — the measurement that settled #261), and it does not need one. The digest
// stamped beside the inventory at record time is sufficient to detect divergence in BOTH
// directions, a grown render and a shrunk one alike.
//
// It lives in its own function rather than inline in Observe because Observe cannot be exercised
// without a chart fixture this repository does not have, and a guard that cannot be tested directly
// is a guard that asserts its own mention (#232). The caller contributes nothing but the current
// digest.
func inventoryStaleReason(mg *unstructured.Unstructured, currentDigest string) (string, error) {
	if mg == nil {
		return "", fmt.Errorf("composition is nil")
	}

	// An absent inventory is #248's ZeroEvidence and is not this check's business: "never recorded"
	// is already reported as Unknown rather than as healthy, and reporting drift on it would
	// dispatch an Update for a composition that may simply not have been created yet.
	_, hasManaged, err := unstructured.NestedSlice(mg.Object, "status", "managed")
	if err != nil {
		return "", fmt.Errorf("reading status.managed: %w", err)
	}
	if !hasManaged {
		return "", nil
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

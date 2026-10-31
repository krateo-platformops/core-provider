package composition

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// managedEntry is the shape Create/Update write into status.managed.
func managedEntry(resource, name, namespace string) any {
	return map[string]any{
		"apiVersion": "metal3.io/v1alpha1",
		"resource":   resource,
		"name":       name,
		"namespace":  namespace,
		"path":       "/apis/metal3.io/v1alpha1/namespaces/" + namespace + "/" + resource + "/" + name,
	}
}

func compositionWithInventory(managed []any, managedDigest string) *unstructured.Unstructured {
	status := map[string]any{"digest": "current"}
	if managed != nil {
		status["managed"] = managed
	}
	if managedDigest != "" {
		status["managedDigest"] = managedDigest
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "composition.krateo.io/v0-13-8",
		"kind":       "KubernetesCluster",
		"metadata":   map[string]any{"name": "lab-ha", "namespace": "krateo-system"},
		"status":     status,
	}}
}

// THE CASE THE FIX EXISTS FOR, in the direction that matters.
//
// The render GAINS entries between reconciles with the earlier inventory already stored — kog's
// chart gates its BaremetalHosts on a helm `lookup` of a PKI Secret that only exists after the
// first apply, so the five-entry inventory is correct when written and becomes a strict subset
// afterwards.
//
// This direction is load-bearing. A test built on a SHRINKING render would pass against code that
// only ever appends, which is the same shape as the rename argument that made #261's first test
// vacuous: a one-directional test cannot distinguish a fix from a fix's happy accident.
func TestInventoryStale_RenderGrewAfterTheInventoryWasRecorded(t *testing.T) {
	// Five entries, recorded from the release whose digest was "five-entry-render".
	five := []any{
		managedEntry("serviceaccounts", "lab-ha-cp-publisher", "krateo-system"),
		managedEntry("secrets", "lab-ha-kubeadm-pki", "krateo-system"),
		managedEntry("secrets", "lab-ha-cp-publisher-token", "krateo-system"),
		managedEntry("roles", "lab-ha-cp-publisher", "krateo-system"),
		managedEntry("rolebindings", "lab-ha-cp-publisher", "krateo-system"),
	}
	mg := compositionWithInventory(five, "five-entry-render")

	// The release now renders eleven: the six BaremetalHosts appeared once the PKI Secret existed.
	// Observe computes the current digest from that release.
	reason, err := inventoryStaleReason(mg, "eleven-entry-render")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reason == "" {
		t.Fatal("a five-entry inventory recorded against a different render must report drift; " +
			"got no reason, which is the #273 bug: Ready=True over six children the rollup never sees")
	}
	if !strings.Contains(reason, "five-entry-render") || !strings.Contains(reason, "eleven-entry-render") {
		t.Errorf("the reason must name BOTH digests so the divergence is diagnosable, got %q", reason)
	}
}

// THE CONTROL. The same inventory, stamped with the digest of the release actually current, must
// NOT report drift — otherwise the check reports drift unconditionally and the test above passes
// for the wrong reason. Without this arm, a function that always returned a reason would be green.
func TestInventoryStale_MatchingStampIsNotDrift(t *testing.T) {
	mg := compositionWithInventory([]any{
		managedEntry("baremetalhosts", "cp-blade01", "krateo-system"),
	}, "current")

	reason, err := inventoryStaleReason(mg, "current")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reason != "" {
		t.Errorf("an inventory stamped with the current release's digest is not drift, got %q", reason)
	}
}

// The shrinking direction, for completeness rather than as the primary evidence: a digest compares
// unequal whichever way the render moved, so this is one assertion rather than a second mechanism.
func TestInventoryStale_RenderShrank(t *testing.T) {
	mg := compositionWithInventory([]any{
		managedEntry("baremetalhosts", "cp-blade01", "krateo-system"),
		managedEntry("baremetalhosts", "cp-blade02", "krateo-system"),
	}, "two-entry-render")

	reason, err := inventoryStaleReason(mg, "one-entry-render")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reason == "" {
		t.Error("a render that shrank also leaves the inventory unmatched and must report drift")
	}
}

// An inventory with NO stamp is the state of every composition recorded before this fix, including
// the live #273 case. It must report drift: the single Update that costs is what rewrites an
// inventory frozen for weeks, and after it the stamp is present so this never fires again for that
// composition.
//
// Asserting the opposite — treating the unverifiable case as healthy — would make the fix silent on
// the only population that currently has the bug, which is the failure mode this test exists to
// prevent rather than a style preference.
func TestInventoryStale_UnstampedInventoryIsUnverifiableSoItReportsDrift(t *testing.T) {
	mg := compositionWithInventory([]any{
		managedEntry("secrets", "lab-ha-kubeadm-pki", "krateo-system"),
	}, "")

	reason, err := inventoryStaleReason(mg, "current")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reason == "" {
		t.Fatal("an inventory with no digest cannot be shown to match the release and must report " +
			"drift; treating it as healthy leaves every pre-fix composition broken")
	}
	if !strings.Contains(reason, "no digest") {
		t.Errorf("the reason should say the stamp is missing rather than name a digest, got %q", reason)
	}
}

// #248's distinction must survive. An ABSENT inventory is "never evaluated", already reported as
// ZeroEvidence/Unknown, and is not this check's business: reporting drift on it would dispatch an
// Update for a composition that may not have been created yet.
//
// Present-and-empty is a DIFFERENT state — "rendered, found nothing" — and is deliberately NOT
// excluded here: it is a recorded inventory like any other and its stamp is checked.
func TestInventoryStale_AbsentInventoryIsZeroEvidenceNotDrift(t *testing.T) {
	mg := compositionWithInventory(nil, "")

	reason, err := inventoryStaleReason(mg, "current")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reason != "" {
		t.Errorf("an absent inventory is #248's ZeroEvidence, not drift, got %q", reason)
	}
}

func TestInventoryStale_EmptyInventoryIsRecordedAndStillChecked(t *testing.T) {
	// "rendered, found nothing", stamped against a render that has since changed.
	mg := compositionWithInventory([]any{}, "empty-render")

	reason, err := inventoryStaleReason(mg, "now-renders-something")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reason == "" {
		t.Error("managed: [] is a recorded inventory, so a changed render must still report drift")
	}
}

func TestInventoryStale_NilComposition(t *testing.T) {
	if _, err := inventoryStaleReason(nil, "current"); err == nil {
		t.Error("a nil composition must error rather than report no drift: 'could not determine' " +
			"must not read as 'nothing to do'")
	}
}

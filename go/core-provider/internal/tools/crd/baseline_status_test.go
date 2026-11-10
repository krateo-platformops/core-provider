// #293's cheap guard: the two hand-maintained lists that decide whether a controller-owned status
// field survives a write must agree. No cluster needed, so this runs in the ordinary unit suite —
// the apiserver-level proof is in baseline_status_e2e_test.go.
package crd

import (
	"testing"

	crdutils "github.com/krateo-platformops/core-provider/internal/tools/crd/generation"
)

// baselineStatusWrites is every status path the cdc writes that is NOT a blueprint declaration.
// It is the contract between the two modules: the cdc writes these, this repo's
// status.schema.json must declare them, or the apiserver prunes them silently (#293).
//
// Adding a status write in the cdc without adding it here leaves the tests passing and the field
// pruned in production — so this list is the thing to update first, and the failure it produces
// names the field.
func baselineStatusWrites() map[string]any {
	return map[string]any{
		"digest":               "abc123",
		"previousDigest":       "abc122",
		"managedDigest":        "abc123", // #273 — the stamp that could not persist
		"previousManagedCount": int64(122),
		"helmChartUrl":         "oci://ghcr.io/krateo-platformops/charts/demo",
		"helmChartVersion":     "1.2.3",
		"observedGeneration":   int64(4),
		// NOT lastReconcileTime: #439 dropped it (liveness comes from the cdc's reconcile spans), and the
		// cdc no longer writes it. Re-adding it here without the write would only re-declare a dead field.
		// The resolved release name. Listed here for the writes->schema arm: the cdc writes it in
		// setStatus, so an undeclared field would be pruned exactly as managedDigest was (#293),
		// and the whole point of the field is to be readable afterwards.
		"releaseName": "lab-ha-54xd86b6",
	}
}

// TestBaselineStatusWritesAreAllReserved is the cheap half, and it runs without a cluster.
//
// Every path this repo declares in the baseline schema must also be in reservedStatusFields, or a
// blueprint can declare a statusDataTemplate forPath that collides with a controller-owned field
// and gets silently overwritten. The two lists are maintained by hand in different files; nothing
// but this test makes them agree.
func TestBaselineStatusWritesAreAllReserved(t *testing.T) {
	declared, err := crdutils.BaselineStatusProperties()
	if err != nil {
		t.Fatalf("reading the baseline status schema: %v", err)
	}
	if len(declared) == 0 {
		t.Fatal("the baseline schema declares no properties — the loader is broken, and every " +
			"assertion below would pass over an empty set")
	}

	for _, name := range declared {
		if !crdutils.IsReservedStatusField(name) {
			t.Errorf("status.%s is declared in the baseline schema but is NOT in "+
				"reservedStatusFields — a blueprint could declare a forPath that collides with it", name)
		}
	}

	// And the other direction, which is what #293 actually needed: a field the cdc writes must be
	// declared. conditions is the one documented exception — crdgen emits it from the Go type
	// rather than from statics/status.schema.json.
	declaredSet := map[string]struct{}{}
	for _, n := range declared {
		declaredSet[n] = struct{}{}
	}
	for path := range baselineStatusWrites() {
		if _, ok := declaredSet[path]; !ok {
			t.Errorf("the cdc writes status.%s but the baseline schema does not declare it — "+
				"the apiserver will prune it (#293)", path)
		}
	}

	// THE THIRD ARM, and it was missing while a comment claimed it existed.
	//
	// There are THREE hand-maintained lists that must agree: statics/status.schema.json,
	// reservedStatusFields, and baselineStatusWrites above. The arms above cover
	// schema->reserved and writes->schema. NOTHING covered reserved->schema, so a field could sit
	// in reservedStatusFields and be absent from the schema — the apiserver would prune the
	// controller's own write, which is #293 exactly.
	//
	// Proved by mutation, not by inspection: removing a field (it was lastReconcileTime, since
	// dropped by #439) from the schema while leaving it in reservedStatusFields passed the whole file. The surviving mutant is the only
	// reason this arm exists.
	//
	// And statusfields.go claimed `TestReservedStatusFieldsMatchSchema asserts the two agree` —
	// a test of that name has never existed in this repo (1 occurrence, in that comment; control:
	// the two tests that DO exist are each found by the same search). A comment citing a test by
	// name is worth less than the test, and reads as stronger.
	for _, name := range crdutils.ReservedStatusFieldNames() {
		if name == "conditions" {
			// The documented exception: crdgen emits conditions from the Go type rather than from
			// statics/status.schema.json, so it is legitimately reserved-but-not-declared.
			continue
		}
		if _, ok := declaredSet[name]; !ok {
			t.Errorf("status.%s is in reservedStatusFields but the baseline schema does not declare "+
				"it — the apiserver will PRUNE the controller's own write of it (#293, #273)", name)
		}
	}
}

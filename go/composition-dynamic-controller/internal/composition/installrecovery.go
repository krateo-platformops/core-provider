package composition

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/krateo-platformops/unstructured-runtime/pkg/logging"
	"github.com/krateo-platformops/unstructured-runtime/pkg/tools"
	unstructuredtools "github.com/krateo-platformops/unstructured-runtime/pkg/tools/unstructured"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// status.installRecovery (core-provider#636, Diego, 2026-11-23): the durable record that this
// controller recovered a first install by itself. Events expire and the attempt counter annotation is
// removed on success, so without it nothing on a healthy composition says its install hooks ran more
// than once.
//
// installRecoveryRetried is its only kind: an interrupted (#629) or failed (#636) first install was
// installed again over what it created. The reason begins with which one.
const installRecoveryRetried = "Retried"

// installRecoveryKeys is every key written under status.installRecovery. The composition CRD's status
// schema is CLOSED there (core-provider statics/status.schema.json): a key missing from it is pruned by
// the apiserver on every write. TestInstallRecoveryKeysAreDeclaredInTheGeneratedStatusSchema holds the
// two together.
var installRecoveryKeys = []string{"kind", "lastAt", "count", "reason", "revision"}

// installRecoveryCount reads the cumulative count. One that cannot be read is an error, never zero.
func installRecoveryCount(mg *unstructured.Unstructured) (int64, error) {
	v, found, err := unstructured.NestedFieldNoCopy(mg.Object, "status", "installRecovery", "count")
	if err != nil {
		return 0, fmt.Errorf("reading status.installRecovery.count: %w", err)
	}
	if !found {
		return 0, nil
	}
	switch n := v.(type) {
	case int64:
		return n, nil
	case float64:
		return int64(n), nil
	}
	return 0, fmt.Errorf("status.installRecovery.count is %v (%T), not a count", v, v)
}

// installRecoveryRevision is the revision the recorded retry is of; 0, which no helm revision is, when
// none is recorded or it cannot be read, so the next retry is counted.
func installRecoveryRevision(mg *unstructured.Unstructured) int64 {
	v, found, err := unstructured.NestedFieldNoCopy(mg.Object, "status", "installRecovery", "revision")
	if err != nil || !found {
		return 0
	}
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	}
	return 0
}

// recordInstallRecovery writes status.installRecovery BEFORE the recovery's first destructive step, and
// returns the object the write left. A write that fails stops the pass: no retry happens unrecorded,
// and a status that lost a race is recomputed by the next pass. The count is
// cumulative and nothing resets it; every later status write starts from an object that carries it.
//
// The count moves once per REVISION retried. A pass that records the retry of a revision and then stops
// (a hook Job that will not go, a refused call) is repeated by the next one, which has retried nothing
// more: without this, a composition blocked on one hook Job counts a retry per pass, and what a reader is
// told ("retried 40 times") is false. Every attempt that starts writes a new revision. The revision
// goes when one is deployed (clearInstallFailures), so a later release's revision 1 counts again.
//
// A CRD that predates the field PRUNES it: the write succeeds and the returned object does not carry it.
// That is not a failure (the CRD is core-provider's to roll, and may lag this controller for as long as
// the definition does), so the recovery goes ahead and the log says so once.
func recordInstallRecovery(ctx context.Context, mg *unstructured.Unstructured, revision int, reason string, now time.Time,
	opts tools.UpdateOptions, log logging.Logger) (*unstructured.Unstructured, error) {
	n, err := installRecoveryCount(mg)
	if err != nil {
		return mg, err
	}
	if installRecoveryRevision(mg) != int64(revision) {
		n++
	}
	if err := unstructured.SetNestedMap(mg.Object, map[string]any{
		"kind": installRecoveryRetried, "lastAt": now.UTC().Format(time.RFC3339), "count": n, "reason": reason, "revision": int64(revision),
	}, "status", "installRecovery"); err != nil {
		return mg, fmt.Errorf("setting status.installRecovery: %w", err)
	}
	// The message of InstallFailing names this count, so it moves in the same write.
	if cond, failing := installFailing(mg, "a retry is starting"); failing {
		if err := unstructuredtools.SetConditions(mg, cond); err != nil {
			return mg, fmt.Errorf("setting Ready: %w", err)
		}
	}
	updated, err := updateStatus(ctx, mg, opts)
	if err != nil {
		return mg, fmt.Errorf("recording the install recovery in status.installRecovery: %w", err)
	}
	if updated == nil {
		return mg, nil
	}
	if _, kept, _ := unstructured.NestedMap(updated.Object, "status", "installRecovery"); !kept {
		if _, said := installRecoveryPruned.LoadOrStore(updated.GetUID(), struct{}{}); !said {
			log.Info("status.installRecovery was pruned: the composition's CRD predates it, so this recovery is not recorded on the composition; recovering anyway",
				"reason", reason)
		}
	}
	return updated, nil
}

// installRecoveryPruned holds the UIDs of the compositions whose pruned record was logged.
var installRecoveryPruned sync.Map

package composition

import (
	"fmt"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
)

// recoveryPlan is what the stuck-operation recovery rolls a release back to (core-provider#563).
type recoveryPlan struct {
	// revision is the rollback target.
	revision int
	// hooksUnverified: no deployed revision precedes the pending one, so the release is rolled back
	// to the pending revision itself, and the hooks of the interrupted operation may not have run.
	hooksUnverified bool
	// why says, for the log and for Ready's message, how the target was chosen.
	why string
}

// recoveryTarget chooses the revision a release stuck in an incomplete helm operation is rolled back
// to, from the release as Observe read it and its stored history.
//
// Diego, 2026-11-17 (#563): roll back to the LAST DEPLOYED revision, the state before the interrupted
// operation, so the next reconcile upgrades to the desired values as a real upgrade and its hooks
// run. Rolling back to the pending revision itself (the behaviour before) re-applies the new
// manifests as a new revision and runs only rollback hooks; the next upgrade then finds nothing to
// change, and post-upgrade hooks that never ran never run.
//
// With NO deployed revision before the pending one (a first install, rev 1; after a failed rollback,
// which supersedes the current revision and marks its target failed, helm v3.20.2 rollback.go:197;
// or a reinstall after uninstall --keep-history), there is no earlier state. The release is rolled
// back to itself, which clears the lock and deletes nothing, and hooksUnverified says so.
//
// "Deployed" is helm's status, read from the history. helm never prunes the last deployed revision
// (pkg/storage/storage.go:179-196), so it can sit beyond MaxHistory; that is why this reads the whole
// stored history and not a window of revisions below the pending one.
//
// A stuck uninstall is out of scope: the revision being uninstalled is the deployed one, and
// rolling back to it, then re-driving the uninstall, is already right.
//
// Every inconsistency is an error, never a guess: a history that is empty, that lacks the pending
// revision, or that holds a revision above it (the release moved on since Observe read it) says this
// read cannot be trusted, and a rollback chosen from it could revert a newer operation.
func recoveryTarget(pending *helmconfig.Release, history []*helmconfig.Release) (recoveryPlan, error) {
	if pending == nil {
		return recoveryPlan{}, fmt.Errorf("choosing a recovery target: no release")
	}
	if len(history) == 0 {
		return recoveryPlan{}, fmt.Errorf("choosing a recovery target for revision %d: the release has no stored history", pending.Revision)
	}
	foundPending := false
	deployed := 0
	for _, r := range history {
		if r == nil {
			continue
		}
		switch {
		case r.Revision > pending.Revision:
			return recoveryPlan{}, fmt.Errorf("choosing a recovery target for revision %d: the history holds revision %d (%s), so the release moved on since it was read",
				pending.Revision, r.Revision, r.Status)
		case r.Revision == pending.Revision:
			foundPending = true
		case r.Status == helmconfig.StatusDeployed && r.Revision > deployed:
			deployed = r.Revision
		}
	}
	if !foundPending {
		return recoveryPlan{}, fmt.Errorf("choosing a recovery target for revision %d: the history does not hold it", pending.Revision)
	}

	if pending.Status == helmconfig.StatusUninstalling {
		return recoveryPlan{revision: pending.Revision,
			why: fmt.Sprintf("revision %d is being uninstalled; rolled back to itself to clear the lock, then the uninstall is re-driven", pending.Revision)}, nil
	}
	if deployed > 0 {
		return recoveryPlan{revision: deployed,
			why: fmt.Sprintf("the %s of revision %d was interrupted; rolled back to revision %d, the last deployed, so the next reconcile upgrades again and its hooks run",
				pending.Status, pending.Revision, deployed)}, nil
	}
	return recoveryPlan{revision: pending.Revision, hooksUnverified: true,
		why: fmt.Sprintf("the %s of revision %d was interrupted and no deployed revision precedes it, so it was rolled back to itself; its %s hooks may not have run",
			pending.Status, pending.Revision, interruptedHooks(pending.Status))}, nil
}

// interruptedHooks names the hooks an interrupted operation of this status runs, for the message.
func interruptedHooks(s helmconfig.Status) string {
	switch s {
	case helmconfig.StatusPendingInstall:
		return "install"
	case helmconfig.StatusPendingUpgrade:
		return "upgrade"
	case helmconfig.StatusPendingRollback:
		return "rollback"
	default:
		return string(s)
	}
}

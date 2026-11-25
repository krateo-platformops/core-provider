package composition

import (
	"sync"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
	"github.com/krateo-platformops/unstructured-runtime/pkg/logging"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Log lines of a failed release's heal (core-provider#640, plumbing v1.110.0). Info, as the drift
// line ("Composition drift detected and self-healed") is, and for its reason: detail at Debug is
// invisible on every cluster that runs at Info.
const (
	msgFailedReleaseHealed   = "Composition's failed release healed against what its failed revision applied"
	msgFailedReleaseNoRecord = "Composition's failed release cannot be healed: its failed revision carries no record of what was applied " +
		"(written before plumbing v1.110.0, or by helm alone), so drift on its objects stays until an attempt gets past its objects step"
	msgFailedReleaseHealFailed = "Composition's failed release could not be healed"
)

// reportFailedReleaseHeal logs what plumbing's heal of a FAILED release did, before it retried the
// upgrade (core-provider#640). Called right after every Reconcile, on both outcomes: when the retry
// fails, as it does for as long as a pre-upgrade hook keeps failing, the pass returns that error and
// never reaches the drift line, so the heal would otherwise be said nowhere. When the retry succeeds,
// plumbing drops the heal's own error (it returns it only joined to the retry's), and this is the only
// place it is said.
//
// Bounded as the drift line is: plumbing caps ChangedObjects at ten identities and ChangedPaths at
// five per child, with omitted counts. The heal's writes are also in the top-level counts and in
// Objects; this line says which were the heal's.
//
//   - recorded, something written or an error: one line per pass that heals;
//   - recorded, nothing to write: silent, the steady state of a release whose hook keeps failing;
//   - not recorded, no error: once per composition until it leaves that state (a recorded heal, or a
//     Reconcile that is not a failed release's retry). NOT once per revision: while a pre-upgrade hook
//     keeps failing, every retry writes a new failed revision, and it too has no record (what was
//     applied before it is unknown), so a line per revision is a line per pass. A process restart says
//     it once more;
//   - not recorded, with an error: the heal could not tell, because the record could not be read or
//     the revision was superseded after Reconcile read it. Every pass, with the error, and the
//     once-per-composition flag is neither set nor cleared.
func reportFailedReleaseHeal(mg *unstructured.Unstructured, res *helmconfig.ReconcileResult, release string, log logging.Logger) {
	if res == nil {
		return // the Reconcile failed before it could say anything: the state is unknown, so is kept
	}
	heal := res.FailedReleaseHeal
	if heal == nil || heal.Recorded {
		failedReleaseNoRecordSaid.Delete(mg.GetUID())
	}
	if heal == nil {
		return
	}
	fields := []any{"release", release, "revision", heal.Revision, "recorded", heal.Recorded}
	if !heal.Recorded && heal.Err != nil {
		// The heal could not tell whether the revision is recorded: it could not be read, or another
		// revision was written after Reconcile read the release (plumbing v1.111.0 says which). Not "no
		// record", and the flag below is left as it is.
		log.Info(msgFailedReleaseHealFailed, append(fields, "error", heal.Err.Error())...)
		return
	}
	if !heal.Recorded && heal.Err == nil {
		if _, said := failedReleaseNoRecordSaid.LoadOrStore(mg.GetUID(), heal.Revision); said {
			return
		}
		log.Info(msgFailedReleaseNoRecord, fields...)
		return
	}
	if heal.Created+heal.PatchedUpdated == 0 && heal.Err == nil {
		return
	}
	fields = append(fields,
		"created", heal.Created, "patched", heal.PatchedUpdated,
		"changedObjects", heal.ChangedObjects, "changedObjectsOmitted", heal.ChangedObjectsOmitted,
		"changedPaths", heal.ChangedPaths, "changedPathsOmitted", heal.ChangedPathsOmitted)
	if heal.Err != nil {
		fields = append(fields, "error", heal.Err.Error())
	}
	log.Info(msgFailedReleaseHealed, fields...)
}

// failedReleaseNoRecordSaid holds the UIDs of the compositions whose unrecorded failed release was
// logged, each with the revision it was first logged for.
var failedReleaseNoRecordSaid sync.Map

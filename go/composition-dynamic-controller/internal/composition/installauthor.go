package composition

import (
	"context"
	"fmt"
	"strings"
	"time"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
	"github.com/krateo-platformops/unstructured-runtime/pkg/logging"
	"github.com/krateo-platformops/unstructured-runtime/pkg/tools"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// core-provider#636: never deployed is not enough to act on a release. The krateo.io/release-name label
// is write-once, and adoption can settle on a release that carries no composition labels
// (pickAdoptableRelease), so a composition can point at a release a HUMAN installed. Ending that
// install, or installing over it, is not this controller's to do: the recoveries also need proof that
// this controller started it.

// recordedInstall says whether status.helmOperation is THIS revision's install: phase install, recorded
// within orphanWindow before the revision was written (orphanedOperation's own test, without its
// question of whether the starter is alive). Absent, another phase, another operation's or unreadable is
// no proof.
func recordedInstall(mg *unstructured.Unstructured, rel *helmconfig.Release) (bool, string) {
	if rel == nil {
		return false, "no release"
	}
	rec, found, err := unstructured.NestedStringMap(mg.Object, "status", "helmOperation")
	if err != nil {
		return false, "the recorded operation cannot be read: " + err.Error()
	}
	if !found {
		return false, "no operation recorded, so this controller is not known to have started the install"
	}
	if rec["phase"] != "install" {
		return false, fmt.Sprintf("the recorded operation is %q, not an install", rec["phase"])
	}
	started, err := time.Parse(time.RFC3339Nano, rec["startedAt"])
	if err != nil {
		return false, "the recorded install has an unreadable startedAt"
	}
	if gap := operationClock(rel).Sub(started); gap < 0 || gap > orphanWindow {
		return false, fmt.Sprintf("the recorded install is not revision %d's (recorded %s before it)", rel.Revision, gap)
	}
	return true, ""
}

// startedByController says whether this controller is known to have started the install of a release
// whose latest revision FAILED: the operation record of that revision, left by a process that died in
// it, or the never-deployed mark, which is only ever written once that is known (after this
// controller's own install call failed, or from that record).
func startedByController(mg *unstructured.Unstructured, releaseName string, rel *helmconfig.Release) (bool, string) {
	ok, why := recordedInstall(mg, rel)
	if ok {
		return true, ""
	}
	if through, _ := readNeverDeployedMark(mg, releaseName); through > 0 && through <= rel.Revision {
		return true, ""
	}
	return false, why
}

// installNameInUse is helm's refusal of an install over a release that is not a failed one (helm v3
// install.go availableName, untyped): it wrote nothing, and what holds the name is not this call's.
func installNameInUse(err error) bool {
	return err != nil && strings.Contains(err.Error(), "cannot re-use a name that is still in use")
}

// startedNothing says whether an install call's error is nil or a refusal: it wrote no revision, and what
// is there, if anything, is not this call's.
func startedNothing(callErr error) bool {
	return callErr == nil || installNameInUse(callErr) || retryRefused(callErr)
}

// afterFailedInstall is everything recorded once this controller's own install call (started at started)
// failed: the never-deployed mark, and the failed attempt with its Ready (recordInstallFailure). The
// operation is over, so its record goes, unless it is what proves the install is this controller's: the
// mark could not be written, and a status write would remove the one thing that stands in for it.
func afterFailedInstall(ctx context.Context, mg *unstructured.Unstructured, hc helmconfig.Client, releaseName string, started time.Time,
	callErr error, opts tools.UpdateOptions, log logging.Logger) (*unstructured.Unstructured, error) {
	if startedNothing(callErr) {
		return mg, callErr
	}
	mg = markOwnFailedInstall(ctx, mg, hc, releaseName, started, callErr, opts, log)
	if through, _ := readNeverDeployedMark(mg, releaseName); through > 0 {
		clearOwnHelmOperationRecord(mg) // the mark's patch returned the stored object, which still carries it
	} else {
		setHelmOperationRecord(mg, "install", started)
	}
	return recordInstallFailure(ctx, mg, callErr, time.Now(), opts, log)
}

// markOwnFailedInstall marks the release never deployed after this controller's own install call
// (started at started) failed and left a failed revision. Best effort, and nothing depends on it alone:
// helm has already run, and a graceful stop cancels this write too, in which case the operation record
// the same call wrote is still there (recordedInstall). A refused call wrote no revision, so it proves
// nothing about the one that is there.
func markOwnFailedInstall(ctx context.Context, mg *unstructured.Unstructured, hc helmconfig.Client, releaseName string, started time.Time,
	callErr error, opts tools.UpdateOptions, log logging.Logger) *unstructured.Unstructured {
	if startedNothing(callErr) {
		return mg
	}
	marked, why := mg, ""
	rel, err := hc.GetRelease(ctx, releaseName, &helmconfig.GetConfig{})
	switch {
	case err != nil:
		why = err.Error()
	case rel == nil:
		return mg // it failed before helm stored anything: there is nothing to retry over
	case rel.Status != helmconfig.StatusFailed || rel.Updated.Before(started):
		why = fmt.Sprintf("%s is not the revision this call wrote", releaseState(rel))
	default:
		history, herr := hc.History(ctx, releaseName, &helmconfig.HistoryConfig{})
		if herr != nil {
			why = herr.Error()
			break
		}
		ok, w := neverDeployed(mg, releaseName, rel, history)
		if !ok {
			why = w
			break
		}
		if marked, err = markNeverDeployed(ctx, mg, releaseName, rel, opts); err != nil {
			marked, why = mg, err.Error()
		}
	}
	if why != "" {
		log.Info("The install failed and was not marked as this controller's; the operation record stands in for the mark while it is there",
			"release", releaseName, "why", why)
	}
	return marked
}

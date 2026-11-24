package composition

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/krateo-platformops/composition-dynamic-controller/internal/conditionreason"
	"github.com/krateo-platformops/unstructured-runtime/pkg/controller"
	"github.com/krateo-platformops/unstructured-runtime/pkg/logging"
	"github.com/krateo-platformops/unstructured-runtime/pkg/tools"
	unstructuredtools "github.com/krateo-platformops/unstructured-runtime/pkg/tools/unstructured"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Ready=False/InstallFailing (core-provider#636, Diego 2026-11-23): a first install that keeps failing.
// Creating is converging with no deadline, so a hook that never passes would read Creating for ever. From
// the 3rd consecutive FAILED attempt Ready says so, and a parent counts it failed; the first two stay
// Creating; the attempts go on at the error backoff, and it clears when one succeeds.
//
// The count is status.installFailures, its own value: status.installRecovery.count is cumulative, never
// reset, and counts the retries of interrupted installs, while this one counts consecutive failures only
// (an interruption is not one; it has its own cap) and goes when a revision is deployed. On the composition
// so that it survives a restart, with the last error so that it can be read while a retry is running.
//
// IT DOES NOT FLAP BETWEEN ATTEMPTS. What is written while an attempt runs is HelmOperationInProgress,
// which a parent counts converging: past the threshold Ready keeps InstallFailing instead, and its message
// says a retry is running (installFailing).

// installFailingAfter is the consecutive failed attempt from which Ready reads InstallFailing.
const installFailingAfter = 3

// maxLastErrorBytes bounds status.installFailures.lastError and the message that quotes it: helm joins
// the error of every object and hook.
const maxLastErrorBytes = 1024

// installFailureKeys is every key written under status.installFailures. As installRecoveryKeys: the CRD's
// schema is closed, and a key missing from it is pruned.
var installFailureKeys = []string{"count", "lastAt", "lastError"}

// installFailureCount is the consecutive failed attempts recorded on mg. One that cannot be read is 0: the
// count decides only which reason a failure is reported under.
func installFailureCount(mg *unstructured.Unstructured) int64 {
	n, _, _ := unstructured.NestedInt64(mg.Object, "status", "installFailures", "count")
	return n
}

// installFailing is Ready=False/InstallFailing for a composition whose first install reached the
// threshold, or false. doing says what is happening now, "" between attempts. It names both numbers, which
// differ: failed attempts, and automatic retries, of which some were of interrupted installs.
func installFailing(mg *unstructured.Unstructured, doing string) (metav1.Condition, bool) {
	n := installFailureCount(mg)
	if n < installFailingAfter {
		return metav1.Condition{}, false
	}
	retries, err := installRecoveryCount(mg)
	if err != nil {
		retries = 0
	}
	last, _, _ := unstructured.NestedString(mg.Object, "status", "installFailures", "lastError")
	msg := fmt.Sprintf("the first install has failed %d times (%d automatic retries so far); last error: %s", n, retries, last)
	if doing != "" {
		msg += "; " + doing
	}
	return conditionreason.InstallFailing.WithMessage(msg), true
}

// installFailed is an attempt that genuinely failed, with the Ready it was recorded under.
type installFailed struct {
	err   error
	ready metav1.Condition
	// recorded: the status write that carries the count and Ready succeeded.
	recorded bool
}

func (f *installFailed) Error() string { return f.err.Error() }
func (f *installFailed) Unwrap() error { return f.err }

// recordInstallFailure counts a failed attempt of a first install and records Ready, in ONE status write,
// and returns the object to carry on with and callErr carrying what was recorded. A call that started
// nothing (refused, or the name is in use), and one a graceful stop cancelled, is not a failed attempt
// and is returned as it is.
func recordInstallFailure(ctx context.Context, mg *unstructured.Unstructured, callErr error, now time.Time,
	opts tools.UpdateOptions, log logging.Logger) (*unstructured.Unstructured, error) {
	if startedNothing(callErr) || ctx.Err() != nil {
		return mg, callErr
	}
	if err := unstructured.SetNestedMap(mg.Object, map[string]any{
		"count": installFailureCount(mg) + 1, "lastAt": now.UTC().Format(time.RFC3339), "lastError": boundedError(callErr),
	}, "status", "installFailures"); err != nil {
		log.Info("Could not count a failed install attempt", "error", err.Error())
		return mg, callErr
	}
	ready, failing := installFailing(mg, "")
	if !failing {
		ready = conditionreason.Creating.WithMessage(boundedError(callErr))
	}
	failed := &installFailed{err: callErr, ready: ready}
	if err := unstructuredtools.SetConditions(mg, ready); err != nil {
		log.Info("Could not set Ready for a failed install attempt", "error", err.Error())
		return mg, failed
	}
	updated, err := updateStatus(ctx, mg, opts)
	if err != nil {
		log.Info("Could not record a failed install attempt: it is not counted", "error", err.Error())
		return mg, failed
	}
	failed.recorded = true
	if updated == nil {
		return mg, failed
	}
	return updated, failed
}

// clearInstallFailures forgets the failed attempts, in memory: the next status write persists it. Called
// where a revision of the release is known to be deployed. So is the revision the last retry was of: the
// count it belongs to stays (installRecovery is never reset), but a later release starts at revision 1.
func clearInstallFailures(mg *unstructured.Unstructured) {
	unstructured.RemoveNestedField(mg.Object, "status", "installFailures")
	unstructured.RemoveNestedField(mg.Object, "status", "installRecovery", "revision")
}

// boundedError is err's text, at most maxLastErrorBytes of it, cut at a character.
func boundedError(err error) string {
	s := strings.TrimSpace(err.Error())
	if len(s) <= maxLastErrorBytes {
		return s
	}
	s = s[:maxLastErrorBytes]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + "..."
}

// CreateWithResult implements controller.ResultCreator, for the conditions a FAILED create ends with. The
// runtime writes Ready=False/Creating on the object it re-fetches after a failed create, so what this handler
// set during the call is lost; what it RETURNS is applied after (unstructured-runtime v1.32.1).
// Requested is always true: what a create did is judged by its error alone, as for a plain Create.
func (h *handler) CreateWithResult(ctx context.Context, mg *unstructured.Unstructured) (controller.CreateResult, error) {
	err := h.Create(ctx, mg)
	return controller.CreateResult{Requested: true, Conditions: failedCreateConditions(err)}, err
}

var _ controller.ResultCreator = (*handler)(nil)

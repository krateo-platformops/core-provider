package composition

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/krateo-platformops/plumbing/env"
	helmconfig "github.com/krateo-platformops/plumbing/helm"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/krateo-platformops/unstructured-runtime/pkg/logging"
	"github.com/krateo-platformops/unstructured-runtime/pkg/tools"
	unstructuredtools "github.com/krateo-platformops/unstructured-runtime/pkg/tools/unstructured"
	"github.com/krateo-platformops/unstructured-runtime/pkg/tools/unstructured/condition"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// helmOperationInProgress is the Ready condition written BEFORE an install or uninstall starts
// (#410). On krateo-057 a composition's install sat on a hook that could never start for three
// hours with no condition at all, and its deletion waited behind it just as silently. The message
// says what is running, since when, and the bound that applies to each of its hooks and waits.
func helmOperationInProgress(phase string, started time.Time, bound time.Duration) metav1.Condition {
	c := condition.FailWithReason(reasonHelmOperationInProgress)
	c.Message = fmt.Sprintf("helm %s in progress since %s; each hook and each wait must finish within %s, or the operation fails",
		phase, started.UTC().Format(time.RFC3339), bound)
	return c
}

// recordHelmOperation writes helmOperationInProgress to mg's status and returns the updated object,
// so the caller's later writes start from the current resourceVersion.
//
// Only for install (Create) and uninstall (Delete). Update and Observe's reconcile run on live
// compositions every time something drifts, and writing Ready=False before each of those would make
// Ready flap on every self-heal; their operations are still bounded by helmOperationTimeout.
//
// A status conflict is returned (the runtime requeues quietly and the next pass writes it), any other
// write failure is logged and the operation goes ahead: the condition is a report, not a gate. Same
// rule as the PendingHelmOperation write (pendingConditionWriteOutcome).
func recordHelmOperation(ctx context.Context, mg *unstructured.Unstructured, phase string, started time.Time,
	opts tools.UpdateOptions, log logging.Logger) (*unstructured.Unstructured, error) {
	unstructuredtools.SetConditions(mg, helmOperationInProgress(phase, started, helmOperationTimeout))
	setHelmOperationRecord(mg, phase, started) // #461: who started it, in the same write
	updated, err := updateStatus(ctx, mg, opts)
	if err != nil {
		if werr := pendingConditionWriteOutcome(err); werr != nil {
			return mg, werr
		}
		log.Debug("Could not record the helm operation in progress", "phase", phase, "error", err.Error())
		return mg, nil
	}
	if updated == nil {
		return mg, nil
	}
	return updated, nil
}

// operationClock is when rel's incomplete operation began: helm's Info.Deleted for an uninstalling
// revision (helm stamps it as the uninstall starts), and Info.LastDeployed (rel.Updated) for a
// pending install, upgrade or rollback, where helm writes the pending revision as the operation
// starts. An uninstall never moves LastDeployed, so measuring it from rel.Updated measured from the
// DEPLOY (#497): a release deployed hours ago read as stuck the moment its uninstall began, and the
// operation record, written just before the uninstall, never matched the revision. A zero Deleted
// on an uninstalling revision (a release written before helm stamped it) falls back to Updated.
func operationClock(rel *helmconfig.Release) time.Time {
	if rel.Status == helmconfig.StatusUninstalling && !rel.Deleted.IsZero() {
		return rel.Deleted
	}
	return rel.Updated
}

// podName / podNamespace identify THIS controller pod (POD_NAME / POD_NAMESPACE, from the downward API
// on the cdc Deployment). Empty when unset (tests, an older chart): then no starter is recorded and
// the orphan check never fires, which is today's behaviour.
var (
	podName      = env.String("POD_NAME", "")
	podNamespace = env.String("POD_NAMESPACE", "")
)

// orphanWindow bounds how long before the pending revision's LastDeployed a record may have been
// written and still be ITS record (#461). The cdc writes the record immediately before calling helm,
// and both timestamps come from the same starter pod's clock, so the real gap is the record write
// plus helm's start: well under a minute. A record outside the window belongs to some other
// operation and is never acted on.
const orphanWindow = time.Minute

// setHelmOperationRecord writes status.helmOperation (#461): the phase, when, and which pod. It is
// written in the same status write as the HelmOperationInProgress condition, before helm runs.
func setHelmOperationRecord(mg *unstructured.Unstructured, phase string, started time.Time) {
	rec := map[string]any{"phase": phase, "startedAt": started.UTC().Format(time.RFC3339Nano)}
	if podName != "" && podNamespace != "" {
		rec["startedBy"] = podNamespace + "/" + podName
	}
	_ = unstructured.SetNestedMap(mg.Object, rec, "status", "helmOperation")
}

// markHelmOperationReturned stamps status.helmOperation.returnedAt (#497): this controller's call for
// the recorded operation RETURNED with an error, and helm's lock (e.g. uninstalling) was left behind.
// Nothing is in flight any more, which the record alone could not say: "started by this pod" was
// read as "maybe still running", so the next Delete waited the full pending grace (16m) for a call
// that had already returned. No-op without a record.
func markHelmOperationReturned(mg *unstructured.Unstructured, returned time.Time) {
	if _, found, _ := unstructured.NestedStringMap(mg.Object, "status", "helmOperation"); !found {
		return
	}
	_ = unstructured.SetNestedField(mg.Object, returned.UTC().Format(time.RFC3339Nano), "status", "helmOperation", "returnedAt")
}

// helmOperationRecordKeys is every key this file writes under status.helmOperation. The composition
// CRD's status schema is CLOSED there (core-provider statics/status.schema.json), so a key missing
// from it is pruned by the apiserver on every write and the behaviour built on it silently never
// engages. helmoperation_schema_test.go holds the two together.
var helmOperationRecordKeys = []string{"phase", "startedAt", "startedBy", "returnedAt"}

// returnedOperation reports whether rel's lock was left by a recorded operation whose call has
// RETURNED (#497, Diego 2026-11-14), so it can be cleared now instead of after the pending grace.
// ALL must hold, and anything else is false (keep the grace):
//   - status.helmOperation exists, for the given phase, and its startedAt is within orphanWindow
//     BEFORE operationClock(rel), so the record is THIS operation's (the same test as orphanedOperation);
//   - it carries returnedAt, at or after startedAt.
//
// A record from a controller that predates returnedAt, or one an older CRD pruned it from, carries
// none: the grace applies, which is the behaviour before #497.
func returnedOperation(mg *unstructured.Unstructured, rel *helmconfig.Release, phase string) (bool, string) {
	if rel == nil {
		return false, "no release"
	}
	rec, found, err := unstructured.NestedStringMap(mg.Object, "status", "helmOperation")
	if err != nil || !found {
		return false, "no operation recorded"
	}
	if rec["phase"] != phase {
		return false, fmt.Sprintf("the recorded operation is %q, not %q", rec["phase"], phase)
	}
	started, err := time.Parse(time.RFC3339Nano, rec["startedAt"])
	if err != nil {
		return false, "unreadable startedAt"
	}
	if gap := operationClock(rel).Sub(started); gap < 0 || gap > orphanWindow {
		return false, fmt.Sprintf("the record is not this revision's (recorded %s before it)", gap)
	}
	if rec["returnedAt"] == "" {
		return false, "the call has not been recorded as returned"
	}
	returned, err := time.Parse(time.RFC3339Nano, rec["returnedAt"])
	if err != nil {
		return false, "unreadable returnedAt"
	}
	if returned.Before(started) {
		return false, "returnedAt precedes startedAt"
	}
	return true, "this operation's call returned at " + rec["returnedAt"]
}

// clearHelmOperationRecord removes status.helmOperation in memory once the operation has returned;
// the next status write persists the removal. A record that stays (no further write) is harmless:
// orphanedOperation only acts on a record that belongs to the pending revision it is looking at.
func clearHelmOperationRecord(mg *unstructured.Unstructured) {
	unstructured.RemoveNestedField(mg.Object, "status", "helmOperation")
}

// podLookup reports a pod's existence: nil if it exists, a NotFound error if it does not, any other
// error if that could not be determined.
type podLookup func(ctx context.Context, namespace, name string) error

func podLookupFrom(dyn dynamic.Interface) podLookup {
	return func(ctx context.Context, namespace, name string) error {
		_, err := dyn.Resource(schema.GroupVersionResource{Version: "v1", Resource: "pods"}).
			Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
		return err
	}
}

// orphanedOperation reports whether rel's incomplete operation was started by a controller pod that
// no longer exists (#461, Diego 2026-11-13), so the lock can be cleared now instead of after the
// pending grace. ALL must hold, and anything else is false (keep the grace):
//   - status.helmOperation exists and its startedAt is within orphanWindow BEFORE operationClock(rel) (the
//     revision's LastDeployed), so the record is THIS operation's and not a stale one a live pod failed
//     to overwrite;
//   - it names a starter pod, and that pod is not this one;
//   - looking that pod up returns NotFound. Any other error is "could not determine", never "dead".
//
// The reason is for the log line.
func orphanedOperation(ctx context.Context, mg *unstructured.Unstructured, rel *helmconfig.Release, lookup podLookup) (bool, string) {
	if rel == nil || lookup == nil {
		return false, "no release or no lookup"
	}
	rec, found, err := unstructured.NestedStringMap(mg.Object, "status", "helmOperation")
	if err != nil || !found {
		return false, "no operation recorded"
	}
	started, err := time.Parse(time.RFC3339Nano, rec["startedAt"])
	if err != nil {
		return false, "unreadable startedAt"
	}
	if gap := operationClock(rel).Sub(started); gap < 0 || gap > orphanWindow {
		return false, fmt.Sprintf("the record is not this revision's (recorded %s before it)", gap)
	}
	ns, name, ok := strings.Cut(rec["startedBy"], "/")
	if !ok || ns == "" || name == "" {
		return false, "no starter recorded"
	}
	if ns == podNamespace && name == podName {
		return false, "started by this pod"
	}
	switch lerr := lookup(ctx, ns, name); {
	case lerr == nil:
		return false, "the starter pod " + rec["startedBy"] + " is alive"
	case apierrors.IsNotFound(lerr):
		return true, "the starter pod " + rec["startedBy"] + " no longer exists"
	default:
		return false, "could not look up the starter pod: " + lerr.Error()
	}
}

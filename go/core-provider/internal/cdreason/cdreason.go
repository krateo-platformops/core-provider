// Package cdreason is the closed set of reasons a CompositionDefinition's Ready and Synced
// conditions can carry, and the only place in core-provider that constructs a CompositionDefinition's
// Ready condition. It is the CompositionDefinition's counterpart of the cdc's internal/conditionreason
// (core-provider#515), for the same reason: consumers gate on Ready's reason, and until this package
// the set existed only as whatever the code happened to write.
//
// A CompositionDefinition IS read by a parent rollup. installer's umbrella composition renders most
// of a cluster's definitions (on krateo-057, 69 of 76), and the cdc's child rollup reads every
// *.krateo.io child's Ready (composition-dynamic-controller internal/composition/childhealth.go,
// krateoEval). So every reason here declares the parent verdict that rollup gives it, and the cdc's
// TestCompositionDefinitionReasonsRollUpAsDeclared reads the rendered docs/cd-ready-reasons.md and
// fails when krateoEval disagrees with a row. A new reason without a rollup arm falls to krateoEval's
// default, converging, and would hold the umbrella at Creating for as long as it lasts.
//
// Enforced by construction, as in the cdc:
//
//   - Reason has unexported fields, so code outside this package cannot make one.
//   - TestNoReadyConditionBuiltOutsideCdreason refuses any other construction of a Ready condition
//     in the compositiondefinitions controller, which the type alone cannot stop.
//   - TestCDReadyReasonTableIsCurrent renders Table into docs/cd-ready-reasons.md and fails while the
//     file is stale.
//
// The set also lists the reasons provider-runtime's managed reconciler writes on the same objects.
// core-provider cannot construct those through this package (their values are unexported); they are
// listed because a consumer reading a definition sees them all the same.
package cdreason

import (
	"errors"

	"github.com/krateo-platformops/plumbing/conditions"
	rtv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The reason literals core-provider owns. The runtime's are its own constants.
const (
	// RemoteChartUnreachable: a remote definition's chart is cluster-local to the management
	// cluster, so the spoke cannot fetch it.
	ReasonRemoteChartUnreachable = "RemoteChartUnreachable"
	// SchemaDefaultContradiction: the chart's values.schema.json has a `default` that crdgen would
	// carry into the CRD and that does not satisfy the subschema it is written in, so no CRD can be
	// generated (plumbing#205, *crdgen.DefaultContradictionError). A config error in the chart.
	ReasonSchemaDefaultContradiction = "SchemaDefaultContradiction"
)

// Writer is who writes a reason on a CompositionDefinition.
type Writer string

const (
	WriterCoreProvider Writer = "core-provider"
	WriterRuntime      Writer = "provider-runtime"
)

// Verdict is how a parent composition's rollup reads a CompositionDefinition child carrying a Ready
// reason. The strings are the cdc's, as docs/ready-reasons.md prints them.
type Verdict string

const (
	Healthy     Verdict = "healthy"
	Converging  Verdict = "converging"
	Failed      Verdict = "failed"
	Unevaluated Verdict = "unevaluated (rolls up as healthy)"
	// NotRead is the verdict column of a condition the rollup does not read (Synced).
	NotRead Verdict = "—"
)

// Reason is one reason a CompositionDefinition's condition can carry, with the status it is always
// written with.
type Reason struct {
	condType rtv1.ConditionType
	reason   rtv1.ConditionReason
	status   metav1.ConditionStatus
	writer   Writer
	verdict  Verdict
	meaning  string
}

// String is the reason as written on the condition.
func (r Reason) String() string { return string(r.reason) }

// Type is the condition the reason is written on.
func (r Reason) Type() rtv1.ConditionType { return r.condType }

// Status is the status the reason is always written with.
func (r Reason) Status() metav1.ConditionStatus { return r.status }

// Writer is who writes it.
func (r Reason) Writer() Writer { return r.writer }

// ParentVerdict is how a parent composition's rollup reads a definition carrying it.
func (r Reason) ParentVerdict() Verdict { return r.verdict }

// Meaning is one line on when it is written.
func (r Reason) Meaning() string { return r.meaning }

// Condition is the condition carrying r, with no message.
func (r Reason) Condition() rtv1.Condition {
	return rtv1.Condition{
		Type:               r.condType,
		Status:             r.status,
		LastTransitionTime: metav1.Now(),
		Reason:             r.reason,
	}
}

// WithMessage is r's condition with message.
func (r Reason) WithMessage(message string) rtv1.Condition {
	c := r.Condition()
	c.Message = message
	return c
}

// Is reports whether c carries r: the same type, reason and status.
func (r Reason) Is(c rtv1.Condition) bool {
	return c.Type == r.condType && c.Reason == r.reason && c.Status == r.status
}

// The Ready reasons core-provider writes.
var (
	Available = ready(rtv1.ReasonAvailable, metav1.ConditionTrue, WriterCoreProvider, Healthy,
		"Observe: the CRD serves the version and is Established, and the controller Deployment has rolled out and is available for its current generation (Diego, 2026-11-16); on a follower, the shared controller, and the message names the definition that owns it")
	ScaledToZero = ready(conditions.ReasonScaledToZero, metav1.ConditionTrue, WriterCoreProvider, Healthy,
		"Observe: the controller is at 0 replicas and the definition opted into scale-to-zero (#393); it wakes on the first instance")
	Creating = ready(rtv1.ReasonCreating, metav1.ConditionFalse, WriterCoreProvider, Converging,
		"Observe: a first pass (seeded before any work), the CRD does not exist yet or is not yet Established, the controller Deployment is absent or rolling out, an update pending with no earlier verdict to keep (#395), or the definition has just become the controller's owner (#387). Update: the pass that switches status to a new version, until Observe looks at its controller")
	Unavailable = ready(rtv1.ReasonUnavailable, metav1.ConditionFalse, WriterCoreProvider, Failed,
		"Observe: the chart cannot be read, the controller has 0 replicas without opting into scale-to-zero, the controller Deployment gave up its rollout (ProgressDeadlineExceeded) or lost availability after a completed one, or adding the served version is refused (a held version, no room); the message says which, and for lost availability ends with the first not-ready pod's cause, e.g. \"; pod <name>: restarting (last exit: Error)\" (#559)")
	RemoteChartUnreachable = ready(ReasonRemoteChartUnreachable, metav1.ConditionFalse, WriterCoreProvider, Failed,
		"remote seed: the chart's URL is cluster-local to the management cluster, so the remote target cannot fetch it")
	SchemaDefaultContradiction = ready(ReasonSchemaDefaultContradiction, metav1.ConditionFalse, WriterCoreProvider, Failed,
		"Observe: a `default` in the chart's values.schema.json that crdgen carries into the CRD fails its own subschema, so no CRD is generated; the message names each JSON pointer (plumbing#205). Nothing is applied, and a definition that worked before keeps serving its last CRD")
	Deleting = ready(rtv1.ReasonDeleting, metav1.ConditionFalse, WriterCoreProvider, Converging,
		"Delete: the definition is being deleted")
)

// The reasons provider-runtime's managed reconciler writes on the same objects. Unexported: core-provider
// must not write them itself.
var (
	runtimeNotObserved = ready(rtv1.ReasonNotObserved, metav1.ConditionUnknown, WriterRuntime, Unevaluated,
		"Connect or Observe returned an error not marked ReadyAssessed (for example, the controller Deployment could not be read); the error is on Synced")
	runtimeCreating = ready(rtv1.ReasonCreating, metav1.ConditionFalse, WriterRuntime, Converging,
		"the create path: written with every Create outcome, success or failure (the error, if any, is on Synced)")
	runtimeDeleting = ready(rtv1.ReasonDeleting, metav1.ConditionFalse, WriterRuntime, Converging,
		"the delete path, and an Observe error on a definition being deleted")

	runtimeSyncedSuccess = on(rtv1.TypeSynced, rtv1.ReasonReconcileSuccess, metav1.ConditionTrue, WriterRuntime,
		"the pass completed")
	runtimeSyncedError = on(rtv1.TypeSynced, rtv1.ReasonReconcileError, metav1.ConditionFalse, WriterRuntime,
		"the pass failed (message: the error); with Ready=False/SchemaDefaultContradiction it reads \"observe failed: \" followed by Ready's message")
	runtimeSyncedPaused = on(rtv1.TypeSynced, rtv1.ReasonReconcilePaused, metav1.ConditionFalse, WriterRuntime,
		"krateo.io/paused is set; Ready is left as it was")
)

func ready(reason rtv1.ConditionReason, status metav1.ConditionStatus, writer Writer, verdict Verdict, meaning string) Reason {
	return Reason{condType: rtv1.TypeReady, reason: reason, status: status, writer: writer, verdict: verdict, meaning: meaning}
}

func on(condType rtv1.ConditionType, reason rtv1.ConditionReason, status metav1.ConditionStatus, writer Writer, meaning string) Reason {
	return Reason{condType: condType, reason: reason, status: status, writer: writer, verdict: NotRead, meaning: meaning}
}

// Table is every reason a CompositionDefinition's Ready and Synced can carry, core-provider first, in
// a fixed order.
func Table() []Reason {
	return []Reason{
		Available, ScaledToZero, Creating, Unavailable, RemoteChartUnreachable, SchemaDefaultContradiction, Deleting,
		runtimeNotObserved, runtimeCreating, runtimeDeleting,
		runtimeSyncedSuccess, runtimeSyncedError, runtimeSyncedPaused,
	}
}

// RuntimeConstructors are provider-runtime's own constructors for the reasons it writes, so a test can
// assert each listed status against what the runtime actually builds.
func RuntimeConstructors() []rtv1.Condition {
	return []rtv1.Condition{
		rtv1.NotObserved(), rtv1.Creating(), rtv1.Deleting(),
		rtv1.ReconcileSuccess(), rtv1.ReconcileError(errors.New("e")), rtv1.ReconcilePaused(),
	}
}

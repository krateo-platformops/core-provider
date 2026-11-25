// Package conditionreason is the closed set of reasons a composition's conditions can carry
// (core-provider#515), and the only place in the composition-dynamic-controller that constructs a
// Ready condition.
//
// Consumers gate on Ready's reason with no else-arm: installer's dependency gate and verifier refuse a
// reason they do not know, by design. Until this package the set existed only as whatever the code
// happened to write, and consumers learned it one incident at a time (InventoryNotRecorded,
// HelmOperationInProgress). Here it is declared, and enforced by construction:
//
//   - Reason has unexported fields, so code outside this package cannot make one. A cdc Ready write
//     must name one of the values below, and a reason outside the set does not compile.
//   - TestNoReadyConditionBuiltOutsideReadyreason (internal/composition) refuses any other
//     construction of a Ready condition in the module, which the type alone cannot stop.
//   - TestReadyReasonTableIsCurrent renders Table, with each reason's verdict as the cdc's own child
//     rollup computes it, into docs/ready-reasons.md and docs/ready-reasons.json, and fails while
//     either file is stale. Those files, at a tag, are what a consumer diffs its classifier against
//     (core-provider#667: the JSON adds each reason's since-version, the action a person takes and
//     the status fields carrying its detail).
//   - TestReleasedReasonsAreStillListed fails when a reason a release carried
//     (testdata/released-reasons.txt) is no longer in Table and Deprecated does not name it: a
//     consumer keys on the reason string, so a rename or a removal is breaking.
//
// The set also lists the reasons unstructured-runtime writes on these same compositions. The cdc
// cannot construct those (their values are unexported); they are listed because a consumer reading a
// composition sees them all the same.
package conditionreason

import (
	"errors"
	compositioncondition "github.com/krateo-platformops/composition-dynamic-controller/internal/condition"
	"github.com/krateo-platformops/plumbing/managementpolicy"
	"github.com/krateo-platformops/unstructured-runtime/pkg/tools/unstructured/condition"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"time"
)

// The reason literals this module owns. The runtime's are its own constants.
const (
	// AvailablePartialCoverage: available, and the rollup assessed a MINORITY of the children the
	// composition names (#297).
	ReasonAvailablePartialCoverage = "AvailablePartialCoverage"
	// ChildrenForbidden: not one child could be assessed, and at least one read was refused by RBAC
	// (#318). Matched exactly by installer's dependency gate: never rename it alone.
	ReasonChildrenForbidden = "ChildrenForbidden"
	// InventoryNotRecorded: the composition's managed-children inventory has never been recorded,
	// so there is nothing to assess.
	ReasonInventoryNotRecorded = "InventoryNotRecorded"
	// HelmOperationInProgress: written before an install or uninstall starts (#410).
	ReasonHelmOperationInProgress = "HelmOperationInProgress"
	// InstallFailing (core-provider#636, Diego 2026-11-23): a first install has failed three times or more
	// in a row. Retried at the error backoff.
	ReasonInstallFailing = "InstallFailing"
	// PendingHelmOperation: an earlier helm operation's lock is still within its grace period.
	ReasonPendingHelmOperation = "PendingHelmOperation"
	// HooksMayNotHaveRun: krateo.io/hooks-unverified is set; an interrupted or failed helm operation's
	// hooks may not have run (core-provider#563, #636).
	ReasonHooksMayNotHaveRun = "HooksMayNotHaveRun"
	// CreateWithheldByPolicy and UpdateWithheldByPolicy (unstructured-runtime#110 D4, core-provider#597):
	// krateo.io/management-policy withholds the verb. The literals are plumbing managementpolicy's, the
	// one definition both runtimes report.
	ReasonCreateWithheldByPolicy = managementpolicy.ReasonCreateWithheld
	ReasonUpdateWithheldByPolicy = managementpolicy.ReasonUpdateWithheld
	// RecoveryWithheldByPolicy (core-provider#597, Diego 2026-11-21): this controller's own. A release
	// stuck in a helm operation is not rolled back, because the rollback is a write that
	// krateo.io/management-policy withholds.
	ReasonRecoveryWithheldByPolicy = "RecoveryWithheldByPolicy"
)

// Writer is who writes a reason on a composition.
type Writer string

const (
	WriterCDC     Writer = "composition-dynamic-controller"
	WriterRuntime Writer = "unstructured-runtime"
)

// Reason is one reason Ready can carry, with the status it is always written with.
type Reason struct {
	// condType is the condition this reason is written on: Ready, Synced, ChartRenderable,
	// RBACComplete or ApiResolved (core-provider#528).
	condType string
	reason   string
	status   metav1.ConditionStatus
	writer   Writer
	meaning  string
	clears   Clearance
	doc      *Doc // a pointer, so Reason stays comparable (a switch on it compiles)
}

// Unreleased is the Since of a reason no release carries yet; write it as `Since: Unreleased`. The
// release that ships it replaces it with its own tag and appends the reason to
// testdata/released-reasons.txt, both done by RELEASE_TAG=X.Y.Z UPDATE_READY_REASONS=1 go test
// ./internal/conditionreason/ -run '^TestReasonTablesAreReleaseReady$' (docs/release.md); release-tag.yaml
// runs the same test without UPDATE and refuses a tag that still has one.
const Unreleased = "unreleased"

// Doc is what docs/ready-reasons.json publishes about a reason beyond its condition (core-provider#667).
type Doc struct {
	// Since is the first core-provider release tag whose released source contains the reason: the
	// module's own non-test source plus the unstructured-runtime and plumbing versions its go.mod pins
	// (a runtime-written reason's literal lives in the runtime), and, on a type other than Ready and
	// Synced, the type's literal too. Backfilled once from `git tag -l '2.*'`; for a literal that
	// another type also used first, it is a lower bound. 2.12.0 is the floor: the cdc joined the
	// monorepo there. Unreleased until a release ships it.
	Since string
	// Action is what a PERSON does on seeing the reason: short, plain, never a command line. The
	// portal shows it as is, so changing it is a UI change. Required unless the reason is healthy or
	// steady.
	Action string
	// DetailFields are JSON paths of the status fields this controller writes with the reason, beyond
	// the condition's own message. Each must be declared in core-provider's status.schema.json.
	DetailFields []string
}

// Since is the first release carrying the reason, or Unreleased.
func (r Reason) Since() string { return r.docOf().Since }

// Action is what a person does on seeing it.
func (r Reason) Action() string { return r.docOf().Action }

// DetailFields are the status fields carrying its detail.
func (r Reason) DetailFields() []string { return append([]string(nil), r.docOf().DetailFields...) }

// docOf is r's Doc, empty for the zero Reason.
func (r Reason) docOf() Doc {
	if r.doc == nil {
		return Doc{}
	}
	return *r.doc
}

// Removed is a reason a release carried and this registry no longer lists (core-provider#667).
type Removed struct {
	Condition string
	Reason    string
	Status    metav1.ConditionStatus
	Writer    Writer
	// RemovedIn is the first release without it, or Unreleased.
	RemovedIn string
	// Note says what a consumer reads instead.
	Note string
}

// deprecated is every released reason removed from Table, oldest first. A rename is a removal here
// plus an addition to Table. Empty: no released reason has been removed since the list began.
var deprecated = []Removed{}

// Deprecated is every released reason removed from Table.
func Deprecated() []Removed { return append([]Removed(nil), deprecated...) }

// ClearKind is how long a reason is expected to last (core-provider#522): what a consumer should do
// on seeing it, re-read soon, wait for a timer, or treat it as a state.
type ClearKind string

const (
	// Steady: the healthy end state, not expected to clear.
	Steady ClearKind = "steady"
	// Timer: clears when an operation returns, bounded by a named timer.
	Timer ClearKind = "timer"
	// NextPass: clears on the next reconcile, normally within the resync interval or the retry backoff.
	NextPass ClearKind = "next pass"
	// Indefinite: lasts until something outside this controller changes (a child, a permission).
	Indefinite ClearKind = "indefinite"
)

// Clearance is a reason's expected lifetime. Note names the controller's timers by key in braces,
// {helmOperationTimeout}, {pendingOperationGrace} or {childTeardownGrace}: the rendering in
// internal/composition substitutes their values and the variables that set them, so a changed
// timeout changes docs/ready-reasons.md.
type Clearance struct {
	Kind ClearKind
	Note string
}

// String is the reason as written on the condition.
func (r Reason) String() string { return r.reason }

// Status is the status the reason is always written with.
func (r Reason) Status() metav1.ConditionStatus { return r.status }

// Writer is who writes it.
func (r Reason) Writer() Writer { return r.writer }

// Meaning is one line on when it is written.
func (r Reason) Meaning() string { return r.meaning }

// Clears is how long the reason is expected to last.
func (r Reason) Clears() Clearance { return r.clears }

// Condition is the Ready condition carrying r, with no message.
func (r Reason) Condition() metav1.Condition {
	return metav1.Condition{
		Type:               r.condType,
		Status:             r.status,
		LastTransitionTime: metav1.Now(),
		Reason:             r.reason,
	}
}

// WithMessage is r's Ready condition with message.
func (r Reason) WithMessage(message string) metav1.Condition {
	c := r.Condition()
	c.Message = message
	return c
}

// The reasons the composition-dynamic-controller writes.
var (
	Available = ready(condition.ReasonAvailable, metav1.ConditionTrue, WriterCDC,
		"stampReady: the health verdict is healthy, over most of the children it names", Clearance{Steady, "the healthy end state"},
		Doc{Since: "2.12.0"})
	AvailablePartialCoverage = ready(ReasonAvailablePartialCoverage, metav1.ConditionTrue, WriterCDC,
		"stampReady: healthy, but the rollup assessed a minority of the children it names (#297)", Clearance{Steady, "the healthy end state, over a minority of the children named"},
		Doc{Since: "2.13.54", DetailFields: []string{"status.healthEvaluatedChildren", "status.managedChildrenCount"}})
	ReconcileGracefullyPaused = ready(compositioncondition.ReasonReconcileGracefullyPaused, metav1.ConditionTrue, WriterCDC,
		"Observe, and setGracefullyPausedCondition: krateo.io/gracefully-paused is in effect (Synced=False is the liveness half)", Clearance{Steady, "until krateo.io/gracefully-paused is removed; paired with Synced=False/ReconcileGracefullyPaused"},
		Doc{Since: "2.12.0", Action: "To resume, remove the krateo.io/gracefully-paused annotation."})
	Creating = ready(condition.ReasonCreating, metav1.ConditionFalse, WriterCDC,
		"stampReady: a child is converging; also stamped after Create's install (ConditionTypeCreating)", Clearance{Indefinite, "until the converging child converges; no deadline (core-provider#498 adds one)"},
		Doc{Since: "2.12.0", Action: "Wait: it clears when the composition's children are ready. If it lasts, the message says what is still converging.", DetailFields: []string{"status.installFailures.count", "status.installFailures.lastError"}})
	Unavailable = ready(condition.ReasonUnavailable, metav1.ConditionFalse, WriterCDC,
		"stampReady: the health verdict is failing; failObserveWithFinding: Observe failed, the message is the error; createFailedOnDerivedRBAC: a first render the chart or the values refuse, or RBAC the API server refuses, the message is the error (#646); any other failure there stays Creating", Clearance{Indefinite, "until the failing child, or the observation, recovers"},
		Doc{Since: "2.12.0", Action: "Fix what the message names: a failing child, values or a chart the render refuses, or a permission the API server refused."})
	HelmOperationInProgress = ready(ReasonHelmOperationInProgress, metav1.ConditionFalse, WriterCDC,
		"recordHelmOperation, before an install or uninstall (#410); the message names the start time and the per-hook bound", Clearance{Timer, "until the install or uninstall returns; each hook and each wait must finish within {helmOperationTimeout}, and the message carries the start time"},
		Doc{Since: "2.13.81", Action: "Wait: it clears when the install or uninstall returns.", DetailFields: []string{"status.helmOperation.phase", "status.helmOperation.startedAt", "status.helmOperation.startedBy"}})
	InstallFailing = ready(ReasonInstallFailing, metav1.ConditionFalse, WriterCDC,
		"recordInstallFailure, from the 3rd consecutive failed attempt of a first install (the first two are Creating); kept, with what is running in its message, while a retry is in flight (#636); the message names the failed attempts (status.installFailures.count), the automatic retries (status.installRecovery.count) and the last error. A retried interruption is not a failed attempt",
		Clearance{Indefinite, "until an attempt succeeds; retried at the error backoff, with no cap"},
		Doc{Since: "2.13.129", Action: "Fix the error the message names, often in the composition's values or the chart; the install is retried automatically.", DetailFields: []string{"status.installFailures.count", "status.installFailures.lastError", "status.installFailures.lastAt", "status.installRecovery.count"}})
	PendingHelmOperation = ready(ReasonPendingHelmOperation, metav1.ConditionFalse, WriterCDC,
		"Observe: a release is pending within its grace period, so this reconcile stands down", Clearance{Timer, "until the pending operation settles; otherwise rolled back once pending longer than {pendingOperationGrace} (at once when the pod that started it is gone); the message says how long it has been pending"},
		Doc{Since: "2.12.0", Action: "Wait: it clears when the pending helm operation settles, or when it is rolled back automatically after the grace period."})
	HooksMayNotHaveRun = ready(ReasonHooksMayNotHaveRun, metav1.ConditionFalse, WriterCDC,
		"stampReady: krateo.io/hooks-unverified is set: a release stuck pending with no deployed revision before it was rolled back to itself, so the interrupted operation's hooks may not have run (#563); outranks the rollup, whose reason the message keeps. A FIRST install of this controller's that was interrupted or failed, and never deployed, is instead installed again with every install hook (#629, #636), so this reason then means it was not: krateo.io/interrupted-install-recovery is set, an interrupted one reached its cap, or the management policy withholds create (it was rolled back to itself or upgraded, and the upgrade's hooks are not the install's); or the policy withholds update, so nothing is written to the release (no annotation in that case); or the release was live once or is not this controller's",
		Clearance{Indefinite, "until a human who has verified the hooks' effects removes krateo.io/hooks-unverified, or the next successful helm install or upgrade after the recovery runs its hooks and removes it"},
		Doc{Since: "2.13.108", Action: "An admin verifies that what the chart's hooks do has been done, then removes the krateo.io/hooks-unverified annotation."})
	ZeroEvidence = ready(condition.ReasonZeroEvidence, metav1.ConditionUnknown, WriterCDC,
		"stampReady: no child could be assessed at all (#165)", Clearance{Indefinite, "until at least one child can be assessed"},
		Doc{Since: "2.13.36", Action: "Check the composition's children: none of them could be assessed. It clears once one can be.", DetailFields: []string{"status.managedChildrenCount", "status.healthEvaluatedChildren", "status.unevaluatedChildren"}})
	ChildrenForbidden = ready(ReasonChildrenForbidden, metav1.ConditionUnknown, WriterCDC,
		"stampReady: no child could be assessed, and at least one read was refused by RBAC (#318)", Clearance{Indefinite, "until the controller may read its children (an RBAC change)"},
		Doc{Since: "2.13.61", Action: "An admin grants the composition's controller permission to read its children.", DetailFields: []string{"status.managedChildrenCount", "status.unreadableChildrenCount"}})
	InventoryNotRecorded = ready(ReasonInventoryNotRecorded, metav1.ConditionUnknown, WriterCDC,
		"stampReady: the managed-children inventory was never recorded", Clearance{NextPass, "the next Update records the inventory (core-provider#326)"},
		Doc{Since: "2.13.61", Action: "Wait: the next reconcile records the composition's children, and it clears."})
	CreateWithheldByPolicy = ready(ReasonCreateWithheldByPolicy, metav1.ConditionFalse, WriterCDC,
		"Observe: the release is not installed and krateo.io/management-policy withholds create, so nothing is installed (#110 D4, #597); the message names the policy value and how to allow create",
		Clearance{Indefinite, "until a human changes krateo.io/management-policy to allow create, or installs the release by other means"},
		Doc{Since: "2.13.118", Action: "To install it, change the krateo.io/management-policy annotation to allow create."})
	UpdateWithheldByPolicy = ready(ReasonUpdateWithheldByPolicy, metav1.ConditionTrue, WriterCDC,
		"stampReady: healthy, and krateo.io/management-policy withholds update, so the INSTALLED chart is FROZEN: a changed chart version or values, or drift, is reported and not applied, so the release may not be at the definition's chart version or values (#110 D4/D7, #597); the message names the installed and wanted versions, the drifted children, or why drift could not be assessed. Replaces only Available: a failing rollup keeps its own reason, and its message notes the withheld update",
		Clearance{Indefinite, "until krateo.io/management-policy allows update, or the definition's chart and the composition's values return to what is installed"},
		Doc{Since: "2.13.118", Action: "To apply changes, change the krateo.io/management-policy annotation to allow update."})
	RecoveryWithheldByPolicy = ready(ReasonRecoveryWithheldByPolicy, metav1.ConditionFalse, WriterCDC,
		"Observe: the release is stuck in a helm operation past its grace period (or its controller is gone), and the recovery, a helm rollback, is a write krateo.io/management-policy withholds (it withholds update), so the release is not rolled back (#597); the message names the helm status, how long it has been pending, and what would recover it",
		Clearance{Indefinite, "until a human recovers the release (for example a helm rollback), or krateo.io/management-policy allows update and the next reconcile recovers it"},
		Doc{Since: "2.13.118", Action: "An admin recovers the release (for example with a helm rollback), or changes the krateo.io/management-policy annotation to allow update."})
)

// The reasons unstructured-runtime writes on the same compositions. Unexported: the cdc must not
// write them itself.
var (
	runtimeReadyPaused = ready(condition.ReasonReconcilePausedReady, metav1.ConditionTrue, WriterRuntime,
		"krateo.io/paused is set (Synced=False/ReconcilePaused is the liveness half)", Clearance{Steady, "until krateo.io/paused is removed; paired with Synced=False/ReconcilePaused"},
		Doc{Since: "2.12.0", Action: "To resume, remove the krateo.io/paused annotation."})
	runtimeCreating = ready(condition.ReasonCreating, metav1.ConditionFalse, WriterRuntime,
		"worker: a Create failed or was incomplete, or writing the resource or its annotations failed (message: the error)", Clearance{Indefinite, "retried with backoff until the write succeeds"},
		Doc{Since: "2.12.0", Action: "Wait: the write is retried. If it lasts, the message names the error."})
	runtimeObserveFailed = ready(condition.ReasonObserveFailed, metav1.ConditionUnknown, WriterRuntime,
		"Observe failed and the handler did not wrap its error with ReadyAssessed", Clearance{NextPass, "the next observation, after the retry backoff"},
		Doc{Since: "2.13.60", Action: "Wait: it clears on the next observation. If it lasts, the message names the error."})
	runtimeCreateOutcomeUnknown = ready(condition.ReasonCreateOutcomeUnknown, metav1.ConditionFalse, WriterRuntime,
		"worker: a create whose outcome cannot be determined is not repeated (krateo.io/external-create-pending set, the existence check failed); reconciliation and deletion both wait on it; message: the cause, that it clears by itself once the check answers, and the remediation (unstructured-runtime#93, #107)",
		Clearance{Indefinite, "until an existence check succeeds (retried every resync; a transient cause clears by itself) or a human, having verified the external state, removes krateo.io/external-create-pending"},
		Doc{Since: "2.13.103", Action: "Wait: it clears once the existence check answers. If it lasts, an admin verifies the external state and removes the krateo.io/external-create-pending annotation."})
	runtimeDeleting = ready(condition.ReasonDeleting, metav1.ConditionFalse, WriterRuntime,
		"Delete is in progress or failed (message: the error, on failure)", Clearance{Timer, "until deletion completes: finalizer-bearing children get {childTeardownGrace}, then the release is uninstalled with each hook and wait within {helmOperationTimeout}"},
		Doc{Since: "2.12.0", Action: "Wait: it clears when the deletion completes."})
)

// ready is a Reason written on the Ready condition.
func ready(reason string, status metav1.ConditionStatus, writer Writer, meaning string, clears Clearance, doc Doc) Reason {
	return Reason{condType: condition.TypeReady, reason: reason, status: status, writer: writer, meaning: meaning, clears: clears, doc: &doc}
}

// on is a Reason written on condType.
func on(condType, reason string, status metav1.ConditionStatus, writer Writer, meaning string, clears Clearance, doc Doc) Reason {
	return Reason{condType: condType, reason: reason, status: status, writer: writer, meaning: meaning, clears: clears, doc: &doc}
}

// Type is the condition a reason is written on.
func (r Reason) Type() string { return r.condType }

// The condition types the composition-dynamic-controller writes besides Ready and Synced
// (core-provider#528).
const (
	// TypeChartRenderable: whether chart-inspector could render the blueprint against its values.
	TypeChartRenderable = "ChartRenderable"
	// TypeRBACComplete: whether the RBAC derived from the chart was applied.
	TypeRBACComplete = "RBACComplete"
	// TypeApiResolved: whether the CompositionDefinition's apiRef resolved, for status projection.
	TypeApiResolved = "ApiResolved"
)

// The reason literals of those types. ChartRenderable's are an OPEN set to a consumer (frontend and
// portal treat an unknown one as "not renderable, cause unrecognised"); the cdc writes these.
const (
	ReasonChartRenderable          = "Renderable"
	ReasonUnservedKind             = "UnservedKind"
	ReasonValuesSchemaViolation    = "ValuesSchemaViolation"
	ReasonInspectorRejected        = "InspectorRejected"
	ReasonTemplateRefused          = "TemplateRefused"
	ReasonInspectorUnavailable     = "InspectorUnavailable"
	ReasonRBACApplied              = "Applied"
	ReasonRBACNamespaceTerminating = "NamespaceTerminating"
	ReasonRBACApplyFailed          = "ApplyFailed"
	ReasonRBACNotEvaluated         = "NotEvaluated"
	ReasonApiResolved              = "Resolved"
	ReasonApiEmpty                 = "ApiEmpty"
	ReasonAuthnUnreachable         = "AuthnUnreachable"
	ReasonAuthnError               = "AuthnError"
	ReasonAuthnRejected            = "AuthnRejected"
	ReasonResolveFailed            = "ResolveFailed"
	ReasonApiPartiallyResolved     = "ApiPartiallyResolved"
	ReasonStageOutcomesUnknown     = "StageOutcomesUnknown"
)

// Synced, written by the cdc (the graceful pause) and by unstructured-runtime.
var (
	SyncedGracefullyPaused = on(condition.TypeSynced, compositioncondition.ReasonReconcileGracefullyPaused, metav1.ConditionFalse, WriterCDC,
		"Observe: krateo.io/gracefully-paused is in effect; nothing is converging this (paired with Ready=True/ReconcileGracefullyPaused)",
		Clearance{Steady, "until krateo.io/gracefully-paused is removed"},
		Doc{Since: "2.12.0", Action: "To resume, remove the krateo.io/gracefully-paused annotation."})
	SyncedGracefulPausePending = on(condition.TypeSynced, compositioncondition.ReasonGracefulPausePending, metav1.ConditionFalse, WriterCDC,
		"Observe: krateo.io/gracefully-paused is set but not yet in effect; the next applied change still goes through (#381)",
		Clearance{NextPass, "takes effect after the next change to the composition is applied"},
		Doc{Since: "2.13.75", Action: "Wait: the pause takes effect after the next change to the composition is applied."})
	runtimeSyncedSuccess = on(condition.TypeSynced, condition.ReasonReconcileSuccess, metav1.ConditionTrue, WriterRuntime,
		"worker: the pass completed", Clearance{Steady, "the healthy end state"},
		Doc{Since: "2.12.0"})
	runtimeSyncedError = on(condition.TypeSynced, condition.ReasonReconcileError, metav1.ConditionFalse, WriterRuntime,
		"worker: the pass failed (message: the error)", Clearance{NextPass, "retried with backoff; persists while the cause does"},
		Doc{Since: "2.12.0", Action: "Wait: the pass is retried. If it lasts, fix the error the message names."})
	runtimeSyncedPaused = on(condition.TypeSynced, condition.ReasonReconcilePaused, metav1.ConditionFalse, WriterRuntime,
		"worker: krateo.io/paused is set (paired with Ready=True/ReconcilePaused)", Clearance{Steady, "until krateo.io/paused is removed"},
		Doc{Since: "2.12.0", Action: "To resume, remove the krateo.io/paused annotation."})
	runtimeSyncedDeferred = on(condition.TypeSynced, condition.ReasonReconcileDeferred, metav1.ConditionFalse, WriterRuntime,
		"worker: the runtime chose not to call the handler this pass, an endpoint's rate limit or its own concurrency limit; Ready is not touched",
		Clearance{NextPass, "the message names the next attempt when the server gave one"},
		Doc{Since: "2.13.65", Action: "Wait: the pass is retried; the message names the next attempt when it is known."})
	// unstructured-runtime v1.31.0 (#110, #117). krateo.io/management-policy is an annotation, so
	// nothing validates it at admission: these two are the only detector of a bad value.
	runtimeSyncedPolicyInvalid = on(condition.TypeSynced, managementpolicy.ReasonInvalid, metav1.ConditionFalse, WriterRuntime,
		"worker: krateo.io/management-policy does not parse (an unknown verb, or a stray separator), so the runtime acts on nothing, as observe; it wins over every other pass outcome on Synced (the pass's own events and errors still occur); the message names the annotation and the bad token",
		Clearance{Indefinite, "until a human fixes krateo.io/management-policy"},
		Doc{Since: "2.13.118", Action: "Fix the krateo.io/management-policy annotation: the message names the value it cannot read."})
	runtimeSyncedDeletionPolicyInvalid = on(condition.TypeSynced, managementpolicy.ReasonDeletionPolicyInvalid, metav1.ConditionFalse, WriterRuntime,
		"worker: krateo.io/deletion-policy is neither delete nor orphan, so the external resource will be KEPT on delete; reported on every pass while set, not only while deleting; the message names the value",
		Clearance{Indefinite, "until a human fixes krateo.io/deletion-policy"},
		Doc{Since: "2.13.118", Action: "Set the krateo.io/deletion-policy annotation to delete or orphan."})
)

// ChartRenderable, written by the cdc from chart-inspector's answer (chartresolved.go).
var (
	ChartRenderable = on(TypeChartRenderable, ReasonChartRenderable, metav1.ConditionTrue, WriterCDC,
		"chart-inspector rendered the blueprint", Clearance{Steady, "the healthy end state"},
		Doc{Since: "2.13.47"})
	ChartUnservedKind = on(TypeChartRenderable, ReasonUnservedKind, metav1.ConditionFalse, WriterCDC,
		"chart-inspector answered 422: the blueprint references a kind this cluster does not serve",
		Clearance{Indefinite, "until the kind is served (it resolves itself once its CRD exists)"},
		Doc{Since: "2.13.47", Action: "An admin installs the CRD of the kind the chart uses; it clears once the kind is served."})
	ChartValuesSchemaViolation = on(TypeChartRenderable, ReasonValuesSchemaViolation, metav1.ConditionFalse, WriterCDC,
		"chart-inspector answered 400: the values do not satisfy the chart's values.schema.json",
		Clearance{Indefinite, "until the author fixes the values or the schema (#325)"},
		Doc{Since: "2.13.64", Action: "Edit the composition's values so they satisfy the chart's values schema, or fix the schema."})
	ChartTemplateRefused = on(TypeChartRenderable, ReasonTemplateRefused, metav1.ConditionFalse, WriterCDC,
		"chart-inspector answered 400 TemplateRefused: the chart's own templates refused these values (a fail/required in the chart, or a template error)",
		Clearance{Indefinite, "until the author changes the values or the chart (#583)"},
		Doc{Since: "2.13.112", Action: "Edit the composition's values, or fix the chart, so its templates accept them."})
	ChartInspectorRejected = on(TypeChartRenderable, ReasonInspectorRejected, metav1.ConditionFalse, WriterCDC,
		"chart-inspector refused the request with another 4xx: the cdc and chart-inspector disagree about the request",
		Clearance{Indefinite, "until the version skew or defect is fixed"},
		Doc{Since: "2.13.64", Action: "An admin aligns the versions of the composition-dynamic-controller and chart-inspector, or reports the defect."})
	ChartInspectorUnavailable = on(TypeChartRenderable, ReasonInspectorUnavailable, metav1.ConditionFalse, WriterCDC,
		"chart-inspector itself failed (5xx, transport); not the blueprint's fault",
		Clearance{NextPass, "retried on the next reconcile; persists while chart-inspector is down"},
		Doc{Since: "2.13.47", Action: "Wait: it is retried. If it lasts, an admin checks that chart-inspector is running."})
)

// RBACComplete, written by the cdc after applying the chart's derived RBAC (rbaccomplete.go).
var (
	RBACApplied = on(TypeRBACComplete, ReasonRBACApplied, metav1.ConditionTrue, WriterCDC,
		"every RBAC object derived from the chart was applied", Clearance{Steady, "the healthy end state"},
		Doc{Since: "2.13.67"})
	RBACApplyFailed = on(TypeRBACComplete, ReasonRBACApplyFailed, metav1.ConditionFalse, WriterCDC,
		"applying the derived RBAC failed (message: the error)", Clearance{NextPass, "retried on the next reconcile; persists while the cause does"},
		Doc{Since: "2.13.67", Action: "Wait: it is retried. If it lasts, fix the error the message names."})
	RBACNamespaceTerminating = on(TypeRBACComplete, ReasonRBACNamespaceTerminating, metav1.ConditionFalse, WriterCDC,
		"objects for a terminating namespace were skipped (message: which)", Clearance{Indefinite, "until the namespace finishes terminating"},
		Doc{Since: "2.13.67", Action: "Wait: it clears when the namespace finishes terminating."})
	RBACNotEvaluated = on(TypeRBACComplete, ReasonRBACNotEvaluated, metav1.ConditionUnknown, WriterCDC,
		"RBAC was not applied this reconcile: the chart could not be rendered (see ChartRenderable)",
		Clearance{Indefinite, "follows ChartRenderable: clears once the chart renders"},
		Doc{Since: "2.13.67", Action: "Fix what ChartRenderable reports first: this follows it."})
)

// ApiResolved, written by the cdc when the CompositionDefinition has an apiRef (apiresolved.go).
var (
	ApiResolved = on(TypeApiResolved, ReasonApiResolved, metav1.ConditionTrue, WriterCDC,
		"the apiRef resolved and snowplow confirmed every RESTAction stage succeeded (X-Snowplow-Stage-Outcomes: [])", Clearance{Steady, "the healthy end state"},
		Doc{Since: "2.13.44"})
	ApiPartiallyResolved = on(TypeApiResolved, ReasonApiPartiallyResolved, metav1.ConditionFalse, WriterCDC,
		"some RESTAction stages did not succeed (message: each stage and snowplow's code); the resolved data IS projected, the only False with data, and status.unrefreshedFields names the fields left stale",
		Clearance{Indefinite, "until the stages succeed; Forbidden, NotFound, Aborted and NotRun do not heal on a retry, a transient code clears on the next reconcile"},
		Doc{Since: "2.13.106", Action: "Fix the RESTAction stages the message names; until then, the fields in status.unrefreshedFields show older values.", DetailFields: []string{"status.unrefreshedFields"}})
	ApiStageOutcomesUnknown = on(TypeApiResolved, ReasonStageOutcomesUnknown, metav1.ConditionUnknown, WriterCDC,
		"snowplow answered but did not say whether every stage succeeded (no readable X-Snowplow-Stage-Outcomes; snowplow older than 1.12.57); the data IS projected",
		Clearance{Indefinite, "until snowplow sends the header"},
		Doc{Since: "2.13.106", Action: "An admin upgrades snowplow to 1.12.57 or later."})
	ApiEmpty = on(TypeApiResolved, ReasonApiEmpty, metav1.ConditionFalse, WriterCDC,
		"the apiRef resolved without error but returned no data", Clearance{Indefinite, "until the RESTAction returns data"},
		Doc{Since: "2.13.44", Action: "Check that the RESTAction returns data for this composition."})
	ApiAuthnUnreachable = on(TypeApiResolved, ReasonAuthnUnreachable, metav1.ConditionFalse, WriterCDC,
		"authn is unreachable, so no service token; on a remote target usually authn is not installed there",
		Clearance{Indefinite, "until authn is reachable (installed on the target)"},
		Doc{Since: "2.13.44", Action: "An admin installs authn on the cluster the controller runs in, or makes it reachable."})
	ApiAuthnError = on(TypeApiResolved, ReasonAuthnError, metav1.ConditionFalse, WriterCDC,
		"authn failed on its own account (5xx)", Clearance{NextPass, "retried on the next reconcile; persists while authn fails"},
		Doc{Since: "2.13.44", Action: "Wait: it is retried. If it lasts, an admin checks authn."})
	ApiAuthnRejected = on(TypeApiResolved, ReasonAuthnRejected, metav1.ConditionFalse, WriterCDC,
		"authn refused the service token and its answer carried no machine-readable cause",
		Clearance{Indefinite, "until the refusal's cause (mapping, audience, credentials) is fixed"},
		Doc{Since: "2.13.44", Action: "An admin fixes the cause of the refusal: the authn mapping, the audience or the credentials."})
	ApiResolveFailed = on(TypeApiResolved, ReasonResolveFailed, metav1.ConditionFalse, WriterCDC,
		"anything that is not an authn problem: snowplow unreachable, a RESTAction that does not resolve, a malformed apiRef",
		Clearance{Indefinite, "until the cause is fixed; a transient one clears on the next reconcile"},
		Doc{Since: "2.13.44", Action: "Fix the cause the message names: snowplow unreachable, a RESTAction that does not resolve, or a malformed apiRef."})
)

// ApiAuthnRefusedWith is ApiResolved=False carrying authn's OWN refusal reason, passed through as
// authn sent it (its Status causes[0].reason, else its top-level reason): NoMapping,
// AudienceMismatch, MissingBearer and whatever authn adds. This is the one OPEN reason the cdc
// writes: its strings are authn's, not this module's, so it is listed once as a family.
func ApiAuthnRefusedWith(authnReason, message string) metav1.Condition {
	return metav1.Condition{
		Type:               TypeApiResolved,
		Status:             metav1.ConditionFalse,
		LastTransitionTime: metav1.Now(),
		Reason:             authnReason,
		Message:            message,
	}
}

// apiAuthnPassthrough is the table row for ApiAuthnRefusedWith's family.
var apiAuthnPassthrough = on(TypeApiResolved, "(authn's reason)", metav1.ConditionFalse, WriterCDC,
	"authn refused the service token and named why; the reason is authn's own (e.g. NoMapping, AudienceMismatch, MissingBearer): an OPEN set",
	Clearance{Indefinite, "until the refusal's cause is fixed"},
	Doc{Since: "2.13.44", Action: "An admin fixes the cause authn names in the reason, for example a missing mapping."})

// Table is every reason a composition's Ready can carry, cdc first, in a fixed order.
func Table() []Reason {
	return []Reason{
		Available, AvailablePartialCoverage, ReconcileGracefullyPaused,
		Creating, Unavailable, HelmOperationInProgress, InstallFailing, PendingHelmOperation, HooksMayNotHaveRun,
		ZeroEvidence, ChildrenForbidden, InventoryNotRecorded, CreateWithheldByPolicy, UpdateWithheldByPolicy, RecoveryWithheldByPolicy,
		runtimeReadyPaused, runtimeCreating, runtimeObserveFailed, runtimeCreateOutcomeUnknown, runtimeDeleting,
		SyncedGracefullyPaused, SyncedGracefulPausePending,
		runtimeSyncedSuccess, runtimeSyncedError, runtimeSyncedPaused, runtimeSyncedDeferred,
		runtimeSyncedPolicyInvalid, runtimeSyncedDeletionPolicyInvalid,
		ChartRenderable, ChartUnservedKind, ChartValuesSchemaViolation, ChartTemplateRefused, ChartInspectorRejected, ChartInspectorUnavailable,
		RBACApplied, RBACApplyFailed, RBACNamespaceTerminating, RBACNotEvaluated,
		ApiResolved, ApiPartiallyResolved, ApiStageOutcomesUnknown, ApiEmpty, ApiAuthnUnreachable, ApiAuthnError, ApiAuthnRejected, ApiResolveFailed, apiAuthnPassthrough,
	}
}

// RuntimeConstructors are unstructured-runtime's own constructors for the reasons it writes, so a
// test can assert each listed status against what the runtime actually builds.
func RuntimeConstructors() []metav1.Condition {
	return []metav1.Condition{
		condition.ReadyPaused(),
		condition.Creating(),
		condition.ObserveFailed(""),
		condition.Deleting(),
		condition.Unavailable(),
		condition.Available(),
		condition.ZeroEvidence(),
		condition.CreateOutcomeUnknown(""),
		// Synced, written by the runtime (#528). ReconcilePaused shares its string with Ready's
		// ReadyPaused, which is why this is a list keyed by type and reason, not a map by reason.
		condition.ReconcileSuccess(),
		condition.ReconcileError(errors.New("e")),
		condition.ReconcilePaused(),
		condition.ReconcileDeferred("k", time.Time{}),
		condition.ManagementPolicyInvalid("m"),
		condition.DeletionPolicyInvalid("m"),
	}
}

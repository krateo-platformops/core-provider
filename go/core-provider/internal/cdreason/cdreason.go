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

	"github.com/krateo-platformops/core-provider/internal/tools/restactionrbac"
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
	// ValuesSchemaMissing: the chart has no root values.schema.json, so no typed CRD can be generated
	// (#566). core-provider requires one; a wrapper chart can add it.
	ReasonValuesSchemaMissing = "ValuesSchemaMissing"
	// DerivedNameTooLong: a name core-provider derives from the chart's name and Chart.yaml version
	// (the CRD version, the controller Deployment and its selector label, the controller Service)
	// would be refused by the API server or silently truncated (#588). A config error in the chart.
	ReasonDerivedNameTooLong = "DerivedNameTooLong"
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
	ValuesSchemaMissing = ready(ReasonValuesSchemaMissing, metav1.ConditionFalse, WriterCoreProvider, Failed,
		"Observe: the chart has no values.schema.json at its root, so no typed CRD can be generated and nothing is applied; the message names the chart (#566)")
	DerivedNameTooLong = ready(ReasonDerivedNameTooLong, metav1.ConditionFalse, WriterCoreProvider, Failed,
		"Observe: a name derived from the chart's name and Chart.yaml version does not fit Kubernetes' limits: the CRD version is not a DNS-1035 label, the controller Deployment's name (also its selector label) is over 63 characters, or, with cdc.metrics.enabled, the controller Service's is; the message names each name, its length and the problem. Nothing is applied, and a definition that worked before keeps what it has (#588)")
	Deleting = ready(rtv1.ReasonDeleting, metav1.ConditionFalse, WriterCoreProvider, Converging,
		"Delete: the definition is being deleted")
)

// The condition types a CompositionDefinition carries besides Ready and Synced (#568 child 5). The
// rollup reads none of them (verdict NotRead), but consumers gate on them (installer reads
// SchemaFullyEnforced), so their reasons are a closed, documented set like Ready's: every row below
// is the only way core-provider builds that condition (TestNoRegisteredConditionBuiltOutsideCdreason).
const (
	// TypeControllerActive: replicas are declared for the definition's controller (scale-to-zero
	// reports on it). Defined in plumbing/conditions, shared with the cdc.
	TypeControllerActive rtv1.ConditionType = conditions.TypeControllerActive
	// TypeSchemaFullyEnforced: the generated CRD enforces everything the chart's values.schema.json
	// states (#362).
	TypeSchemaFullyEnforced rtv1.ConditionType = "SchemaFullyEnforced"
	// TypeSpecSchemaCurrent: the served CRD version carries the spec schema the generator produces
	// today (#534).
	TypeSpecSchemaCurrent rtv1.ConditionType = "SpecSchemaCurrent"
	// TypeApiRefRBACComplete: the apiRef's RESTAction read-set was resolved into RBAC. Present only
	// while the definition declares an apiRef.
	TypeApiRefRBACComplete rtv1.ConditionType = "ApiRefRBACComplete"
	// TypeAuthnDelegation: a declared spec.apiRef.delegation reaches the controller's authn mapping
	// (#405). Present only while delegation is declared.
	TypeAuthnDelegation rtv1.ConditionType = "AuthnDelegation"
	// TypeAuthnMappingApplied: the authn mappings the definition needs are in place, the controller's
	// where the controller runs (#251) and core-provider's own on the management cluster (#642), as read
	// on the pass that wrote the condition. Present only while the definition has an apiRef, and absent
	// on a pass that could not read one of them or found it missing while the kind is served: neither is
	// Applied, and neither is authn not being installed.
	TypeAuthnMappingApplied rtv1.ConditionType = "AuthnMappingApplied"
	// TypeValuesSchemaAuthored: whether the CRD's spec schema is the chart's own values.schema.json or
	// was inferred from its values.yaml files (#568 D6; inference is opt-in, krateo.io/values-schema:
	// infer). Ready is unaffected: an inferred definition reads Available.
	TypeValuesSchemaAuthored rtv1.ConditionType = "ValuesSchemaAuthored"
	// TypeMigratedValuesValid: whether every instance on the served version still satisfies that
	// version's schema after a version migration moved instances onto it (#614). The apiserver
	// ratchets validation on update, so a migration can carry a value the new version refuses; this
	// reports it. Absent until a migration has moved at least one instance. Ready is unaffected: the
	// migration is not blocked or undone.
	TypeMigratedValuesValid rtv1.ConditionType = "MigratedValuesValid"
)

// The reasons of those types, each with the one status it is written with.
var (
	ControllerRunning = on(TypeControllerActive, conditions.ReasonRunning, metav1.ConditionTrue, WriterCoreProvider,
		"the controller Deployment has replicas declared; the message says whether scale-to-zero applies and when")
	ControllerScaledToZero = on(TypeControllerActive, conditions.ReasonScaledToZero, metav1.ConditionFalse, WriterCoreProvider,
		"0 replicas, and the definition opted into scale-to-zero: idle, wakes on the first instance (#393)")
	ControllerUnavailable = on(TypeControllerActive, rtv1.ReasonUnavailable, metav1.ConditionFalse, WriterCoreProvider,
		"0 replicas, and the definition did NOT opt into scale-to-zero: nothing reconciles the Kind")
	ControllerNotObserved = on(TypeControllerActive, rtv1.ReasonNotObserved, metav1.ConditionUnknown, WriterCoreProvider,
		"the controller Deployment was not found or could not be read")

	SchemaFullyEnforced = on(TypeSchemaFullyEnforced, "FullyEnforced", metav1.ConditionTrue, WriterCoreProvider,
		"the generated CRD enforces every constraint in the chart's values schema")
	SchemaConstraintsNotEnforced = on(TypeSchemaFullyEnforced, "ConstraintsNotEnforced", metav1.ConditionFalse, WriterCoreProvider,
		"crdgen could not express some constraints, so the cluster accepts values the schema rejects; the message names them (recomputed on every Observe)")
	SchemaEnforcedOwnedElsewhere = on(TypeSchemaFullyEnforced, "OwnedByAnotherDefinition", metav1.ConditionUnknown, WriterCoreProvider,
		"a follower: another definition generates the CRD; the message names it")
	SchemaInferred = on(TypeSchemaFullyEnforced, "SchemaInferred", metav1.ConditionUnknown, WriterCoreProvider,
		"the spec schema was inferred (ValuesSchemaAuthored=False): it has no authored constraints, so True, which means authored constraints all enforced, cannot be claimed (#568 D6)")

	SpecSchemaCurrent = on(TypeSpecSchemaCurrent, "Current", metav1.ConditionTrue, WriterCoreProvider,
		"the served and generated spec schemas are identical")
	SpecSchemaLooserPending = on(TypeSpecSchemaCurrent, "LooserSchemaPending", metav1.ConditionFalse, WriterCoreProvider,
		"transient: the generated schema is provably looser and the next Update writes it")
	SpecSchemaNotLooser = on(TypeSpecSchemaCurrent, "GeneratedSchemaNotLooser", metav1.ConditionFalse, WriterCoreProvider,
		"until the chart version changes: the generated schema differs in a way not provably looser, so the served one is kept rather than risk refusing stored values")
	SpecSchemaOwnedElsewhere = on(TypeSpecSchemaCurrent, "OwnedByAnotherDefinition", metav1.ConditionUnknown, WriterCoreProvider,
		"a follower: another definition generates the CRD")

	ApiRefReadSetResolved = on(TypeApiRefRBACComplete, "ReadSetResolved", metav1.ConditionTrue, WriterCoreProvider,
		"the apiRef's RESTAction read-set was resolved and its RBAC rendered")

	AuthnDelegationRendered = on(TypeAuthnDelegation, "Rendered", metav1.ConditionTrue, WriterCoreProvider,
		"the authn mapping CRD serves spec.delegation and it is rendered onto the mapping")
	AuthnDelegationUnsupported = on(TypeAuthnDelegation, "DelegationUnsupported", metav1.ConditionFalse, WriterCoreProvider,
		"the installed authn mapping CRD does not serve spec.delegation (authn-crds < 0.37.0), so it is not rendered rather than pruned")

	AuthnMappingApplied = on(TypeAuthnMappingApplied, "Applied", metav1.ConditionTrue, WriterCoreProvider,
		"read live on this pass, both authorize: the controller's mapping, where the controller runs, and core-provider's own, on the management cluster (#642)")
	AuthnNotInstalled = on(TypeAuthnMappingApplied, "AuthnNotInstalled", metav1.ConditionFalse, WriterCoreProvider,
		"the cluster the controller runs in, or the management cluster (for core-provider's own mapping, #642), serves no serviceaccounts.serviceaccount.authn.krateo.io, so the mapping that belongs there is not written; the message names the cluster and what waits on it, and every pass looks again")

	ValuesSchemaAuthored = on(TypeValuesSchemaAuthored, "Authored", metav1.ConditionTrue, WriterCoreProvider,
		"the chart ships values.schema.json at its root, and the CRD is generated from it")
	ValuesSchemaInferred = on(TypeValuesSchemaAuthored, "InferredFromValues", metav1.ConditionFalse, WriterCoreProvider,
		"the chart ships no values.schema.json and the definition opted in (krateo.io/values-schema: infer): the schema is inferred from values.yaml, typed-open, with no authored constraints; the message gives the rules version and the untyped paths")
	ValuesSchemaOwnedElsewhere = on(TypeValuesSchemaAuthored, "OwnedByAnotherDefinition", metav1.ConditionUnknown, WriterCoreProvider,
		"a follower: another definition generates the CRD; the message names it")

	MigratedValuesAllValid = on(TypeMigratedValuesValid, "AllValid", metav1.ConditionTrue, WriterCoreProvider,
		"every instance on the served version passed a dry-run create through it after the last migration; nothing reopens this until the next migration")
	MigratedValuesRefused = on(TypeMigratedValuesValid, "ValuesRefusedByNewVersion", metav1.ConditionFalse, WriterCoreProvider,
		"some instances hold values the served version refuses (validation ratcheting let the migration through); the message names them and the fields, and a write that changes those fields will be refused until they satisfy the new schema; rechecked on every Observe until all pass")
	MigratedValuesUnchecked = on(TypeMigratedValuesValid, "ValidationIncomplete", metav1.ConditionUnknown, WriterCoreProvider,
		"no instance was refused, but some could not be judged (the dry run failed for a reason that is not a spec validation error); the message names them; rechecked on every Observe")
)

// apiRefRBACFailures are ApiRefRBACComplete=False, one per restactionrbac.Cause: each sends an
// operator somewhere different, which is why they are told apart.
var apiRefRBACFailures = map[restactionrbac.Cause]Reason{
	restactionrbac.CauseRESTActionNotFound: on(TypeApiRefRBACComplete, rtv1.ConditionReason(restactionrbac.CauseRESTActionNotFound), metav1.ConditionFalse, WriterCoreProvider,
		"snowplow says the RESTAction does not exist"),
	restactionrbac.CauseRESTActionForbidden: on(TypeApiRefRBACComplete, rtv1.ConditionReason(restactionrbac.CauseRESTActionForbidden), metav1.ConditionFalse, WriterCoreProvider,
		"the caller may not read the RESTAction; it MAY ALSO NOT EXIST, so this never means missing"),
	restactionrbac.CauseStageUnresolvable: on(TypeApiRefRBACComplete, rtv1.ConditionReason(restactionrbac.CauseStageUnresolvable), metav1.ConditionFalse, WriterCoreProvider,
		"a stage cannot be enumerated without dispatching it"),
	restactionrbac.CauseInvalidApiRef: on(TypeApiRefRBACComplete, rtv1.ConditionReason(restactionrbac.CauseInvalidApiRef), metav1.ConditionFalse, WriterCoreProvider,
		"snowplow refused the request itself, such as extras naming a reserved key"),
	restactionrbac.CauseUnauthorized: on(TypeApiRefRBACComplete, rtv1.ConditionReason(restactionrbac.CauseUnauthorized), metav1.ConditionFalse, WriterCoreProvider,
		"no usable identity reached snowplow"),
	restactionrbac.CauseSnowplowUnavailable: on(TypeApiRefRBACComplete, rtv1.ConditionReason(restactionrbac.CauseSnowplowUnavailable), metav1.ConditionFalse, WriterCoreProvider,
		"snowplow could not be reached, or ran out of time"),
	restactionrbac.CauseSnowplowError: on(TypeApiRefRBACComplete, rtv1.ConditionReason(restactionrbac.CauseSnowplowError), metav1.ConditionFalse, WriterCoreProvider,
		"snowplow, or something in front of it, answered in a way nothing here recognises (including cyclic stages)"),
	restactionrbac.CauseNotConfigured: on(TypeApiRefRBACComplete, rtv1.ConditionReason(restactionrbac.CauseNotConfigured), metav1.ConditionFalse, WriterCoreProvider,
		"core-provider has no snowplow URL (apiRef RBAC is off in the chart) while the definition declares an apiRef"),
}

// ApiRefRBACFailure is the ApiRefRBACComplete=False reason for cause, and false for a cause this
// registry does not list (TestEveryRESTActionCauseIsRegistered keeps that from happening silently).
func ApiRefRBACFailure(cause restactionrbac.Cause) (Reason, bool) {
	r, ok := apiRefRBACFailures[cause]
	return r, ok
}

// nonReady is every non-Ready, non-Synced row, in a fixed order: by type, then True, False, Unknown.
func nonReady() []Reason {
	out := []Reason{
		ControllerRunning, ControllerScaledToZero, ControllerUnavailable, ControllerNotObserved,
		SchemaFullyEnforced, SchemaConstraintsNotEnforced, SchemaEnforcedOwnedElsewhere, SchemaInferred,
		SpecSchemaCurrent, SpecSchemaLooserPending, SpecSchemaNotLooser, SpecSchemaOwnedElsewhere,
		ApiRefReadSetResolved,
	}
	for _, c := range []restactionrbac.Cause{
		restactionrbac.CauseRESTActionNotFound, restactionrbac.CauseRESTActionForbidden, restactionrbac.CauseStageUnresolvable,
		restactionrbac.CauseInvalidApiRef, restactionrbac.CauseUnauthorized, restactionrbac.CauseSnowplowUnavailable,
		restactionrbac.CauseSnowplowError, restactionrbac.CauseNotConfigured,
	} {
		out = append(out, apiRefRBACFailures[c])
	}
	return append(out, AuthnDelegationRendered, AuthnDelegationUnsupported, AuthnMappingApplied, AuthnNotInstalled,
		ValuesSchemaAuthored, ValuesSchemaInferred, ValuesSchemaOwnedElsewhere,
		MigratedValuesAllValid, MigratedValuesRefused, MigratedValuesUnchecked)
}

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

	// provider-runtime v1.21.0 (unstructured-runtime#110): the management policy reported on a
	// CompositionDefinition that carries krateo.io/management-policy or krateo.io/deletion-policy. The
	// reconciler writes them after core-provider's Observe in the same pass, and writes status once,
	// so there is no second writer: core-provider writes a CompositionDefinition's conditions only
	// through the reconciler's single status write.
	runtimeCreateWithheld = ready(rtv1.ReasonCreateWithheldByPolicy, metav1.ConditionFalse, WriterRuntime, Failed,
		"the external resource (the CRD) is absent and krateo.io/management-policy withholds create, observe-only included; nothing is created until a human changes the policy (#110 D4)")
	runtimeUpdateWithheld = ready(rtv1.ReasonUpdateWithheldByPolicy, metav1.ConditionTrue, WriterRuntime, Healthy,
		"core-provider's Observe found drift and krateo.io/management-policy withholds update: replaces a healthy Ready only, so the definition works but is NOT brought to its desired state; an unhealthy Ready keeps its own reason, with \"updates are withheld by policy\" appended (#110 D4)")
	runtimeSyncedPolicyInvalid = on(rtv1.TypeSynced, rtv1.ReasonManagementPolicyInvalid, metav1.ConditionFalse, WriterRuntime,
		"krateo.io/management-policy does not parse: the reconciler acts on nothing (observe only); wins over the pass's own Synced outcome; paused and deferred passes keep theirs")
	runtimeSyncedDeletionPolicyInvalid = on(rtv1.TypeSynced, rtv1.ReasonDeletionPolicyInvalid, metav1.ConditionFalse, WriterRuntime,
		"krateo.io/deletion-policy is neither delete nor orphan, so the external resource is KEPT on delete; reported on every pass while set")
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
	return append([]Reason{
		Available, ScaledToZero, Creating, Unavailable, RemoteChartUnreachable, SchemaDefaultContradiction, ValuesSchemaMissing, DerivedNameTooLong, Deleting,
		runtimeNotObserved, runtimeCreating, runtimeDeleting, runtimeCreateWithheld, runtimeUpdateWithheld,
		runtimeSyncedSuccess, runtimeSyncedError, runtimeSyncedPaused, runtimeSyncedPolicyInvalid, runtimeSyncedDeletionPolicyInvalid,
	}, nonReady()...)
}

// RuntimeConstructors are provider-runtime's own constructors for the reasons it writes, so a test can
// assert each listed status against what the runtime actually builds.
func RuntimeConstructors() []rtv1.Condition {
	return []rtv1.Condition{
		rtv1.NotObserved(), rtv1.Creating(), rtv1.Deleting(),
		rtv1.ReconcileSuccess(), rtv1.ReconcileError(errors.New("e")), rtv1.ReconcilePaused(),
		rtv1.CreateWithheldByPolicy("m"), rtv1.UpdateWithheldByPolicy("m"),
		rtv1.ManagementPolicyInvalid("m"), rtv1.DeletionPolicyInvalid("m"),
	}
}

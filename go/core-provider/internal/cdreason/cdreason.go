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
//   - TestCDReadyReasonTableIsCurrent renders Table into docs/cd-ready-reasons.md and
//     docs/cd-ready-reasons.json, and fails while either file is stale (core-provider#667: the JSON
//     adds each reason's expected clearance, since-version, the action a person takes and the status
//     fields carrying its detail).
//   - TestReleasedReasonsAreStillListed fails when a reason a release carried
//     (testdata/released-reasons.txt) is no longer in Table and Deprecated does not name it: a
//     consumer keys on the reason string, so a rename or a removal is breaking.
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
	clears   Clearance
	doc      *Doc // a pointer, so Reason stays comparable (a switch on it compiles)
}

// ClearKind is how long a reason is expected to last, in the words the cdc's docs/ready-reasons.md
// uses: steady (an end state), next pass (re-read after the next reconcile) or indefinite (lasts until
// something outside the controller changes). core-provider names no timer, so it has no "timer" kind.
type ClearKind string

const (
	Steady     ClearKind = "steady"
	NextPass   ClearKind = "next pass"
	Indefinite ClearKind = "indefinite"
)

// Clearance is a reason's expected lifetime (core-provider#667). Published in
// docs/cd-ready-reasons.json only: the markdown table has no column for it.
type Clearance struct {
	Kind ClearKind
	Note string
}

// Unreleased is the Since of a reason no release carries yet; write it as `Since: Unreleased`. The
// release that ships it replaces it with its own tag and appends the reason to
// testdata/released-reasons.txt, both done by RELEASE_TAG=X.Y.Z UPDATE_READY_REASONS=1 go test
// ./internal/cdreason/ -run '^TestReasonTablesAreReleaseReady$' (docs/release.md); release-tag.yaml
// runs the same test without UPDATE and refuses a tag that still has one.
const Unreleased = "unreleased"

// Doc is what docs/cd-ready-reasons.json publishes about a reason beyond its condition (#667).
type Doc struct {
	// Since is the first core-provider release tag whose released source contains the reason: the
	// module's own non-test source plus the provider-runtime and plumbing versions its go.mod pins (a
	// runtime-written reason's literal lives in the runtime), and, on a type other than Ready and
	// Synced, the type's literal too. Backfilled once from `git tag -l '2.*'`; for a literal another
	// type also used first, it is a lower bound. Unreleased until a release ships it.
	Since string
	// Action is what a PERSON does on seeing the reason: short, plain, never a command line. The
	// portal shows it as is, so changing it is a UI change. Required unless the reason is healthy or
	// steady.
	Action string
	// DetailFields are JSON paths of the CompositionDefinition status fields written with the reason,
	// beyond the condition's own message. Each must be a field of CompositionDefinitionStatus.
	DetailFields []string
}

// Clears is how long the reason is expected to last.
func (r Reason) Clears() Clearance { return r.clears }

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

// Removed is a reason a release carried and this registry no longer lists (#667).
type Removed struct {
	Condition rtv1.ConditionType
	Reason    rtv1.ConditionReason
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
		"Observe: the CRD serves the version and is Established, and the controller Deployment has rolled out and is available for its current generation (Diego, 2026-11-16); on a follower, the shared controller, and the message names the definition that owns it",
		Clearance{Steady, "the healthy end state"},
		Doc{Since: "2.0.0", DetailFields: []string{"status.owner"}})
	ScaledToZero = ready(conditions.ReasonScaledToZero, metav1.ConditionTrue, WriterCoreProvider, Healthy,
		"Observe: the controller is at 0 replicas and the definition opted into scale-to-zero (#393); it wakes on the first instance",
		Clearance{Steady, "idle until the first instance, which wakes the controller"},
		Doc{Since: "2.13.84", DetailFields: []string{"status.scaleToZero.replicas", "status.scaleToZero.lastSeen"}})
	Creating = ready(rtv1.ReasonCreating, metav1.ConditionFalse, WriterCoreProvider, Converging,
		"Observe: a first pass (seeded before any work), the CRD does not exist yet or is not yet Established, the controller Deployment is absent or rolling out, an update pending with no earlier verdict to keep (#395), or the definition has just become the controller's owner (#387); for a pool member whose pool runs, its controller is starting there, its ownership Lease not yet held by the pool for less than G (20s at the default timings), or is taking over from the Lease's current holder (#653). Update: the pass that switches status to a new version, until Observe looks at its controller",
		Clearance{Indefinite, "until the CRD is Established and the controller Deployment has rolled out; no deadline of its own"},
		Doc{Since: "2.0.0", Action: "Wait: it clears when the CRD is established and the controller has rolled out."})
	Unavailable = ready(rtv1.ReasonUnavailable, metav1.ConditionFalse, WriterCoreProvider, Failed,
		"Observe: the chart cannot be read, the controller has 0 replicas without opting into scale-to-zero, the controller Deployment gave up its rollout (ProgressDeadlineExceeded) or lost availability after a completed one, a pool member's controller is not running in its pool (its ownership Lease not held by the pool for G or more, #653), or adding the served version is refused (a held version, no room); the message says which, and for lost availability ends with the first not-ready pod's cause, e.g. \"; pod <name>: restarting (last exit: Error)\" (#559)",
		Clearance{Indefinite, "until the cause the message names is fixed"},
		Doc{Since: "2.0.0", Action: "Fix what the message names: the chart, the controller's replicas or rollout, or the served version that cannot be added."})
	RemoteChartUnreachable = ready(ReasonRemoteChartUnreachable, metav1.ConditionFalse, WriterCoreProvider, Failed,
		"remote seed: the chart's URL is cluster-local to the management cluster, so the remote target cannot fetch it",
		Clearance{Indefinite, "until the chart's URL is one the remote target can fetch"},
		Doc{Since: "2.6.0", Action: "Point the definition at a chart URL the remote cluster can reach."})
	SchemaDefaultContradiction = ready(ReasonSchemaDefaultContradiction, metav1.ConditionFalse, WriterCoreProvider, Failed,
		"Observe: a `default` in the chart's values.schema.json that crdgen carries into the CRD fails its own subschema, so no CRD is generated; the message names each JSON pointer (plumbing#205). Nothing is applied, and a definition that worked before keeps serving its last CRD",
		Clearance{Indefinite, "until the chart's values.schema.json is fixed"},
		Doc{Since: "2.13.108", Action: "Fix each default the message names in the chart's values.schema.json, then use that chart version."})
	ValuesSchemaMissing = ready(ReasonValuesSchemaMissing, metav1.ConditionFalse, WriterCoreProvider, Failed,
		"Observe: the chart has no values.schema.json at its root, so no typed CRD can be generated and nothing is applied; the message names the chart (#566)",
		Clearance{Indefinite, "until the chart ships a root values.schema.json, or the definition opts into an inferred schema"},
		Doc{Since: "2.13.108", Action: "Add a values.schema.json at the chart's root, or opt the definition into an inferred schema with the krateo.io/values-schema: infer annotation."})
	DerivedNameTooLong = ready(ReasonDerivedNameTooLong, metav1.ConditionFalse, WriterCoreProvider, Failed,
		"Observe: a name derived from the chart's name and Chart.yaml version does not fit Kubernetes' limits: the CRD version is not a DNS-1035 label, the controller Deployment's name (also its selector label) is over 63 characters, or, with cdc.metrics.enabled, the controller Service's is; the message names each name, its length and the problem. Nothing is applied, and a definition that worked before keeps what it has (#588)",
		Clearance{Indefinite, "until the chart's name and Chart.yaml version yield names that fit"},
		Doc{Since: "2.13.114", Action: "Shorten the chart's name or its Chart.yaml version: the message names each name that does not fit."})
	Deleting = ready(rtv1.ReasonDeleting, metav1.ConditionFalse, WriterCoreProvider, Converging,
		"Delete: the definition is being deleted",
		Clearance{Indefinite, "until the deletion completes"},
		Doc{Since: "2.0.0", Action: "Wait: it clears when the deletion completes."})
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
	// on the pass that wrote the condition. Present only while the definition has an apiRef.
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
		"the controller Deployment has replicas declared; the message says whether scale-to-zero applies and when; for a pool member, its pool runs, and its controller holds its ownership Lease, is starting or is taking over (#653)",
		Clearance{Steady, "the healthy end state"},
		Doc{Since: "2.13.84"})
	ControllerScaledToZero = on(TypeControllerActive, conditions.ReasonScaledToZero, metav1.ConditionFalse, WriterCoreProvider,
		"0 replicas, and the definition opted into scale-to-zero: idle, wakes on the first instance (#393)",
		Clearance{Steady, "idle until the first instance, which wakes the controller"},
		Doc{Since: "2.13.84", DetailFields: []string{"status.scaleToZero.replicas", "status.scaleToZero.lastSeen"}})
	ControllerUnavailable = on(TypeControllerActive, rtv1.ReasonUnavailable, metav1.ConditionFalse, WriterCoreProvider,
		"0 replicas, and the definition did NOT opt into scale-to-zero, or, for a pool member, its ownership Lease has not been held by the pool for G or more (#653): nothing reconciles the Kind",
		Clearance{Indefinite, "until the controller has replicas, the definition opts into scale-to-zero, or the pool holds the member's Lease"},
		Doc{Since: "2.13.84", Action: "Give the controller replicas, or opt the definition into scale-to-zero; for a pool member, an admin checks its pool."})
	ControllerNotObserved = on(TypeControllerActive, rtv1.ReasonNotObserved, metav1.ConditionUnknown, WriterCoreProvider,
		"the controller Deployment was not found or could not be read, or, for a pool member, core-provider has not listed the ownership Leases yet (#653)",
		Clearance{NextPass, "the next Observe that can read the controller Deployment (or list the pool's Leases)"},
		Doc{Since: "2.13.84", Action: "Wait: it is retried. If it lasts, an admin checks that the controller Deployment exists."})

	SchemaFullyEnforced = on(TypeSchemaFullyEnforced, "FullyEnforced", metav1.ConditionTrue, WriterCoreProvider,
		"the generated CRD enforces every constraint in the chart's values schema",
		Clearance{Steady, "the healthy end state"},
		Doc{Since: "2.13.68"})
	SchemaConstraintsNotEnforced = on(TypeSchemaFullyEnforced, "ConstraintsNotEnforced", metav1.ConditionFalse, WriterCoreProvider,
		"crdgen could not express some constraints, so the cluster accepts values the schema rejects; the message names them (recomputed on every Observe)",
		Clearance{Indefinite, "until the chart's schema or crdgen changes; recomputed on every Observe"},
		Doc{Since: "2.13.68", Action: "Review the constraints the message names: the cluster does not enforce them, so values that break them are accepted."})
	SchemaEnforcedOwnedElsewhere = on(TypeSchemaFullyEnforced, "OwnedByAnotherDefinition", metav1.ConditionUnknown, WriterCoreProvider,
		"a follower: another definition generates the CRD; the message names it",
		Clearance{Steady, "while another definition generates the CRD"},
		Doc{Since: "2.13.68", Action: "Read this on the definition the message names: it generates the CRD.", DetailFields: []string{"status.owner"}})
	SchemaInferred = on(TypeSchemaFullyEnforced, "SchemaInferred", metav1.ConditionUnknown, WriterCoreProvider,
		"the spec schema was inferred (ValuesSchemaAuthored=False): it has no authored constraints, so True, which means authored constraints all enforced, cannot be claimed (#568 D6)",
		Clearance{Steady, "while the spec schema is inferred (ValuesSchemaAuthored=False)"},
		Doc{Since: "2.13.117", Action: "To have constraints enforced, ship a values.schema.json at the chart's root."})

	SpecSchemaCurrent = on(TypeSpecSchemaCurrent, "Current", metav1.ConditionTrue, WriterCoreProvider,
		"the served and generated spec schemas are identical",
		Clearance{Steady, "the healthy end state"},
		Doc{Since: "2.13.104"})
	SpecSchemaLooserPending = on(TypeSpecSchemaCurrent, "LooserSchemaPending", metav1.ConditionFalse, WriterCoreProvider,
		"transient: the generated schema is provably looser and the next Update writes it",
		Clearance{NextPass, "the next Update writes the generated schema"},
		Doc{Since: "2.13.104", Action: "Wait: the next update writes the new schema."})
	SpecSchemaNotLooser = on(TypeSpecSchemaCurrent, "GeneratedSchemaNotLooser", metav1.ConditionFalse, WriterCoreProvider,
		"until the chart version changes: the generated schema differs in a way not provably looser, so the served one is kept rather than risk refusing stored values",
		Clearance{Indefinite, "until the chart version changes"},
		Doc{Since: "2.13.104", Action: "Publish the schema change under a new chart version: the served schema is kept until the version changes."})
	SpecSchemaOwnedElsewhere = on(TypeSpecSchemaCurrent, "OwnedByAnotherDefinition", metav1.ConditionUnknown, WriterCoreProvider,
		"a follower: another definition generates the CRD",
		Clearance{Steady, "while another definition generates the CRD"},
		Doc{Since: "2.13.104", Action: "Read this on the definition the message names: it generates the CRD.", DetailFields: []string{"status.owner"}})

	ApiRefReadSetResolved = on(TypeApiRefRBACComplete, "ReadSetResolved", metav1.ConditionTrue, WriterCoreProvider,
		"the apiRef's RESTAction read-set was resolved and its RBAC rendered",
		Clearance{Steady, "the healthy end state"},
		Doc{Since: "2.13.72"})

	AuthnDelegationRendered = on(TypeAuthnDelegation, "Rendered", metav1.ConditionTrue, WriterCoreProvider,
		"the authn mapping CRD serves spec.delegation and it is rendered onto the mapping",
		Clearance{Steady, "the healthy end state"},
		Doc{Since: "2.13.79"})
	AuthnDelegationUnsupported = on(TypeAuthnDelegation, "DelegationUnsupported", metav1.ConditionFalse, WriterCoreProvider,
		"the installed authn mapping CRD does not serve spec.delegation (authn-crds < 0.37.0), so it is not rendered rather than pruned",
		Clearance{Indefinite, "until authn-crds 0.37.0 or later is installed"},
		Doc{Since: "2.13.79", Action: "An admin upgrades authn's CRDs to 0.37.0 or later."})

	AuthnMappingApplied = on(TypeAuthnMappingApplied, "Applied", metav1.ConditionTrue, WriterCoreProvider,
		"read live on this pass, both authorize: the controller's mapping, where the controller runs, and core-provider's own, on the management cluster (#642)",
		Clearance{Steady, "the healthy end state"},
		Doc{Since: "2.13.97"})
	AuthnNotInstalled = on(TypeAuthnMappingApplied, "AuthnNotInstalled", metav1.ConditionFalse, WriterCoreProvider,
		"the cluster the controller runs in, or the management cluster (for core-provider's own mapping, #642), serves no serviceaccounts.serviceaccount.authn.krateo.io, so the mapping that belongs there is not written; the message names the cluster and what waits on it, and every pass looks again",
		Clearance{Indefinite, "until authn is installed on that cluster; every pass looks again"},
		Doc{Since: "2.13.97", Action: "An admin installs authn on the cluster the message names."})
	AuthnMappingMissing = on(TypeAuthnMappingApplied, "MappingMissing", metav1.ConditionFalse, WriterCoreProvider,
		"read live on this pass: the cluster serves the mapping kind, and the controller's mapping or core-provider's own is absent or does not authorize its ServiceAccount; the message names which. Written again by the next pass that may write; it stays only while a policy withholds update (#642)",
		Clearance{NextPass, "written again by the next pass that may write; stays only while a policy withholds update"},
		Doc{Since: "2.13.129", Action: "Wait: the next pass writes the mapping. If it stays, allow update in the krateo.io/management-policy annotation."})
	AuthnMappingNotObserved = on(TypeAuthnMappingApplied, "NotObserved", metav1.ConditionUnknown, WriterCoreProvider,
		"a mapping could not be read on this pass, so it is not known to be there or to be missing; the message names which and the error, which is on Synced too, and the pass is retried (#642)",
		Clearance{NextPass, "the pass is retried"},
		Doc{Since: "2.13.97", Action: "Wait: it is retried. If it lasts, the message names the error."})

	ValuesSchemaAuthored = on(TypeValuesSchemaAuthored, "Authored", metav1.ConditionTrue, WriterCoreProvider,
		"the chart ships values.schema.json at its root, and the CRD is generated from it",
		Clearance{Steady, "the healthy end state"},
		Doc{Since: "2.13.117"})
	ValuesSchemaInferred = on(TypeValuesSchemaAuthored, "InferredFromValues", metav1.ConditionFalse, WriterCoreProvider,
		"the chart ships no values.schema.json and the definition opted in (krateo.io/values-schema: infer): the schema is inferred from values.yaml, typed-open, with no authored constraints; the message gives the rules version and the untyped paths",
		Clearance{Steady, "while the chart ships no values.schema.json and the definition opts into inference"},
		Doc{Since: "2.13.117", Action: "To have an authored schema, ship a values.schema.json at the chart's root."})
	ValuesSchemaOwnedElsewhere = on(TypeValuesSchemaAuthored, "OwnedByAnotherDefinition", metav1.ConditionUnknown, WriterCoreProvider,
		"a follower: another definition generates the CRD; the message names it",
		Clearance{Steady, "while another definition generates the CRD"},
		Doc{Since: "2.13.117", Action: "Read this on the definition the message names: it generates the CRD.", DetailFields: []string{"status.owner"}})

	MigratedValuesAllValid = on(TypeMigratedValuesValid, "AllValid", metav1.ConditionTrue, WriterCoreProvider,
		"every instance on the served version passed a dry-run create through it after the last migration; nothing reopens this until the next migration",
		Clearance{Steady, "until the next migration"},
		Doc{Since: "2.13.125"})
	MigratedValuesRefused = on(TypeMigratedValuesValid, "ValuesRefusedByNewVersion", metav1.ConditionFalse, WriterCoreProvider,
		"some instances hold values the served version refuses (validation ratcheting let the migration through); the message names them and the fields, and a write that changes those fields will be refused until they satisfy the new schema; rechecked on every Observe until all pass",
		Clearance{Indefinite, "until the named instances satisfy the served version's schema; rechecked on every Observe"},
		Doc{Since: "2.13.125", Action: "Edit the named compositions' values so they satisfy the new version's schema.", DetailFields: []string{"status.migratedValues.version", "status.migratedValues.refusedCount", "status.migratedValues.refused"}})
	MigratedValuesUnchecked = on(TypeMigratedValuesValid, "ValidationIncomplete", metav1.ConditionUnknown, WriterCoreProvider,
		"no instance was refused, but some could not be judged (the dry run failed for a reason that is not a spec validation error); the message names them; rechecked on every Observe",
		Clearance{NextPass, "rechecked on every Observe"},
		Doc{Since: "2.13.125", Action: "Wait: it is rechecked on every pass. If it lasts, the message names the compositions that could not be checked.", DetailFields: []string{"status.migratedValues.uncheckedCount", "status.migratedValues.unchecked"}})
)

// apiRefRBACFailures are ApiRefRBACComplete=False, one per restactionrbac.Cause: each sends an
// operator somewhere different, which is why they are told apart.
var apiRefRBACFailures = map[restactionrbac.Cause]Reason{
	restactionrbac.CauseRESTActionNotFound: on(TypeApiRefRBACComplete, rtv1.ConditionReason(restactionrbac.CauseRESTActionNotFound), metav1.ConditionFalse, WriterCoreProvider,
		"snowplow says the RESTAction does not exist",
		Clearance{Indefinite, "until the RESTAction exists; retried on every pass"},
		Doc{Since: "2.13.72", Action: "Create the RESTAction the apiRef names, or fix the apiRef."}),
	restactionrbac.CauseRESTActionForbidden: on(TypeApiRefRBACComplete, rtv1.ConditionReason(restactionrbac.CauseRESTActionForbidden), metav1.ConditionFalse, WriterCoreProvider,
		"the caller may not read the RESTAction; it MAY ALSO NOT EXIST, so this never means missing",
		Clearance{Indefinite, "until the caller may read the RESTAction (it may also not exist); retried on every pass"},
		Doc{Since: "2.13.72", Action: "An admin grants read access to the RESTAction, and checks that it exists."}),
	restactionrbac.CauseStageUnresolvable: on(TypeApiRefRBACComplete, rtv1.ConditionReason(restactionrbac.CauseStageUnresolvable), metav1.ConditionFalse, WriterCoreProvider,
		"a stage cannot be enumerated without dispatching it",
		Clearance{Indefinite, "until the RESTAction changes; retried on every pass"},
		Doc{Since: "2.13.72", Action: "Change the RESTAction stage the message names so it can be resolved without running it."}),
	restactionrbac.CauseInvalidApiRef: on(TypeApiRefRBACComplete, rtv1.ConditionReason(restactionrbac.CauseInvalidApiRef), metav1.ConditionFalse, WriterCoreProvider,
		"snowplow refused the request itself, such as extras naming a reserved key",
		Clearance{Indefinite, "until the request snowplow refused is fixed; retried on every pass"},
		Doc{Since: "2.13.72", Action: "Fix the apiRef: the message says what snowplow refused."}),
	restactionrbac.CauseUnauthorized: on(TypeApiRefRBACComplete, rtv1.ConditionReason(restactionrbac.CauseUnauthorized), metav1.ConditionFalse, WriterCoreProvider,
		"no usable identity reached snowplow",
		Clearance{Indefinite, "until a usable identity reaches snowplow; retried on every pass"},
		Doc{Since: "2.13.72", Action: "An admin checks the identity core-provider uses to call snowplow."}),
	restactionrbac.CauseSnowplowUnavailable: on(TypeApiRefRBACComplete, rtv1.ConditionReason(restactionrbac.CauseSnowplowUnavailable), metav1.ConditionFalse, WriterCoreProvider,
		"snowplow could not be reached, or ran out of time",
		Clearance{NextPass, "retried on every pass; persists while snowplow cannot be reached"},
		Doc{Since: "2.13.72", Action: "Wait: it is retried. If it lasts, an admin checks that snowplow is running."}),
	restactionrbac.CauseSnowplowError: on(TypeApiRefRBACComplete, rtv1.ConditionReason(restactionrbac.CauseSnowplowError), metav1.ConditionFalse, WriterCoreProvider,
		"snowplow, or something in front of it, answered in a way nothing here recognises (including cyclic stages)",
		Clearance{Indefinite, "until snowplow, or what is in front of it, answers as expected; retried on every pass"},
		Doc{Since: "2.13.72", Action: "An admin checks snowplow and anything in front of it: the message names the answer."}),
	restactionrbac.CauseNotConfigured: on(TypeApiRefRBACComplete, rtv1.ConditionReason(restactionrbac.CauseNotConfigured), metav1.ConditionFalse, WriterCoreProvider,
		"core-provider has no snowplow URL (apiRef RBAC is off in the chart) while the definition declares an apiRef",
		Clearance{Indefinite, "until core-provider has a snowplow URL, or the apiRef is removed"},
		Doc{Since: "2.13.72", Action: "An admin turns on apiRef RBAC in core-provider's chart, or the definition's apiRef is removed."}),
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
	return append(out, AuthnDelegationRendered, AuthnDelegationUnsupported, AuthnMappingApplied, AuthnNotInstalled, AuthnMappingMissing, AuthnMappingNotObserved,
		ValuesSchemaAuthored, ValuesSchemaInferred, ValuesSchemaOwnedElsewhere,
		MigratedValuesAllValid, MigratedValuesRefused, MigratedValuesUnchecked)
}

// The reasons provider-runtime's managed reconciler writes on the same objects. Unexported: core-provider
// must not write them itself.
var (
	runtimeNotObserved = ready(rtv1.ReasonNotObserved, metav1.ConditionUnknown, WriterRuntime, Unevaluated,
		"Connect or Observe returned an error not marked ReadyAssessed (for example, the controller Deployment could not be read); the error is on Synced",
		Clearance{NextPass, "the next pass, after the retry backoff; persists while the error does"},
		Doc{Since: "2.13.82", Action: "Wait: it is retried. If it lasts, the error is on Synced."})
	runtimeCreating = ready(rtv1.ReasonCreating, metav1.ConditionFalse, WriterRuntime, Converging,
		"the create path: written with every Create outcome, success or failure (the error, if any, is on Synced)",
		Clearance{NextPass, "the next Observe reports the outcome; the error, if any, is on Synced"},
		Doc{Since: "2.0.0", Action: "Wait: it clears on the next pass. If it lasts, the error is on Synced."})
	runtimeDeleting = ready(rtv1.ReasonDeleting, metav1.ConditionFalse, WriterRuntime, Converging,
		"the delete path, and an Observe error on a definition being deleted",
		Clearance{Indefinite, "until the deletion completes"},
		Doc{Since: "2.0.0", Action: "Wait: it clears when the deletion completes."})

	runtimeSyncedSuccess = on(rtv1.TypeSynced, rtv1.ReasonReconcileSuccess, metav1.ConditionTrue, WriterRuntime,
		"the pass completed",
		Clearance{Steady, "the healthy end state"},
		Doc{Since: "2.0.0"})
	runtimeSyncedError = on(rtv1.TypeSynced, rtv1.ReasonReconcileError, metav1.ConditionFalse, WriterRuntime,
		"the pass failed (message: the error); with Ready=False/SchemaDefaultContradiction it reads \"observe failed: \" followed by Ready's message",
		Clearance{NextPass, "retried with backoff; persists while the cause does"},
		Doc{Since: "2.0.0", Action: "Wait: the pass is retried. If it lasts, fix the error the message names."})
	runtimeSyncedPaused = on(rtv1.TypeSynced, rtv1.ReasonReconcilePaused, metav1.ConditionFalse, WriterRuntime,
		"krateo.io/paused is set; Ready is left as it was",
		Clearance{Steady, "until krateo.io/paused is removed"},
		Doc{Since: "2.0.0", Action: "To resume, remove the krateo.io/paused annotation."})

	// provider-runtime v1.21.0 (unstructured-runtime#110): the management policy reported on a
	// CompositionDefinition that carries krateo.io/management-policy or krateo.io/deletion-policy. The
	// reconciler writes them after core-provider's Observe in the same pass, and writes status once,
	// so there is no second writer: core-provider writes a CompositionDefinition's conditions only
	// through the reconciler's single status write.
	runtimeCreateWithheld = ready(rtv1.ReasonCreateWithheldByPolicy, metav1.ConditionFalse, WriterRuntime, Failed,
		"the external resource (the CRD) is absent and krateo.io/management-policy withholds create, observe-only included; nothing is created until a human changes the policy (#110 D4)",
		Clearance{Indefinite, "until a human changes krateo.io/management-policy to allow create"},
		Doc{Since: "2.13.120", Action: "To create it, change the krateo.io/management-policy annotation to allow create."})
	runtimeUpdateWithheld = ready(rtv1.ReasonUpdateWithheldByPolicy, metav1.ConditionTrue, WriterRuntime, Healthy,
		"core-provider's Observe found drift and krateo.io/management-policy withholds update: replaces a healthy Ready only, so the definition works but is NOT brought to its desired state; an unhealthy Ready keeps its own reason, with \"updates are withheld by policy\" appended (#110 D4)",
		Clearance{Indefinite, "until krateo.io/management-policy allows update"},
		Doc{Since: "2.13.120", Action: "To apply changes, change the krateo.io/management-policy annotation to allow update."})
	runtimeSyncedPolicyInvalid = on(rtv1.TypeSynced, rtv1.ReasonManagementPolicyInvalid, metav1.ConditionFalse, WriterRuntime,
		"krateo.io/management-policy does not parse: the reconciler acts on nothing (observe only); wins over the pass's own Synced outcome; paused and deferred passes keep theirs",
		Clearance{Indefinite, "until a human fixes krateo.io/management-policy"},
		Doc{Since: "2.13.120", Action: "Fix the krateo.io/management-policy annotation: the message names the value it cannot read."})
	runtimeSyncedDeletionPolicyInvalid = on(rtv1.TypeSynced, rtv1.ReasonDeletionPolicyInvalid, metav1.ConditionFalse, WriterRuntime,
		"krateo.io/deletion-policy is neither delete nor orphan, so the external resource is KEPT on delete; reported on every pass while set",
		Clearance{Indefinite, "until a human fixes krateo.io/deletion-policy"},
		Doc{Since: "2.13.120", Action: "Set the krateo.io/deletion-policy annotation to delete or orphan."})
)

func ready(reason rtv1.ConditionReason, status metav1.ConditionStatus, writer Writer, verdict Verdict, meaning string, clears Clearance, doc Doc) Reason {
	return Reason{condType: rtv1.TypeReady, reason: reason, status: status, writer: writer, verdict: verdict, meaning: meaning, clears: clears, doc: &doc}
}

func on(condType rtv1.ConditionType, reason rtv1.ConditionReason, status metav1.ConditionStatus, writer Writer, meaning string, clears Clearance, doc Doc) Reason {
	return Reason{condType: condType, reason: reason, status: status, writer: writer, verdict: NotRead, meaning: meaning, clears: clears, doc: &doc}
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

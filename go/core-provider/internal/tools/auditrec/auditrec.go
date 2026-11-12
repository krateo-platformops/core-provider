// Package auditrec writes core-provider's controller audit records (core-provider#423, AUDIT-4):
// one record per write core-provider sends that changed what an object holds, at the sites the
// issue names (RBAC, credential Secrets, the authn ServiceAccount mapping, the spoke mirror, the
// hub composition delete, a migration's rewrite of a composition).
//
// The write is what plumbing's objectclient reports (created, updated, unchanged; on error, the
// write the final attempt sent), never re-derived here. Whether an update changed content is
// plumbing's kubeutil/content, the same comparison the cdc's helm verdicts use, so the two
// controllers can't disagree about one pair of objects.
package auditrec

import (
	"context"
	"errors"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"net/http"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/krateo-platformops/audit"
	"github.com/krateo-platformops/plumbing/kubeutil/content"
	"github.com/krateo-platformops/plumbing/kubeutil/objectclient"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// Scope is the logger's instrumentation scope: the core-provider population of the audit schema.
const Scope = "github.com/krateo-platformops/core-provider/audit"

// The krateo.action literals (snowplow, 2026-11-12): one per controller action, shared with every
// writer that performs the same action; the object is in object_ref and the verb in the verb key.
const (
	ActionRBACApply            = "rbac.apply"  // Role, RoleBinding, ClusterRole, ClusterRoleBinding, ServiceAccount
	ActionRBACDelete           = "rbac.delete" // the same kinds, torn down
	ActionSecretApply          = "secret.apply"
	ActionSecretDelete         = "secret.delete"
	ActionAuthnMappingApply    = "authn.mapping.apply"
	ActionCompositionMirror    = "composition.mirror"
	ActionCompositionHubDelete = "composition.hub.delete"
	ActionCompositionMigrate   = "composition.migrate"
)

// MetricExcluded counts the writes core-provider sent that get no record, by reason.
const MetricExcluded = "core_provider_audit_excluded_total"

// Excluded reasons.
const (
	// ExcludedUnchanged: an Update the apiserver persisted nothing for.
	ExcludedUnchanged = "unchanged"
	// ExcludedMetadataOnly: a persisted Update whose content (content.Differs) did not change.
	ExcludedMetadataOnly = "metadata_only"
)

// Target names the object a write was for.
type Target struct {
	GVR       schema.GroupVersionResource
	Kind      string
	Namespace string
	Name      string
	// Namespaces reads the preview label from the cluster the object lives on, when that is not
	// the Recorder's own (a spoke). nil uses the Recorder's reader. The schema has the writer read
	// the target's Namespace from apiserver state; a spoke object's Namespace is on the spoke.
	Namespaces NamespaceLabels
}

// LabelsFrom reads Namespace labels through dyn: the reader for a target on dyn's cluster.
func LabelsFrom(dyn dynamic.Interface) NamespaceLabels {
	return func(ctx context.Context, name string) (map[string]string, error) {
		ns, err := dyn.Resource(schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		return ns.GetLabels(), nil
	}
}

// LabelsFromClient reads Namespace labels through a controller-runtime client.
func LabelsFromClient(c client.Client) NamespaceLabels {
	return func(ctx context.Context, name string) (map[string]string, error) {
		var ns corev1.Namespace
		if err := c.Get(ctx, client.ObjectKey{Name: name}, &ns); err != nil {
			return nil, err
		}
		return ns.GetLabels(), nil
	}
}

// NamespaceLabels reads a Namespace's labels from the cluster the object lives on.
type NamespaceLabels func(ctx context.Context, name string) (map[string]string, error)

// Recorder writes the records. A nil Recorder records nothing.
type Recorder struct {
	emitter    *audit.Emitter
	namespaces NamespaceLabels
	excluded   metric.Int64Counter
}

// NewRecorder builds a Recorder on emitter, counting on mp. namespaces reads the preview label; nil
// leaves preview unknown (marked).
func NewRecorder(emitter *audit.Emitter, mp metric.MeterProvider, namespaces NamespaceLabels) (*Recorder, error) {
	excluded, err := mp.Meter(Scope).Int64Counter(MetricExcluded,
		metric.WithDescription("Writes core-provider sent that get no audit record, by action and reason: unchanged, metadata_only."))
	if err != nil {
		return nil, err
	}
	return &Recorder{emitter: emitter, namespaces: namespaces, excluded: excluded}, nil
}

// Apply records one objectclient.Apply of t, with its result and error, caused by cause.
//
//	sent nothing (Op "")              no record: no write was sent
//	created                          create / POST / success
//	updated, content changed         update / PUT  / success
//	updated, metadata only           counted metadata_only
//	unchanged                        counted unchanged
//	refused (error, Op created|updated)  create|update / failure, with the apiserver's status code
func (r *Recorder) Apply(ctx context.Context, action string, t Target, res objectclient.ApplyResult, err error, cause audit.CauseRef) {
	r.apply(ctx, action, t, res, err, cause, false)
}

// apply is Apply; knownChanged says the caller knows the write changed content (a migration's
// fieldloss report), which content.Differs over the objects it holds cannot see.
func (r *Recorder) apply(ctx context.Context, action string, t Target, res objectclient.ApplyResult, err error, cause audit.CauseRef, knownChanged bool) {
	if r == nil || res.Op == "" {
		return
	}
	verb := "update"
	if res.Op == objectclient.ApplyCreated {
		verb = "create"
	}
	if err != nil {
		r.emit(ctx, action, verb, t, objectOf(res.Before), audit.OutcomeFailure, statusOf(err), cause)
		return
	}
	switch res.Op {
	case objectclient.ApplyUnchanged:
		r.count(ctx, action, ExcludedUnchanged)
		return
	case objectclient.ApplyUpdated:
		changed, cerr := content.Differs(object(res.Before), object(res.After), audit.AuthorizationAnnotations())
		// A comparison that can't be made is a write whose effect is unknown: recorded, as the cdc
		// records compareFailed (Q1). An unrecorded write is what AUDIT-4 exists to prevent.
		if !knownChanged && cerr == nil && !changed {
			r.count(ctx, action, ExcludedMetadataOnly)
			return
		}
	}
	r.emit(ctx, action, verb, t, objectOf(res.After), audit.OutcomeSuccess, 0, cause)
}

// Delete records one objectclient.Uninstall of t. An absent object is no write of ours: no record.
func (r *Recorder) Delete(ctx context.Context, action string, t Target, res objectclient.UninstallResult, err error, cause audit.CauseRef) {
	if r == nil || res.Op != objectclient.UninstallDeleted {
		return
	}
	outcome, code := audit.OutcomeSuccess, 0
	if err != nil {
		outcome, code = audit.OutcomeFailure, statusOf(err)
	}
	r.emit(ctx, action, "delete", t, objectOf(res.Before), outcome, code, cause)
}

// object is u as a runtime.Object, and an untyped nil when u is nil: a nil *Unstructured inside the
// interface is not == nil, and content.Differs would dereference it instead of reporting it.
func object(u *unstructured.Unstructured) runtime.Object {
	if u == nil {
		return nil
	}
	return u
}

// identity is what object_ref carries beyond the target's name.
type identity struct {
	uid             string
	generation      int64
	resourceVersion string
}

func objectOf(u *unstructured.Unstructured) identity {
	if u == nil {
		return identity{}
	}
	return identity{uid: string(u.GetUID()), generation: u.GetGeneration(), resourceVersion: u.GetResourceVersion()}
}

func (r *Recorder) emit(ctx context.Context, action, verb string, t Target, id identity, outcome string, code int, cause audit.CauseRef) {
	ev := audit.Event{
		Action:     action,
		Verb:       verb,
		HTTPMethod: methodOf(verb),
		StatusCode: code,
		Outcome:    outcome,
		Namespace:  t.Namespace,
		Group:      t.GVR.Group,
		Version:    t.GVR.Version,
		Resource:   t.GVR.Resource,
		Kind:       t.Kind,
		ObjectName: t.Name,
		ObjectUID:  id.uid,
		Preview:    r.preview(ctx, t),
		Cause:      cause,
	}
	// The generation and resourceVersion the authority returned, on a successful non-delete write.
	if outcome == audit.OutcomeSuccess && verb != "delete" {
		ev.ObjectGeneration, ev.ObjectResourceVersion = id.generation, id.resourceVersion
	}
	r.emitter.Emit(ctx, ev)
}

func (r *Recorder) count(ctx context.Context, action, reason string) {
	r.excluded.Add(ctx, 1, metric.WithAttributes(attribute.String("action", action), attribute.String("reason", reason)))
}

// preview is the target's krateo.audit.preview: false for a cluster-scoped object; from its
// Namespace's label otherwise; Unknown (omitted and marked) when the Namespace can't be read.
func (r *Recorder) preview(ctx context.Context, t Target) audit.Preview {
	if t.Namespace == "" {
		return audit.PreviewFalse
	}
	read := r.namespaces
	if t.Namespaces != nil {
		read = t.Namespaces
	}
	if read == nil {
		return audit.PreviewUnknown
	}
	labels, err := read(ctx, t.Namespace)
	if err != nil {
		return audit.PreviewUnknown
	}
	return audit.PreviewOf(labels[previewLabel] == previewValue)
}

const (
	previewLabel = "krateo.io/purpose"
	previewValue = "preview-sandbox"
)

func methodOf(verb string) string {
	switch verb {
	case "create":
		return http.MethodPost
	case "update":
		return http.MethodPut
	case "delete":
		return http.MethodDelete
	}
	return ""
}

// statusOf is the HTTP status the apiserver answered a refused write with, 0 when the error carries
// none (a transport failure: no authority answered).
func statusOf(err error) int {
	var s apierrors.APIStatus
	if errors.As(err, &s) {
		return int(s.Status().Code)
	}
	return 0
}

// scope is one reconcile's recorder and cause, carried in the context the way the reconcile's logger
// is (contexttools.LoggerFromCtx): the write sites are many functions deep, and the cause is the
// CompositionDefinition that reconcile is for.
type scope struct {
	r     *Recorder
	cause audit.CauseRef
}

type scopeKey struct{}

// WithScope returns ctx carrying r and cause for every write recorded under it.
func WithScope(ctx context.Context, r *Recorder, cause audit.CauseRef) context.Context {
	return context.WithValue(ctx, scopeKey{}, scope{r: r, cause: cause})
}

func scopeFrom(ctx context.Context) (scope, bool) {
	s, ok := ctx.Value(scopeKey{}).(scope)
	return s, ok && s.r != nil
}

// RecordApply records one Apply under ctx's scope. Without a scope it records nothing: a write made
// outside a reconcile that set one (a test, a bootstrap) has no cause to name.
func RecordApply(ctx context.Context, action string, t Target, res objectclient.ApplyResult, err error) {
	if s, ok := scopeFrom(ctx); ok {
		s.r.Apply(ctx, action, t, res, err, s.cause)
	}
}

// RecordDelete records one Uninstall under ctx's scope.
func RecordDelete(ctx context.Context, action string, t Target, res objectclient.UninstallResult, err error) {
	if s, ok := scopeFrom(ctx); ok {
		s.r.Delete(ctx, action, t, res, err, s.cause)
	}
}

// UpdateResult is what an Update a site sends itself (not through objectclient) did, in
// objectclient's terms: the read object it replaced, and what the apiserver returned. An unmoved
// resourceVersion is an Update the apiserver short-circuited: nothing was persisted (the same rule
// objectclient applies). On error the write was sent and refused.
func UpdateResult(before, after *unstructured.Unstructured, err error) objectclient.ApplyResult {
	if err != nil {
		return objectclient.ApplyResult{Op: objectclient.ApplyUpdated, Before: before}
	}
	op := objectclient.ApplyUpdated
	if before != nil && after != nil && before.GetResourceVersion() == after.GetResourceVersion() {
		op = objectclient.ApplyUnchanged
	}
	return objectclient.ApplyResult{Op: op, Before: before, After: after}
}

// RecordUpdate records an Update a site sent itself. knownChanged is true when the site knows the
// write changed content though the objects it holds can't show it: a migration that rewrites a
// composition through a narrower version discards fields its own read (through that version) was
// already missing, so its fieldloss report, not content.Differs, is the authority (core-provider#188).
func RecordUpdate(ctx context.Context, action string, t Target, before, after *unstructured.Unstructured, err error, knownChanged bool) {
	if s, ok := scopeFrom(ctx); ok {
		s.r.apply(ctx, action, t, UpdateResult(before, after, err), err, s.cause, knownChanged)
	}
}

// RecordDeleteSent records a Delete a site sent itself, by name; err is the apiserver's answer. A
// NotFound means someone else removed it first: not this controller's write, no record. uid is the
// object's when the site read it, "" when it deleted by name alone (object_ref.uid is then omitted,
// as the schema allows for an object that isn't known).
func RecordDeleteSent(ctx context.Context, action string, t Target, uid string, err error) {
	if apierrors.IsNotFound(err) {
		return
	}
	var before *unstructured.Unstructured
	if uid != "" {
		before = &unstructured.Unstructured{Object: map[string]interface{}{}}
		before.SetUID(types.UID(uid))
	}
	RecordDelete(ctx, action, t, objectclient.UninstallResult{Op: objectclient.UninstallDeleted, Before: before}, err)
}

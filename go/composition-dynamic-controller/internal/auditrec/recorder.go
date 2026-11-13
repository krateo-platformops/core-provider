package auditrec

import (
	"context"

	"github.com/krateo-platformops/audit"
	helmconfig "github.com/krateo-platformops/plumbing/helm"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// PreviewLabel is the Namespace label that makes a child a preview-sandbox object (audit schema
// krateo.audit.preview; installer#716 §2.4).
const (
	PreviewLabel      = "krateo.io/purpose"
	PreviewLabelValue = "preview-sandbox"
)

// The counters. Every child of an operation is recorded, deliberately excluded (counted by reason),
// or never attempted (counted on its own), so an absent record is never ambiguous.
const (
	// MetricExcluded counts the children of an operation the audit deliberately records no row
	// for, by reason: metadata_only, unchanged, hook.
	MetricExcluded = "composition_audit_excluded_total"
	// MetricChildrenNotAttempted counts the children an operation never reached: helm stopped at a
	// failed create, so no request was sent. They are not writes, so the audit records none
	// (snowplow, 2026-11-12: the failed create that stopped helm is the record). This is
	// operational state, not an audit gap.
	MetricChildrenNotAttempted = "composition_children_not_attempted_total"
	// MetricIncompleteOperations counts the operations whose verdicts may not name every child
	// written (plumbing's ObjectsComplete=false), by action: their records may be short.
	MetricIncompleteOperations = "composition_audit_incomplete_operations_total"
)

// Excluded reasons, beside ExcludedMetadataOnly.
const (
	// ExcludedUnchanged: a child the operation left as it was. Not a write.
	ExcludedUnchanged = "unchanged"
	// ExcludedHook: a hook write (helm.sh/hook). Hooks are not release children.
	ExcludedHook = "hook"
)

// Operation is what one helm operation reported, in the shape plumbing's two result types share.
type Operation struct {
	Action        string
	Objects       []helmconfig.ObjectVerdict
	Complete      bool
	NotAttempted  int
	HooksExcluded int
	// ReleaseChildren is how many children the release the operation LEFT names, or -1 when that is
	// not known (the operation failed, or left no release). It only feeds the unchanged count.
	ReleaseChildren int
}

// FromReconcile is a Reconcile's operation. res may be nil (a Reconcile that wrote nothing).
func FromReconcile(action string, res *helmconfig.ReconcileResult, releaseChildren int) Operation {
	if res == nil {
		return Operation{Action: action, Complete: true, ReleaseChildren: releaseChildren}
	}
	return Operation{Action: action, Objects: res.Objects, Complete: res.ObjectsComplete,
		NotAttempted: res.ObjectsNotAttempted, HooksExcluded: res.HooksExcluded, ReleaseChildren: releaseChildren}
}

// FromOperation is an Install's, Upgrade's, Uninstall's or Rollback's operation. op may be nil (it
// failed before writing anything).
func FromOperation(action string, op *helmconfig.OperationResult, releaseChildren int) Operation {
	if op == nil {
		return Operation{Action: action, Complete: true, ReleaseChildren: releaseChildren}
	}
	return Operation{Action: action, Objects: op.Objects, Complete: op.ObjectsComplete,
		NotAttempted: op.ObjectsNotAttempted, HooksExcluded: op.HooksExcluded, ReleaseChildren: releaseChildren}
}

// NamespaceLabels reads a Namespace's labels from the cluster the children live on.
type NamespaceLabels func(ctx context.Context, name string) (map[string]string, error)

// Recorder writes the controller audit records of the cdc's helm operations.
type Recorder struct {
	// resource is the plural of the composition kind this cdc reconciles (its -resource flag), the
	// cause_ref.resource of every record.
	resource     string
	emitter      *audit.Emitter
	namespaces   NamespaceLabels
	excluded     metric.Int64Counter
	notAttempted metric.Int64Counter
	incomplete   metric.Int64Counter
}

// NewRecorder builds a Recorder for the compositions of resource (the plural), emitting on emitter
// and counting on mp.
func NewRecorder(resource string, emitter *audit.Emitter, mp metric.MeterProvider, namespaces NamespaceLabels) (*Recorder, error) {
	m := mp.Meter(Scope)
	excluded, err := m.Int64Counter(MetricExcluded,
		metric.WithDescription("Children of a helm operation the audit deliberately records no row for, by reason: metadata_only, unchanged, hook."))
	if err != nil {
		return nil, err
	}
	notAttempted, err := m.Int64Counter(MetricChildrenNotAttempted,
		metric.WithDescription("Children a helm operation never reached (no request sent: helm stopped at a failed create), by action."))
	if err != nil {
		return nil, err
	}
	incomplete, err := m.Int64Counter(MetricIncompleteOperations,
		metric.WithDescription("Helm operations whose per-child verdicts may not name every child written, by action: their audit records may be short."))
	if err != nil {
		return nil, err
	}
	return &Recorder{resource: resource, emitter: emitter, namespaces: namespaces, excluded: excluded, notAttempted: notAttempted, incomplete: incomplete}, nil
}

// Record writes one record per recorded verdict of op, every one with the composition mg as its
// cause_ref, and counts the rest. ctx should carry the reconcile's span, so the records join its
// trace and mark it recorded. A nil Recorder records nothing.
func (r *Recorder) Record(ctx context.Context, op Operation, mg *unstructured.Unstructured) {
	if r == nil {
		return
	}
	cause := CauseOf(mg, r.resource)
	previews := map[string]audit.Preview{}
	inRelease := map[string]bool{}
	for _, v := range op.Objects {
		if leftInRelease(v.Change) {
			inRelease[v.Group+"/"+v.Kind+"/"+v.Namespace+"/"+v.Name] = true
		}
		ev, ok, excluded := Event(op.Action, v, cause, r.preview(ctx, previews, v.Namespace))
		if !ok {
			r.count(ctx, op.Action, excluded, 1)
			continue
		}
		r.emitter.Emit(ctx, ev)
	}
	if op.ReleaseChildren >= 0 {
		if n := op.ReleaseChildren - len(inRelease); n > 0 {
			r.count(ctx, op.Action, ExcludedUnchanged, n)
		}
	}
	r.count(ctx, op.Action, ExcludedHook, op.HooksExcluded)
	if op.NotAttempted > 0 {
		r.notAttempted.Add(ctx, int64(op.NotAttempted), metric.WithAttributes(attribute.String("action", op.Action)))
	}
	if !op.Complete {
		r.incomplete.Add(ctx, 1, metric.WithAttributes(attribute.String("action", op.Action)))
	}
}

func (r *Recorder) count(ctx context.Context, action, reason string, n int) {
	if n <= 0 {
		return
	}
	r.excluded.Add(ctx, int64(n), metric.WithAttributes(attribute.String("action", action), attribute.String("reason", reason)))
}

// preview is the child's krateo.audit.preview, read once per Namespace per operation. A
// cluster-scoped child is false (the schema's rule). A Namespace that cannot be read, absent
// included, is Unknown, which the emitter omits and marks: a deleted Namespace's label is not
// knowable, and false would assert it.
func (r *Recorder) preview(ctx context.Context, memo map[string]audit.Preview, ns string) audit.Preview {
	if ns == "" {
		return audit.PreviewFalse
	}
	if p, ok := memo[ns]; ok {
		return p
	}
	p := audit.PreviewUnknown
	if r.namespaces != nil {
		if labels, err := r.namespaces(ctx, ns); err == nil {
			p = audit.PreviewFalse
			if labels[PreviewLabel] == PreviewLabelValue {
				p = audit.PreviewTrue
			}
		}
	}
	memo[ns] = p
	return p
}

// leftInRelease: the verdict names a child the release the operation left still renders, so it is
// not an unchanged one. A deletion (done, not done, unknown or kept) names one the release dropped.
func leftInRelease(c helmconfig.ObjectChange) bool {
	switch c {
	case helmconfig.ChangeDeleted, helmconfig.ChangeDeleteNotDone, helmconfig.ChangeDeleteUnknown, helmconfig.ChangeKept:
		return false
	}
	return true
}

// CauseOf is the cause_ref of every record a reconcile of mg writes: the composition, by its group,
// version, plural resource, namespace, name, uid and the generation this reconcile acts on.
func CauseOf(mg *unstructured.Unstructured, resource string) audit.CauseRef {
	gvk := mg.GroupVersionKind()
	return audit.CauseRef{
		Group: gvk.Group, Version: gvk.Version, Resource: resource,
		Namespace: mg.GetNamespace(), Name: mg.GetName(),
		UID: string(mg.GetUID()), Generation: mg.GetGeneration(),
	}
}

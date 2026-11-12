package auditrec

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/krateo-platformops/audit"
	"github.com/krateo-platformops/audit/audittest"
	helmconfig "github.com/krateo-platformops/plumbing/helm"
	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

const (
	actor   = "system:serviceaccount:krateo-system:exampleapps-v1-2-0-controller"
	preview = "preview-ns"
	plain   = "plain-ns"
	gone    = "gone-ns"
)

func composition() *unstructured.Unstructured {
	mg := &unstructured.Unstructured{}
	mg.SetGroupVersionKind(schema.GroupVersionKind{Group: "composition.krateo.io", Version: "v1-2-0", Kind: "ExampleApp"})
	mg.SetNamespace("example-ns")
	mg.SetName("example-composition")
	mg.SetUID(types.UID("00000000-0000-4000-8000-0000000000c1"))
	mg.SetGeneration(4)
	return mg
}

type harness struct {
	rec    *audittest.Recorder
	reader *sdkmetric.ManualReader
	r      *Recorder
	reads  map[string]int
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{rec: audittest.NewRecorder(t), reader: sdkmetric.NewManualReader(), reads: map[string]int{}}
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(h.reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	e := audit.New(h.rec.Logger(Scope), audit.WithWriter(audit.WriterController), audit.WithActor(actor, audit.ClassController),
		audit.WithMeterProvider(mp))
	r, err := NewRecorder("exampleapps", e, mp, func(_ context.Context, name string) (map[string]string, error) {
		h.reads[name]++
		switch name {
		case preview:
			return map[string]string{PreviewLabel: PreviewLabelValue}, nil
		case plain:
			return map[string]string{"other": "x"}, nil
		}
		return nil, errors.New("namespaces \"" + name + "\" not found")
	})
	if err != nil {
		t.Fatal(err)
	}
	h.r = r
	return h
}

// counts sums MetricExcluded by reason, MetricChildrenNotAttempted under "not_attempted" and
// MetricIncompleteOperations under "incomplete".
func (h *harness) counts(t *testing.T) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := h.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
			for _, dp := range sum.DataPoints {
				switch m.Name {
				case MetricExcluded:
					r, _ := dp.Attributes.Value("reason")
					out[r.AsString()] += dp.Value
				case MetricChildrenNotAttempted:
					out["not_attempted"] += dp.Value
				case MetricIncompleteOperations:
					out["incomplete"] += dp.Value
				}
			}
		}
	}
	return out
}

func attr(rec sdklog.Record, key string) (attribute.Value, bool) {
	for _, kv := range audittest.Attributes(rec) {
		if string(kv.Key) == key {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}

func child(name, ns string, c helmconfig.ObjectChange, verb string) helmconfig.ObjectVerdict {
	return helmconfig.ObjectVerdict{Group: "apps", Version: "v1", Resource: "deployments", Kind: "Deployment",
		Namespace: ns, Name: name, UID: "11111111-2222-4333-8444-555555555555", Verb: verb, Change: c}
}

// One operation with every shape: each recorded verdict is one conformant record whose cause_ref is
// the composition; every other child is counted by reason, so records + counts account for all.
func TestRecordAccountsForEveryChild(t *testing.T) {
	h := newHarness(t)
	op := Operation{
		Action: ActionUpgrade,
		Objects: []helmconfig.ObjectVerdict{
			child("created", preview, helmconfig.ChangeCreated, "create"),
			child("labels", plain, helmconfig.ChangeMetadataOnly, "patch"),
			child("kept", preview, helmconfig.ChangeKept, "delete"),
			{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles", Kind: "ClusterRole",
				Name: "pruned", Verb: "delete", Change: helmconfig.ChangeDeleted},
			child("unreadable", gone, helmconfig.ChangeSpec, "patch"),
		},
		Complete:        false,
		NotAttempted:    1,
		HooksExcluded:   2,
		ReleaseChildren: 6, // created, labels, unreadable + 3 the operation left alone
	}
	h.r.Record(context.Background(), op, composition())

	recs := h.rec.Records()
	if len(recs) != 4 {
		t.Fatalf("records = %d, want 4 (created, kept, pruned, unreadable; metadataOnly excluded)", len(recs))
	}
	byName := map[string]sdklog.Record{}
	for _, r := range recs {
		n, _ := attr(r, "krateo.audit.object_ref.name")
		byName[n.AsString()] = r
		if r.Body().AsString() != "" {
			t.Errorf("%s: the body is a closed set and this site writes none, got %q", n.AsString(), r.Body().AsString())
		}
		if u, _ := attr(r, "krateo.audit.cause_ref.uid"); u.AsString() != "00000000-0000-4000-8000-0000000000c1" {
			t.Errorf("%s: cause_ref.uid = %q, want the composition's", n.AsString(), u.AsString())
		}
		if g, _ := attr(r, "krateo.audit.cause_ref.generation"); g.AsInt64() != 4 {
			t.Errorf("%s: cause_ref.generation = %d, want 4", n.AsString(), g.AsInt64())
		}
		if res, _ := attr(r, "krateo.audit.cause_ref.resource"); res.AsString() != "exampleapps" {
			t.Errorf("%s: cause_ref.resource = %q, want exampleapps", n.AsString(), res.AsString())
		}
		if a, _ := attr(r, "krateo.action"); a.AsString() != ActionUpgrade {
			t.Errorf("%s: krateo.action = %q", n.AsString(), a.AsString())
		}
	}
	// Preview, read from the child's Namespace.
	for name, want := range map[string]bool{"created": true, "kept": true, "pruned": false} {
		v, ok := attr(byName[name], "krateo.audit.preview")
		if !ok || v.AsBool() != want {
			t.Errorf("%s: preview = %v (present %v), want %v", name, v.AsBool(), ok, want)
		}
	}
	// An unreadable Namespace: the key is omitted and the record marked, never guessed false.
	if _, ok := attr(byName["unreadable"], "krateo.audit.preview"); ok {
		t.Error("unreadable: an unreadable Namespace must leave preview omitted")
	}
	if m, _ := attr(byName["unreadable"], "krateo.audit.incomplete"); !strings.Contains(m.AsString(), "preview") {
		t.Errorf("unreadable: want krateo.audit.incomplete to name preview, got %q", m.AsString())
	}
	audittest.AssertConformant(t, audit.WriterController, byName["unreadable"], audittest.AllowIncomplete(audit.KeyPreview))
	for _, name := range []string{"created", "kept", "pruned"} {
		audittest.AssertConformant(t, audit.WriterController, byName[name])
		audittest.AssertRequiredKeysDetected(t, audit.WriterController, byName[name])
	}
	if o, _ := attr(byName["kept"], "krateo.audit.outcome"); o.AsString() != audit.OutcomeDenied {
		t.Errorf("kept: outcome %q, want denied", o.AsString())
	}

	got := h.counts(t)
	want := map[string]int64{
		ExcludedMetadataOnly: 1,
		ExcludedUnchanged:    3, // 6 in the release, 3 with an in-release verdict
		ExcludedHook:         2,
		"not_attempted":      1,
		"incomplete":         1,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("count %s = %d, want %d (all: %v)", k, got[k], v, got)
		}
	}
	// Each Namespace is read once per operation, however many children it holds.
	if h.reads[preview] != 1 {
		t.Errorf("the preview Namespace was read %d times, want 1", h.reads[preview])
	}
}

// An unchanged reconcile writes no record and counts every child as unchanged.
func TestUnchangedReconcileIsCountedNotRecorded(t *testing.T) {
	h := newHarness(t)
	h.r.Record(context.Background(), FromReconcile(ActionReconcile, &helmconfig.ReconcileResult{ObjectsComplete: true}, 1), composition())
	if n := len(h.rec.Records()); n != 0 {
		t.Fatalf("records = %d, want 0", n)
	}
	if got := h.counts(t); got[ExcludedUnchanged] != 1 || got["incomplete"] != 0 {
		t.Errorf("counts = %v, want unchanged=1 and no incomplete", got)
	}
}

// A failed operation's release is not known (-1), so nothing is counted unchanged on a guess.
func TestUnknownReleaseCountsNoUnchanged(t *testing.T) {
	h := newHarness(t)
	h.r.Record(context.Background(), FromOperation(ActionInstall, &helmconfig.OperationResult{
		Objects:         []helmconfig.ObjectVerdict{child("failed", plain, helmconfig.ChangeCreateFailed, "create")},
		ObjectsComplete: true,
	}, -1), composition())
	if n := len(h.rec.Records()); n != 1 {
		t.Fatalf("records = %d, want 1 (the failed create)", n)
	}
	if got := h.counts(t); got[ExcludedUnchanged] != 0 {
		t.Errorf("unchanged = %d, want 0 when the release is unknown", got[ExcludedUnchanged])
	}
}

// A sentinel token-shaped string fed to every input the site sees reaches no body, and appears only
// in the identity keys it was given for.
func TestNoInputReachesTheBody(t *testing.T) {
	const sentinel = "eyJhbGciOiJSUzI1NiJ9.sentinel-token.c2lnbmF0dXJl"
	h := newHarness(t)
	mg := composition()
	mg.SetName("comp-" + sentinel)
	v := child("child-"+sentinel, plain, helmconfig.ChangeUpdateFailed, "patch")
	h.r.Record(context.Background(), Operation{Action: ActionUpgrade, Objects: []helmconfig.ObjectVerdict{v}, Complete: true, ReleaseChildren: -1}, mg)
	r := h.rec.Only(t)
	if strings.Contains(r.Body().AsString(), sentinel) {
		t.Errorf("the body echoes an input: %q", r.Body().AsString())
	}
	allowed := map[string]bool{
		"krateo.audit.object_ref.name": true, "krateo.audit.cause_ref.name": true,
		// The v0.3 key written beside object_ref.name during W1.
		"k8s.resource.name": true,
	}
	var seen []string
	for _, kv := range audittest.Attributes(r) {
		if !strings.Contains(kv.Value.Emit(), sentinel) {
			continue
		}
		seen = append(seen, string(kv.Key))
		if !allowed[string(kv.Key)] {
			t.Errorf("the sentinel reached %s", kv.Key)
		}
	}
	// Control: the sentinel really was fed in, and lands where it was given.
	if n, _ := attr(r, "krateo.audit.object_ref.name"); n.AsString() != "child-"+sentinel {
		t.Errorf("control: object_ref.name = %q, want the sentinel child name", n.AsString())
	}
	t.Logf("the sentinel appears in %d keys: %v", len(seen), seen)
}

// Emitted while the reconcile's span records, the record marks that span krateo.audit.recorded.
func TestRecordMarksTheReconcileSpan(t *testing.T) {
	h := newHarness(t)
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	ctx, span := tp.Tracer("test").Start(context.Background(), "reconcile")
	h.r.Record(ctx, Operation{Action: ActionReconcile, Objects: []helmconfig.ObjectVerdict{child("c", plain, helmconfig.ChangeCreated, "create")}, Complete: true, ReleaseChildren: -1}, composition())
	span.End()
	var marked bool
	for _, s := range sr.Ended() {
		for _, kv := range s.Attributes() {
			if string(kv.Key) == audit.SpanKeyRecorded && kv.Value.AsBool() {
				marked = true
			}
		}
	}
	if !marked {
		t.Error("the reconcile span must carry krateo.audit.recorded=true")
	}
}

func TestNilRecorderIsANoop(t *testing.T) {
	var r *Recorder
	r.Record(context.Background(), Operation{Objects: []helmconfig.ObjectVerdict{child("c", plain, helmconfig.ChangeCreated, "create")}}, composition())
}

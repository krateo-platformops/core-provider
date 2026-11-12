package auditrec

import (
	"context"
	"errors"
	"testing"

	"github.com/krateo-platformops/audit"
	"github.com/krateo-platformops/audit/audittest"
	"github.com/krateo-platformops/plumbing/kubeutil/objectclient"
	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

var (
	roleGVR = schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}
	roleT   = Target{GVR: roleGVR, Kind: "Role", Namespace: "plain-ns", Name: "fireworksapp-v1-2-0"}
	cause   = audit.CauseRef{Group: "core.krateo.io", Version: "v1alpha1", Resource: "compositiondefinitions",
		Namespace: "krateo-system", Name: "fireworksapp", UID: "00000000-0000-4000-8000-0000000000cd", Generation: 3}
)

func role(uid, rv string, verbs ...interface{}) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "Role",
		"rules": []interface{}{map[string]interface{}{"apiGroups": []interface{}{""}, "resources": []interface{}{"secrets"}, "verbs": verbs}},
	}}
	u.SetNamespace("plain-ns")
	u.SetName("fireworksapp-v1-2-0")
	u.SetUID(types.UID(uid))
	u.SetResourceVersion(rv)
	u.SetGeneration(1)
	return u
}

type harness struct {
	rec    *audittest.Recorder
	reader *sdkmetric.ManualReader
	r      *Recorder
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{rec: audittest.NewRecorder(t), reader: sdkmetric.NewManualReader()}
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(h.reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	e := audit.New(h.rec.Logger(Scope), audit.WithWriter(audit.WriterController),
		audit.WithActor("system:serviceaccount:krateo-system:core-provider", audit.ClassController), audit.WithMeterProvider(mp))
	r, err := NewRecorder(e, mp, func(_ context.Context, ns string) (map[string]string, error) {
		switch ns {
		case "plain-ns":
			return map[string]string{}, nil
		case "preview-ns":
			return map[string]string{previewLabel: previewValue}, nil
		}
		return nil, errors.New("namespaces \"" + ns + "\" not found")
	})
	if err != nil {
		t.Fatal(err)
	}
	h.r = r
	return h
}

func (h *harness) excluded(t *testing.T) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := h.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != MetricExcluded {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				v, _ := dp.Attributes.Value("reason")
				out[v.AsString()] += dp.Value
			}
		}
	}
	return out
}

func val(rec sdklog.Record, key string) (attribute.Value, bool) {
	for _, kv := range audittest.Attributes(rec) {
		if string(kv.Key) == key {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}

func str(rec sdklog.Record, key string) string { v, _ := val(rec, key); return v.Emit() }

var forbidden = apierrors.NewForbidden(schema.GroupResource{Group: "rbac.authorization.k8s.io", Resource: "roles"}, "fireworksapp-v1-2-0", errors.New("escalation"))

// Every row of the mapping: what is recorded, with which verb, method, outcome and status, and what
// is counted instead. Every record is conformant to the controller writer's schema.
func TestApplyMapping(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		res                         objectclient.ApplyResult
		err                         error
		wantRecord                  bool
		verb, method, outcome, code string
		wantExcluded                string
	}{
		{name: "created", res: objectclient.ApplyResult{Op: objectclient.ApplyCreated, After: role("u1", "10", "get")},
			wantRecord: true, verb: "create", method: "POST", outcome: audit.OutcomeSuccess},
		{name: "updated, rules changed", res: objectclient.ApplyResult{Op: objectclient.ApplyUpdated, Before: role("u1", "10", "get"), After: role("u1", "11", "get", "list")},
			wantRecord: true, verb: "update", method: "PUT", outcome: audit.OutcomeSuccess},
		{name: "updated, only the rule order changed (a set)", res: objectclient.ApplyResult{Op: objectclient.ApplyUpdated, Before: role("u1", "10", "get", "list"), After: role("u1", "11", "list", "get")},
			wantExcluded: ExcludedMetadataOnly},
		{name: "updated, a label only", res: objectclient.ApplyResult{Op: objectclient.ApplyUpdated, Before: role("u1", "10", "get"), After: func() *unstructured.Unstructured {
			u := role("u1", "11", "get")
			u.SetLabels(map[string]string{"x": "y"})
			return u
		}()}, wantExcluded: ExcludedMetadataOnly},
		{name: "updated, content not comparable (no Before): recorded, as the cdc records compareFailed", res: objectclient.ApplyResult{Op: objectclient.ApplyUpdated, After: role("u1", "11", "get")},
			wantRecord: true, verb: "update", method: "PUT", outcome: audit.OutcomeSuccess},
		{name: "unchanged", res: objectclient.ApplyResult{Op: objectclient.ApplyUnchanged, Before: role("u1", "10", "get"), After: role("u1", "10", "get")},
			wantExcluded: ExcludedUnchanged},
		{name: "a refused create", res: objectclient.ApplyResult{Op: objectclient.ApplyCreated}, err: forbidden,
			wantRecord: true, verb: "create", method: "POST", outcome: audit.OutcomeFailure, code: "403"},
		{name: "a refused update", res: objectclient.ApplyResult{Op: objectclient.ApplyUpdated, Before: role("u1", "10", "get")}, err: forbidden,
			wantRecord: true, verb: "update", method: "PUT", outcome: audit.OutcomeFailure, code: "403"},
		{name: "no write sent (the read failed)", res: objectclient.ApplyResult{}, err: errors.New("etcd down")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.r.Apply(context.Background(), ActionRBACApply, roleT, tc.res, tc.err, cause)
			recs := h.rec.Records()
			if !tc.wantRecord {
				if len(recs) != 0 {
					t.Fatalf("want no record, got %d", len(recs))
				}
				if got := h.excluded(t); tc.wantExcluded != "" && got[tc.wantExcluded] != 1 {
					t.Errorf("excluded = %v, want %s=1", got, tc.wantExcluded)
				}
				return
			}
			if len(recs) != 1 {
				t.Fatalf("records = %d, want 1", len(recs))
			}
			r := recs[0]
			if str(r, "krateo.audit.verb") != tc.verb || str(r, "http.request.method") != tc.method || str(r, "krateo.audit.outcome") != tc.outcome {
				t.Errorf("verb %q method %q outcome %q, want %q %q %q", str(r, "krateo.audit.verb"), str(r, "http.request.method"), str(r, "krateo.audit.outcome"), tc.verb, tc.method, tc.outcome)
			}
			if c, ok := val(r, "http.response.status_code"); tc.code != "" && (!ok || c.Emit() != tc.code) {
				t.Errorf("status code %v (present %v), want %s", c.Emit(), ok, tc.code)
			}
			if str(r, "krateo.action") != ActionRBACApply || str(r, "krateo.audit.cause_ref.uid") != cause.UID || str(r, "krateo.audit.object_ref.name") != roleT.Name {
				t.Errorf("action/cause/object wrong: %s %s %s", str(r, "krateo.action"), str(r, "krateo.audit.cause_ref.uid"), str(r, "krateo.audit.object_ref.name"))
			}
			if r.Body().AsString() != "" {
				t.Errorf("the body is a closed set, empty here, got %q", r.Body().AsString())
			}
			audittest.AssertConformant(t, audit.WriterController, r)
			audittest.AssertRequiredKeysDetected(t, audit.WriterController, r)
		})
	}
}

// snowplow (2026-11-12): a migration that only REMOVES a spec field (the #188 pruning case) changed
// content, so it is recorded.
func TestARemovedSpecFieldIsAContentChange(t *testing.T) {
	comp := func(spec map[string]interface{}) *unstructured.Unstructured {
		u := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "composition.krateo.io/v0-3-48", "kind": "Keystone", "spec": spec}}
		u.SetNamespace("plain-ns")
		u.SetName("keystone")
		u.SetUID("u2")
		return u
	}
	h := newHarness(t)
	h.r.Apply(context.Background(), ActionCompositionMigrate,
		Target{GVR: schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v0-3-48", Resource: "keystones"}, Kind: "Keystone", Namespace: "plain-ns", Name: "keystone"},
		objectclient.ApplyResult{Op: objectclient.ApplyUpdated,
			Before: comp(map[string]interface{}{"replicas": int64(1), "oidc": map[string]interface{}{"caBundleRef": "ca"}}),
			After:  comp(map[string]interface{}{"replicas": int64(1)})},
		nil, cause)
	if n := len(h.rec.Records()); n != 1 {
		t.Fatalf("a pruning rewrite must be recorded, got %d records (excluded %v)", n, h.excluded(t))
	}
}

func TestDeleteMapping(t *testing.T) {
	for _, tc := range []struct {
		name    string
		res     objectclient.UninstallResult
		err     error
		want    int
		outcome string
	}{
		{"deleted", objectclient.UninstallResult{Op: objectclient.UninstallDeleted, Before: role("u1", "10", "get")}, nil, 1, audit.OutcomeSuccess},
		{"a refused delete", objectclient.UninstallResult{Op: objectclient.UninstallDeleted, Before: role("u1", "10", "get")}, forbidden, 1, audit.OutcomeFailure},
		{"absent: nothing to delete", objectclient.UninstallResult{Op: objectclient.UninstallAbsent}, nil, 0, ""},
		{"no delete sent (the read failed)", objectclient.UninstallResult{}, errors.New("etcd down"), 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.r.Delete(context.Background(), ActionRBACDelete, roleT, tc.res, tc.err, cause)
			recs := h.rec.Records()
			if len(recs) != tc.want {
				t.Fatalf("records = %d, want %d", len(recs), tc.want)
			}
			if tc.want == 0 {
				return
			}
			r := recs[0]
			if str(r, "krateo.audit.verb") != "delete" || str(r, "http.request.method") != "DELETE" || str(r, "krateo.audit.outcome") != tc.outcome {
				t.Errorf("verb %q method %q outcome %q", str(r, "krateo.audit.verb"), str(r, "http.request.method"), str(r, "krateo.audit.outcome"))
			}
			if str(r, "krateo.audit.object_ref.uid") != "u1" {
				t.Errorf("object_ref.uid = %q, want the deleted object's", str(r, "krateo.audit.object_ref.uid"))
			}
			audittest.AssertConformant(t, audit.WriterController, r)
		})
	}
}

// Preview: false for a cluster-scoped object, from the Namespace label otherwise, and omitted and
// marked when the Namespace can't be read.
func TestPreview(t *testing.T) {
	created := objectclient.ApplyResult{Op: objectclient.ApplyCreated, After: role("u1", "10", "get")}
	for _, tc := range []struct {
		ns      string
		present bool
		want    bool
	}{{"", true, false}, {"plain-ns", true, false}, {"preview-ns", true, true}, {"gone-ns", false, false}} {
		h := newHarness(t)
		tg := roleT
		tg.Namespace = tc.ns
		h.r.Apply(context.Background(), ActionRBACApply, tg, created, nil, cause)
		r := h.rec.Only(t)
		v, ok := val(r, "krateo.audit.preview")
		if ok != tc.present || (ok && v.AsBool() != tc.want) {
			t.Errorf("ns %q: preview %v present %v, want %v present %v", tc.ns, v.AsBool(), ok, tc.want, tc.present)
		}
		if !tc.present {
			if m := str(r, "krateo.audit.incomplete"); m != "preview" {
				t.Errorf("ns %q: incomplete = %q, want preview", tc.ns, m)
			}
		}
	}
}

func TestNilRecorderIsANoop(t *testing.T) {
	var r *Recorder
	r.Apply(context.Background(), ActionRBACApply, roleT, objectclient.ApplyResult{Op: objectclient.ApplyCreated}, nil, cause)
	r.Delete(context.Background(), ActionRBACDelete, roleT, objectclient.UninstallResult{Op: objectclient.UninstallDeleted}, nil, cause)
}

// RecordUpdate, for sites that send their own Update. A migration whose read (through the narrower
// target version) already lacks the fields its write discards shows only a label change between the
// objects it holds; its fieldloss report says content changed, so knownChanged records it. Control:
// the same write without knownChanged is excluded as metadata-only.
func TestRecordUpdate(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v0-3-48", Resource: "keystones"}
	tg := Target{GVR: gvr, Kind: "Keystone", Namespace: "plain-ns", Name: "keystone"}
	comp := func(rv, version string) *unstructured.Unstructured {
		u := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "composition.krateo.io/v0-3-48", "kind": "Keystone",
			"spec": map[string]interface{}{"replicas": int64(1)}}}
		u.SetNamespace("plain-ns")
		u.SetName("keystone")
		u.SetUID("u3")
		u.SetResourceVersion(rv)
		u.SetLabels(map[string]string{"krateo.io/composition-version": version})
		return u
	}
	for _, tc := range []struct {
		name          string
		before, after *unstructured.Unstructured
		err           error
		known         bool
		wantRecords   int
		wantOutcome   string
		wantExcluded  string
	}{
		{"a pruning rewrite: label-only objects, but the report says fields were discarded", comp("10", "v0-3-47"), comp("11", "v0-3-48"), nil, true, 1, audit.OutcomeSuccess, ""},
		{"control: the same write, no report: metadata only", comp("10", "v0-3-47"), comp("11", "v0-3-48"), nil, false, 0, "", ExcludedMetadataOnly},
		{"resourceVersion unmoved: unchanged", comp("10", "v0-3-48"), comp("10", "v0-3-48"), nil, false, 0, "", ExcludedUnchanged},
		{"refused", comp("10", "v0-3-47"), nil, forbidden, false, 1, audit.OutcomeFailure, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			ctx := WithScope(context.Background(), h.r, cause)
			RecordUpdate(ctx, ActionCompositionMigrate, tg, tc.before, tc.after, tc.err, tc.known)
			recs := h.rec.Records()
			if len(recs) != tc.wantRecords {
				t.Fatalf("records = %d, want %d (excluded %v)", len(recs), tc.wantRecords, h.excluded(t))
			}
			if tc.wantRecords == 1 {
				if got := str(recs[0], "krateo.audit.outcome"); got != tc.wantOutcome {
					t.Errorf("outcome %q, want %q", got, tc.wantOutcome)
				}
				if str(recs[0], "krateo.action") != ActionCompositionMigrate || str(recs[0], "krateo.audit.verb") != "update" {
					t.Errorf("action %q verb %q", str(recs[0], "krateo.action"), str(recs[0], "krateo.audit.verb"))
				}
				audittest.AssertConformant(t, audit.WriterController, recs[0])
			}
			if tc.wantExcluded != "" && h.excluded(t)[tc.wantExcluded] != 1 {
				t.Errorf("excluded = %v, want %s=1", h.excluded(t), tc.wantExcluded)
			}
		})
	}
}

// No scope on the context: a write outside a reconcile that set one records nothing.
func TestNoScopeRecordsNothing(t *testing.T) {
	h := newHarness(t)
	RecordApply(context.Background(), ActionRBACApply, roleT, objectclient.ApplyResult{Op: objectclient.ApplyCreated, After: role("u1", "1", "get")}, nil)
	if n := len(h.rec.Records()); n != 0 {
		t.Errorf("records = %d without a scope, want 0", n)
	}
	// Control: the same call under a scope records.
	RecordApply(WithScope(context.Background(), h.r, cause), ActionRBACApply, roleT, objectclient.ApplyResult{Op: objectclient.ApplyCreated, After: role("u1", "1", "get")}, nil)
	if n := len(h.rec.Records()); n != 1 {
		t.Errorf("control: records = %d under a scope, want 1", n)
	}
}

// A Delete a site sent by name: accepted is a record, refused is a failure, NotFound is someone
// else's (no record).
func TestRecordDeleteSent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		want    int
		outcome string
	}{
		{"accepted", nil, 1, audit.OutcomeSuccess},
		{"refused", forbidden, 1, audit.OutcomeFailure},
		{"already gone", apierrors.NewNotFound(schema.GroupResource{Group: "rbac.authorization.k8s.io", Resource: "roles"}, "r"), 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			RecordDeleteSent(WithScope(context.Background(), h.r, cause), ActionRBACDelete, roleT, "u9", tc.err)
			recs := h.rec.Records()
			if len(recs) != tc.want {
				t.Fatalf("records = %d, want %d", len(recs), tc.want)
			}
			if tc.want == 1 {
				if str(recs[0], "krateo.audit.outcome") != tc.outcome || str(recs[0], "krateo.audit.object_ref.uid") != "u9" {
					t.Errorf("outcome %q uid %q", str(recs[0], "krateo.audit.outcome"), str(recs[0], "krateo.audit.object_ref.uid"))
				}
				audittest.AssertConformant(t, audit.WriterController, recs[0])
			}
		})
	}
}

// snowplow (2026-11-12): a dry run is not a write. A dry-run apply (created or updated, changed or
// not, refused) and a dry-run uninstall each record nothing, by their op alone: there is no caller
// flag any more.
func TestDryRunRecordsNothing(t *testing.T) {
	h := newHarness(t)
	ctx := WithScope(context.Background(), h.r, cause)
	for _, res := range []objectclient.ApplyResult{
		{Op: objectclient.ApplyDryRun, After: role("u1", "1", "get")},
		{Op: objectclient.ApplyDryRun, Before: role("u1", "10", "get"), After: role("u1", "10", "get", "list")},
	} {
		RecordApply(ctx, ActionRBACApply, roleT, res, nil)
	}
	RecordApply(ctx, ActionRBACApply, roleT, objectclient.ApplyResult{Op: objectclient.ApplyDryRun}, forbidden)
	RecordDelete(ctx, ActionRBACDelete, roleT, objectclient.UninstallResult{Op: objectclient.UninstallDryRun, Before: role("u1", "10", "get")}, nil)
	if n := len(h.rec.Records()); n != 0 {
		t.Fatalf("dry runs recorded %d rows, want 0", n)
	}
	// Control: the same apply as a real update records.
	RecordApply(ctx, ActionRBACApply, roleT, objectclient.ApplyResult{Op: objectclient.ApplyUpdated, Before: role("u1", "10", "get"), After: role("u1", "11", "get", "list")}, nil)
	if n := len(h.rec.Records()); n != 1 {
		t.Fatalf("control: a real update recorded %d rows, want 1", n)
	}
}

// audit v0.13.0: an update that changes only an authorization-bearing label changes content, so it
// is recorded; one that changes only an undeclared label is metadata.
func TestAuthorizationLabelIsContent(t *testing.T) {
	labelled := func(rv string, l map[string]string) *unstructured.Unstructured {
		u := role("u1", rv, "get")
		u.SetLabels(l)
		return u
	}
	for _, tc := range []struct {
		name          string
		before, after map[string]string
		want          int
	}{
		{"a declared label changed", map[string]string{"authn.krateo.io/principal": "alice"}, map[string]string{"authn.krateo.io/principal": "bob"}, 1},
		{"an undeclared label changed (control)", map[string]string{"app.kubernetes.io/part-of": "a"}, map[string]string{"app.kubernetes.io/part-of": "b"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.r.Apply(context.Background(), ActionRBACApply, roleT,
				objectclient.ApplyResult{Op: objectclient.ApplyUpdated, Before: labelled("10", tc.before), After: labelled("11", tc.after)}, nil, cause)
			if n := len(h.rec.Records()); n != tc.want {
				t.Errorf("records = %d, want %d", n, tc.want)
			}
		})
	}
}

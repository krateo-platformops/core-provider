//go:build integration
// +build integration

package composition

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/gobuffalo/flect"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/e2e-framework/klient/decoder"

	"github.com/krateo-platformops/audit"
	"github.com/krateo-platformops/audit/audittest"
	"github.com/krateo-platformops/composition-dynamic-controller/internal/auditrec"
	"go.opentelemetry.io/otel/attribute"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

// core-provider#423, through the REAL handler on kind: each helm write the cdc makes for a
// composition is one controller audit record per child, with that composition as cause_ref, and a
// reconcile that writes nothing records nothing. The records go to an in-memory Logs SDK
// (audittest), and every one is checked against the controller writer's schema.

// newTestAudit is the cdc's Recorder writing to sink, reading preview from rc's cluster.
func newTestAudit(t *testing.T, sink *audittest.Recorder, rc *rest.Config, resource string) *auditrec.Recorder {
	t.Helper()
	dyn := dynamic.NewForConfigOrDie(rc)
	e := audit.New(sink.Logger(auditrec.Scope), audit.WithWriter(audit.WriterController),
		audit.WithActor(auditrec.Actor(altNamespace, "test-sa"), audit.ClassController))
	r, err := auditrec.NewRecorder(resource, e, metricnoop.NewMeterProvider(), func(ctx context.Context, name string) (map[string]string, error) {
		ns, err := dyn.Resource(schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		return ns.GetLabels(), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// auditRow is one record, its object_ref in full: two children may share a name across kinds and
// namespaces (the focus chart's DataPresentationAzure and FocusConfig do).
type auditRow struct {
	action, verb, outcome                     string
	group, version, resource, namespace, name string
	uid                                       string
}

func (r auditRow) key() string {
	return r.group + "/" + r.version + "/" + r.resource + "/" + r.namespace + "/" + r.name
}

func recAttr(rec sdklog.Record, key string) string {
	for _, kv := range audittest.Attributes(rec) {
		if string(kv.Key) == key {
			if kv.Value.Type() == attribute.INT64 {
				return ""
			}
			return kv.Value.AsString()
		}
	}
	return ""
}

// auditRowsFor is the records whose cause_ref is the composition uid, each checked conformant.
func auditRowsFor(t *testing.T, sink *audittest.Recorder, uid types.UID) []auditRow {
	t.Helper()
	var out []auditRow
	for _, rec := range sink.Records() {
		if recAttr(rec, "krateo.audit.cause_ref.uid") != string(uid) {
			continue
		}
		audittest.AssertConformant(t, audit.WriterController, rec)
		if rec.Body().AsString() != "" {
			t.Errorf("an audit record's body must be empty, got %q", rec.Body().AsString())
		}
		out = append(out, auditRow{
			action:    recAttr(rec, "krateo.action"),
			verb:      recAttr(rec, "krateo.audit.verb"),
			outcome:   recAttr(rec, "krateo.audit.outcome"),
			group:     recAttr(rec, "krateo.audit.object_ref.api_group"),
			version:   recAttr(rec, "krateo.audit.object_ref.api_version"),
			resource:  recAttr(rec, "krateo.audit.object_ref.resource"),
			namespace: recAttr(rec, "k8s.namespace.name"),
			name:      recAttr(rec, "krateo.audit.object_ref.name"),
			uid:       recAttr(rec, "krateo.audit.object_ref.uid"),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key()+out[i].verb < out[j].key()+out[j].verb })
	return out
}

func rowNames(rows []auditRow) string {
	var s []string
	for _, r := range rows {
		s = append(s, r.action+" "+r.verb+" "+r.outcome+" "+r.key())
	}
	return strings.Join(s, ", ")
}

// focusResource is the plural of the focus composition's kind, as the cdc's -resource flag names it.
func focusResource(t *testing.T) string {
	t.Helper()
	var obj unstructured.Unstructured
	if err := decoder.DecodeFile(os.DirFS(filepath.Join(testdataPath, "compositions")), "focus.yaml", &obj); err != nil {
		t.Fatal(err)
	}
	return flect.Pluralize(strings.ToLower(obj.GetKind()))
}

// assertRowsMatchCluster checks every successful row against the cluster, an independent source:
// a create or patch names an object that exists with the recorded uid; a delete names one that is
// gone. A record of a write that did not happen, or of the wrong object, fails here.
func assertRowsMatchCluster(ctx context.Context, t *testing.T, rc *rest.Config, rows []auditRow) {
	t.Helper()
	dyn := dynamic.NewForConfigOrDie(rc)
	for _, r := range rows {
		if r.outcome != audit.OutcomeSuccess {
			continue
		}
		gvr := schema.GroupVersionResource{Group: r.group, Version: r.version, Resource: r.resource}
		var ri dynamic.ResourceInterface = dyn.Resource(gvr)
		if r.namespace != "" {
			ri = dyn.Resource(gvr).Namespace(r.namespace)
		}
		live, err := ri.Get(ctx, r.name, metav1.GetOptions{})
		switch r.verb {
		case "delete":
			if err == nil && live.GetDeletionTimestamp() == nil {
				t.Errorf("#423: row %s says deleted, but the object is live (uid %s)", r.key(), live.GetUID())
			}
		default:
			if err != nil {
				t.Errorf("#423: row %s %s, but the object cannot be read: %v", r.verb, r.key(), err)
			} else if string(live.GetUID()) != r.uid {
				t.Errorf("#423: row %s %s records uid %q, the live object is %q", r.verb, r.key(), r.uid, live.GetUID())
			}
		}
	}
}

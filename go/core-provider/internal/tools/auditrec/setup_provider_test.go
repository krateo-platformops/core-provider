package auditrec

import (
	"context"
	"testing"

	"github.com/krateo-platformops/audit"
	"github.com/krateo-platformops/audit/audittest"
	"github.com/krateo-platformops/plumbing/kubeutil/objectclient"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/embedded"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

// sinkProvider is a LoggerProvider whose loggers write to an audittest sink.
type sinkProvider struct {
	embedded.LoggerProvider
	sink *audittest.Recorder
}

func (p sinkProvider) Logger(name string, _ ...otellog.LoggerOption) otellog.Logger {
	return p.sink.Logger(name)
}

// OTEL_AUDIT_LOGS_ENABLED (Diego, 2026-11-14): New sends the records to the audit-only provider it is
// given, not to the global one. Without this, the dedicated provider could be built and shut down
// while every record still went to the global (no-op) provider and nothing would fail.
func TestNewEmitsThroughTheGivenProvider(t *testing.T) {
	sink := audittest.NewRecorder(t)
	r, _, err := New(&rest.Config{Host: "http://127.0.0.1:1"}, sinkProvider{sink: sink})
	if err != nil {
		t.Fatal(err)
	}
	after := &unstructured.Unstructured{}
	after.SetName("fireworksapps-v1-0-0")
	after.SetUID("00000000-0000-4000-8000-0000000000aa")
	// A cluster-scoped target: preview is false without reading a Namespace.
	r.Apply(context.Background(), ActionRBACApply,
		Target{GVR: schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}, Kind: "ClusterRole", Name: "fireworksapps-v1-0-0"},
		objectclient.ApplyResult{Op: objectclient.ApplyCreated, After: after}, nil,
		audit.CauseRef{Group: "core.krateo.io", Version: "v1alpha1", Resource: "compositiondefinitions", Namespace: "ns", Name: "fireworksapp", UID: "00000000-0000-4000-8000-0000000000cd", Generation: 1})
	if n := len(sink.Records()); n != 1 {
		t.Fatalf("the given provider received %d records, want the 1 audit record", n)
	}
}

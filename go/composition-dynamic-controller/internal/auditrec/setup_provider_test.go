package auditrec

import (
	"context"
	"testing"

	"github.com/krateo-platformops/audit/audittest"
	helmconfig "github.com/krateo-platformops/plumbing/helm"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/embedded"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
	r, err := New(&rest.Config{Host: "http://127.0.0.1:1"}, "focusdatapresentationazures", "krateo-system", "test-sa", sinkProvider{sink: sink})
	if err != nil {
		t.Fatal(err)
	}
	mg := &unstructured.Unstructured{}
	mg.SetAPIVersion("composition.krateo.io/v0-1-0")
	mg.SetKind("FocusDataPresentationAzure")
	mg.SetNamespace("demo-system")
	mg.SetName("focus")
	mg.SetUID("00000000-0000-4000-8000-0000000000cd")
	mg.SetGeneration(1)
	// A cluster-scoped child: preview is false without reading a Namespace.
	r.Record(context.Background(), Operation{
		Action: ActionInstall, Complete: true, ReleaseChildren: -1,
		Objects: []helmconfig.ObjectVerdict{{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles", Kind: "ClusterRole",
			Name: "child", UID: "00000000-0000-4000-8000-0000000000aa", Verb: "create", Change: helmconfig.ChangeCreated}},
	}, mg)
	if n := len(sink.Records()); n != 1 {
		t.Fatalf("the given provider received %d records, want the 1 audit record", n)
	}
}

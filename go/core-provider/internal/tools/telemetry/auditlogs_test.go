package telemetry

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/krateo-platformops/audit"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	logglobal "go.opentelemetry.io/otel/log/global"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	"google.golang.org/protobuf/proto"
)

// The audit-only export, end to end against an OTLP/HTTP receiver configured through the standard
// OTEL_EXPORTER_OTLP_ENDPOINT, the same environment the trace and metric exporters read (so it goes
// wherever they go). With OTEL_AUDIT_LOGS_ENABLED on and OTEL_LOGS_ENABLED off:
//   - an audit record reaches the receiver, with EventName "audit" and the event.name attribute;
//   - an ordinary log line, written through the same logging.NewOTelHandler the processes use, does
//     NOT: the dedicated provider is never installed globally, so the slog bridge cannot see it;
//   - the global provider is left as it was.
func TestAuditLoggerProviderExportsOnlyAuditRecords(t *testing.T) {
	var mu sync.Mutex
	var records []*collogspb.ExportLogsServiceRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body io.Reader = r.Body
		if r.Header.Get("Content-Encoding") == "gzip" {
			zr, err := gzip.NewReader(r.Body)
			if err != nil {
				t.Errorf("gzip: %v", err)
				return
			}
			body = zr
		}
		raw, _ := io.ReadAll(body)
		req := &collogspb.ExportLogsServiceRequest{}
		if err := proto.Unmarshal(raw, req); err != nil {
			t.Errorf("decoding the OTLP request: %v", err)
		}
		mu.Lock()
		records = append(records, req)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", srv.URL)
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/protobuf")

	globalBefore := logglobal.GetLoggerProvider()
	lp, shutdown, err := NewAuditLoggerProvider(context.Background(), true, "core-provider")
	if err != nil {
		t.Fatal(err)
	}
	if lp == nil {
		t.Fatal("enabled returned no provider")
	}
	if logglobal.GetLoggerProvider() != globalBefore {
		t.Fatal("the audit provider was installed as the global LoggerProvider: the slog stream would reach it")
	}

	// The process's ordinary log stream, as main builds it.
	app := slog.New(logging.NewOTelHandler(slog.LevelInfo, io.Discard, "core-provider"))
	app.Info("an ordinary log line that must stay on stderr")

	e := audit.New(lp.Logger("github.com/krateo-platformops/core-provider/audit"), audit.WithWriter(audit.WriterController))
	e.Emit(context.Background(), audit.Event{Action: "rbac.apply", Verb: "create", HTTPMethod: "POST", Outcome: audit.OutcomeSuccess,
		Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles", Kind: "Role", Namespace: "ns", ObjectName: "r",
		Preview: audit.PreviewFalse})
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("flushing: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	var bodies, eventNames, attrNames []string
	n := 0
	for _, req := range records {
		for _, rl := range req.GetResourceLogs() {
			for _, sl := range rl.GetScopeLogs() {
				for _, lr := range sl.GetLogRecords() {
					n++
					bodies = append(bodies, lr.GetBody().GetStringValue())
					eventNames = append(eventNames, lr.GetEventName())
					for _, kv := range lr.GetAttributes() {
						if kv.GetKey() == "event.name" {
							attrNames = append(attrNames, kv.GetValue().GetStringValue())
						}
					}
				}
			}
		}
	}
	if n != 1 {
		t.Fatalf("the receiver got %d log records (bodies %q), want exactly the one audit record", n, bodies)
	}
	if eventNames[0] != audit.EventName || len(attrNames) != 1 || attrNames[0] != audit.EventName {
		t.Errorf("the audit record carries EventName %q and event.name attributes %q, want both %q", eventNames[0], attrNames, audit.EventName)
	}
	for _, b := range bodies {
		if bytes.Contains([]byte(b), []byte("ordinary log line")) {
			t.Errorf("a plain slog record reached the audit-only OTLP export")
		}
	}
}

// Disabled is no provider at all: the recorder falls back to the global one (today's behaviour).
func TestAuditLoggerProviderDisabledIsNil(t *testing.T) {
	lp, shutdown, err := NewAuditLoggerProvider(context.Background(), false, "core-provider")
	if err != nil || lp != nil || shutdown(context.Background()) != nil {
		t.Fatalf("disabled = (%v, err %v), want (nil, nil) and a no-op shutdown", lp, err)
	}
}

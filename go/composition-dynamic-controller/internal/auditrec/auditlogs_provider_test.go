package auditrec

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	logglobal "go.opentelemetry.io/otel/log/global"
)

// The cdc's audit-only provider is never installed as the global LoggerProvider (the slog bridge the
// application log stream uses captures the global one, so installing it would export the whole stream),
// and disabled it is nil, so the recorder keeps today's global provider.
func TestAuditLoggerProviderIsNotGlobal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer srv.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", srv.URL)

	before := logglobal.GetLoggerProvider()
	lp, shutdown, err := NewAuditLoggerProvider(context.Background(), true, "composition-dynamic-controller")
	if err != nil || lp == nil {
		t.Fatalf("enabled = (%v, err %v), want a provider", lp, err)
	}
	defer func() { _ = shutdown(context.Background()) }()
	if logglobal.GetLoggerProvider() != before {
		t.Fatal("the audit provider was installed as the global LoggerProvider: the log stream would reach it")
	}

	off, offShutdown, err := NewAuditLoggerProvider(context.Background(), false, "composition-dynamic-controller")
	if err != nil || off != nil || offShutdown(context.Background()) != nil {
		t.Fatalf("disabled = (%v, err %v), want (nil, nil) and a no-op shutdown", off, err)
	}
}

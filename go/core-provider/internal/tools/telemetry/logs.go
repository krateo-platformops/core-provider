package telemetry

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	otellog "go.opentelemetry.io/otel/log"
	logglobal "go.opentelemetry.io/otel/log/global"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
)

// SetupOTLPLogs installs a global OTel LoggerProvider backed by an OTLP/HTTP log exporter, so log
// records emitted through provider-runtime's logging.NewOTelHandler are exported over OTLP alongside
// traces and metrics. Endpoint/headers come from the standard OTEL_EXPORTER_OTLP_* environment (the
// same as the trace exporter). It MUST be called before the log handler is built (the otelslog
// bridge captures the installed provider at construction). When enabled is false it is a no-op
// returning a nil-op shutdown, so callers can wire it unconditionally and gate on their OTEL flag.
func SetupOTLPLogs(ctx context.Context, enabled bool, serviceName string) (func(context.Context) error, error) {
	if !enabled {
		return func(context.Context) error { return nil }, nil
	}
	exporter, err := otlploghttp.New(ctx)
	if err != nil {
		return nil, err
	}
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(
		attribute.String("service.name", serviceName),
	))
	if err != nil {
		res = resource.Default()
	}
	lp := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewBatchProcessor(exporter)),
		sdklog.WithResource(res),
	)
	logglobal.SetLoggerProvider(lp)
	return lp.Shutdown, nil
}

// NewAuditLoggerProvider is the OTLP LoggerProvider for controller audit records ONLY (Diego,
// 2026-11-14): OTEL_AUDIT_LOGS_ENABLED. It is NOT installed as the global provider, so the
// application's slog stream (logging.NewOTelHandler's otelslog bridge, which captures the global one)
// never reaches it and stays on stderr. The exporter is the same OTLP/HTTP one, from the standard
// OTEL_EXPORTER_OTLP_* environment, as traces, metrics and OTEL_LOGS_ENABLED logs. When enabled is
// false it returns a nil provider and a no-op shutdown: the audit recorder then uses the global
// provider, which is today's behaviour (OTLP when OTEL_LOGS_ENABLED, a no-op otherwise).
func NewAuditLoggerProvider(ctx context.Context, enabled bool, serviceName string) (otellog.LoggerProvider, func(context.Context) error, error) {
	if !enabled {
		return nil, func(context.Context) error { return nil }, nil
	}
	exporter, err := otlploghttp.New(ctx)
	if err != nil {
		return nil, nil, err
	}
	lp := newLoggerProvider(sdklog.NewBatchProcessor(exporter), serviceName)
	return lp, lp.Shutdown, nil
}

// newLoggerProvider is a LoggerProvider over processor with the service.name resource; the seam the
// audit-only test feeds an in-memory processor through.
func newLoggerProvider(processor sdklog.Processor, serviceName string) *sdklog.LoggerProvider {
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(
		attribute.String("service.name", serviceName),
	))
	if err != nil {
		res = resource.Default()
	}
	return sdklog.NewLoggerProvider(sdklog.WithProcessor(processor), sdklog.WithResource(res))
}

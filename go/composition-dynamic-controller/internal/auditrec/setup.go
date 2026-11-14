package auditrec

import (
	"context"

	"github.com/krateo-platformops/audit"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	otellog "go.opentelemetry.io/otel/log"
	logglobal "go.opentelemetry.io/otel/log/global"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

var namespacesGVR = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}

// Actor is the controller's principal, system:serviceaccount:<namespace>:<name>, or "" when either
// is unset: an unknown actor is left for the emitter to mark, never guessed.
func Actor(saNamespace, saName string) string {
	// audit's own rule (v0.12.0), so the rule lives in one place: "" when either is unset or not a
	// valid Kubernetes name.
	return audit.ServiceAccountActor(saNamespace, saName)
}

// New builds the cdc's Recorder for the compositions of resource (the plural): records go to the
// process LoggerProvider under Scope (OTLP when OTEL_LOGS_ENABLED; a no-op provider otherwise), the
// counters to the process MeterProvider, and preview is read from the Namespaces of the cluster cfg
// points at, the one helm writes to.
//
// auditLogs is the audit-only LoggerProvider (NewAuditLoggerProvider), nil to use the process's global
// one.
func New(cfg *rest.Config, resource, saNamespace, saName string, auditLogs otellog.LoggerProvider) (*Recorder, error) {
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	opts := []audit.Option{audit.WithWriter(audit.WriterController)}
	if a := Actor(saNamespace, saName); a != "" {
		opts = append(opts, audit.WithActor(a, audit.ClassController))
	}
	lp := auditLogs
	if lp == nil {
		lp = logglobal.GetLoggerProvider()
	}
	e := audit.New(lp.Logger(Scope), opts...)
	return NewRecorder(resource, e, otel.GetMeterProvider(), func(ctx context.Context, name string) (map[string]string, error) {
		ns, err := dyn.Resource(namespacesGVR).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		return ns.GetLabels(), nil
	})
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

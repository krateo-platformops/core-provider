package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/go-logr/logr"

	"github.com/krateo-platformops/core-provider/internal/controllers/compositiondefinitions"
	"github.com/krateo-platformops/core-provider/internal/controllers/compositionmirror"
	"github.com/krateo-platformops/core-provider/internal/controllers/kubernetestargets"
	"github.com/krateo-platformops/core-provider/internal/tools/pluralizer"
	"github.com/krateo-platformops/plumbing/env"
	"github.com/krateo-platformops/plumbing/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/krateo-platformops/core-provider/apis"
	"github.com/krateo-platformops/provider-runtime/pkg/controller"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	"github.com/krateo-platformops/provider-runtime/pkg/ratelimiter"
	"github.com/krateo-platformops/provider-runtime/pkg/telemetry"

	"github.com/krateo-platformops/core-provider/internal/tools/health"
	coretelemetry "github.com/krateo-platformops/core-provider/internal/tools/telemetry"
	"github.com/stoewer/go-strcase"
)

const (
	providerName              = "Core"
	defaultOtelExportInterval = 30 * time.Second
)

// Build stamp, injected at image build time with -ldflags -X (see this module's Dockerfile and the
// build_args in .github/workflows/release-{tag,pullrequest}.yaml). Without it an incident can only
// be traced to a release version, never to a commit (#105).
//
// The defaults are deliberately non-empty so an unstamped build says so out loud instead of
// reporting a convincing-looking blank.
var (
	buildVersion = "dev"
	buildCommit  = "unknown"
)

// main exits with run's code. run returns rather than calling os.Exit, so its deferred telemetry
// and log shutdowns always run and flush buffered spans, metrics and log records; os.Exit inside it
// skipped every defer (provider-runtime's review of #434, L3).
func main() {
	os.Exit(run())
}

// shutdownTimeout bounds each telemetry shutdown on exit: a collector that is down must not hold the
// process past its termination grace period.
const shutdownTimeout = 5 * time.Second

func run() int {
	envVarPrefix := fmt.Sprintf("%s_PROVIDER", strcase.UpperSnakeCase(providerName))

	debug := flag.Bool("debug", env.Bool(fmt.Sprintf("%s_DEBUG", envVarPrefix), false), "Run with debug logging.")
	// SyncPeriod is the controller-runtime cache re-list interval. The CD reconcile reads the
	// CompositionDefinition from this informer cache; if a watch event for a spec.chart.version bump
	// is missed/late, the engine keeps reconciling the stale version (regenerating the OLD generated
	// CRD version) and only self-heals at the next full re-list -- so a 1h default meant a live
	// installer/component version upgrade could hang for up to an hour and in practice needed a manual
	// engine restart to force a re-list (D3, "nudges must not exist", 2026-07-08). Default to 10m so a
	// missed watch self-heals within minutes without any manual nudge; the CD population is small
	// (dozens), so the extra re-list load is negligible. Still overridable via KRATEO_CORE_PROVIDER_SYNC.
	syncPeriod := flag.Duration("sync", env.Duration(fmt.Sprintf("%s_SYNC", envVarPrefix), time.Minute*10), "Controller manager cache re-list period such as 300ms, 1.5h, or 2h45m")
	pollInterval := flag.Duration("poll", env.Duration(fmt.Sprintf("%s_POLL_INTERVAL", envVarPrefix), time.Minute*3), "Poll interval controls how often an individual resource should be checked for drift.")
	maxReconcileRate := flag.Int("max-reconcile-rate", env.Int(fmt.Sprintf("%s_MAX_RECONCILE_RATE", envVarPrefix), 5), "The global maximum rate per second at which resources may checked for drift from the desired state.")
	// #414: off = dry run (dangling controller bindings are logged, not deleted).
	removeDanglingRBAC := flag.Bool("remove-dangling-controller-rbac", env.Bool(fmt.Sprintf("%s_REMOVE_DANGLING_CONTROLLER_RBAC", envVarPrefix), false),
		"Delete RoleBindings/ClusterRoleBindings whose only subject is a retired composition controller's absent ServiceAccount (#414). Off: log them only.")
	leaderElection := flag.Bool("leader-election", env.Bool(fmt.Sprintf("%s_LEADER_ELECTION", envVarPrefix), false), "Use leader election for the controller manager.")
	metricsEnabled := flag.Bool("otel-enabled", env.Bool("OTEL_ENABLED", false), "Enable OTLP metrics export for provider-runtime telemetry.")
	tracingEnabled := flag.Bool("otel-tracing-enabled", env.Bool("OTEL_TRACING_ENABLED", false), "Enable OTLP trace export (distributed reconcile traces).")
	logsEnabled := flag.Bool("otel-logs-enabled", env.Bool("OTEL_LOGS_ENABLED", false), "Enable OTLP log export (logs as a first-class OTel signal, in addition to the stderr JSON stream).")
	metricsServiceName := flag.String("otel-service-name", fmt.Sprintf("%s-provider", strcase.KebabCase(providerName)), "The service name attached to exported OTLP metrics/traces.")
	metricsExportInterval := flag.Duration("otel-export-interval", env.Duration("OTEL_EXPORT_INTERVAL", defaultOtelExportInterval), "The interval used to export OTLP metrics.")
	maxErrorRetryInterval := flag.Duration("max-error-retry-interval", env.Duration(fmt.Sprintf("%s_MAX_ERROR_RETRY_INTERVAL", envVarPrefix), 1*time.Minute), "The maximum interval between retries when an error occurs. This should be less than the half of the poll interval.")
	minErrorRetryInterval := flag.Duration("min-error-retry-interval", env.Duration(fmt.Sprintf("%s_MIN_ERROR_RETRY_INTERVAL", envVarPrefix), 1*time.Second), "The minimum interval between retries when an error occurs. This should be less than max-error-retry-interval.")

	// The chart's livenessProbe/readinessProbe reach this port by the container port name "health"
	// (helm/core-provider, healthProbe.port), and render it into this env var, so the two move together.
	healthProbePort := flag.Int("health-probe-port", env.Int(fmt.Sprintf("%s_HEALTH_PROBE_PORT", envVarPrefix), 8081),
		"The port serving /healthz (liveness: the process answers) and /readyz (readiness: the manager's informers have completed their first successful list). 0 disables both.")

	flag.Parse()

	log.Default().SetOutput(os.Stderr)

	logLevel := slog.LevelInfo
	if *debug {
		logLevel = slog.LevelDebug
	}

	// Identify ALL OTel signal pipelines (metrics, traces, logs) via the standard OTel env BEFORE any
	// exporter/handler is built. OTEL_SERVICE_NAME / OTEL_RESOURCE_ATTRIBUTES are the one source
	// every pipeline reads (provider-runtime has honoured cfg.ServiceName too since v1.9.1, unless
	// OTEL_SERVICE_NAME is set), so metrics, traces and logs carry the same service.name/version. core-provider is the engine/operator, so it carries no composition-id.
	os.Setenv("OTEL_SERVICE_NAME", *metricsServiceName)
	// service.version prefers the binary's own build stamp over the SERVICE_VERSION the chart injects:
	// the stamp describes the image that is actually running, whereas the env var describes what the
	// chart believes it deployed, and the two diverge whenever a tag is moved or an image is pinned by
	// digest. SERVICE_VERSION remains the fallback for unstamped (local/dev) builds so nothing that
	// reads service.version today loses it. service.build.commit is added alongside rather than folded
	// into service.version, so dashboards keyed on the release version keep working while an incident
	// can still be attributed to an exact commit (#105).
	var attrPairs []string
	if buildVersion != "dev" && buildVersion != "" {
		attrPairs = append(attrPairs, "service.version="+buildVersion)
	} else if sv := os.Getenv("SERVICE_VERSION"); sv != "" {
		attrPairs = append(attrPairs, "service.version="+sv)
	}
	attrPairs = append(attrPairs, "service.build.commit="+buildCommit)
	attrs := strings.Join(attrPairs, ",")
	if existing := os.Getenv("OTEL_RESOURCE_ATTRIBUTES"); existing != "" {
		attrs = existing + "," + attrs
	}
	os.Setenv("OTEL_RESOURCE_ATTRIBUTES", attrs)

	// Install the OTLP LoggerProvider BEFORE building the log handler, so logging.NewOTelHandler's
	// otelslog bridge captures it and log records export over OTLP (gated OTEL_LOGS_ENABLED). No app
	// logger exists yet, so a bootstrap error goes to the stdlib logger.
	logsShutdown, err := coretelemetry.SetupOTLPLogs(context.Background(), *logsEnabled, *metricsServiceName)
	if err != nil {
		log.Default().Printf("cannot initialize OpenTelemetry logs: %v", err)
		return 1
	}

	// Emit logs as one JSON object per line in the OTel log model (RFC3339Nano UTC `timestamp`,
	// SeverityNumber, service.name, trace correlation) for logs-ingester, AND — when OTEL_LOGS_ENABLED
	// — tee them to the OTLP pipeline. Shared handler from provider-runtime; see
	// docs/log-ingester-compatibility.md.
	// slog-backed, not logr-backed: only a slog Logger can carry a context, so logging.WithContext(log,
	// ctx) at the reconcile sites adds trace_id/span_id to their records (provider-runtime v1.13.0;
	// review of #434, L1). The logr-backed form silently ignored WithContext.
	log := logging.NewSlogLogger(*slog.New(logging.NewOTelHandler(logLevel, os.Stderr, *metricsServiceName)))

	// Announce the build stamp first: `kubectl logs` alone should answer "which commit is this pod
	// running", without needing an OTel pipeline or a metrics scrape (#105).
	log.Info("starting core-provider", "version", buildVersion, "commit", buildCommit)

	// controller-runtime logger at INFO (our logger above handles debug).
	ctrl.SetLogger(logr.FromSlogHandler(logging.NewOTelHandler(slog.LevelInfo, os.Stderr, *metricsServiceName)))

	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := logsShutdown(ctx); err != nil {
			log.Error(err, "Cannot shutdown OpenTelemetry logs")
		}
	}()

	log.Debug("Starting",
		"sync-period", syncPeriod.String(),
		"poll-interval", pollInterval.String(),
		"max-reconcile-rate", *maxReconcileRate,
		"leader-election", *leaderElection,
		"max-error-retry-interval", maxErrorRetryInterval.String(),
		"min-error-retry-interval", minErrorRetryInterval.String(),
		"otel-enabled", *metricsEnabled,
		"otel-service-name", *metricsServiceName,
		"otel-export-interval", metricsExportInterval.String())

	telemetryEnabled := *metricsEnabled
	telemetryExportInterval := *metricsExportInterval

	// Metrics AND traces come from provider-runtime's telemetry.Setup since v1.14 (#395, its
	// design-tracing §11.4): one TracerProvider owns the global, and the reconciler's reconcile /
	// observe / create / update / delete spans parent core-provider's own
	// compositiondefinition.observe span (coretelemetry.Tracer() uses the global). Setup installs the
	// W3C propagator on every call, tracing on or off, which core-provider's SetupTracing did before.
	// Tracing exports OTLP/HTTP only: with OTEL_TRACING_ENABLED and protocol grpc, Setup refuses to
	// start rather than send HTTP to a gRPC port.
	telemetryMetrics, telemetryShutdown, err := telemetry.Setup(context.Background(), log, telemetry.Config{
		Enabled:        telemetryEnabled,
		TracingEnabled: *tracingEnabled,
		ServiceName:    *metricsServiceName,
		ExportInterval: telemetryExportInterval,
	})
	if err != nil {
		log.Error(err, "Cannot initialize OpenTelemetry telemetry")
		return 1
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := telemetryShutdown(ctx); err != nil {
			log.Error(err, "Cannot shutdown OpenTelemetry telemetry")
		}
	}()

	cfg, err := ctrl.GetConfig()
	if err != nil {
		log.Error(err, "Cannot get API server rest config")
		return 1
	}

	// "0" is controller-runtime's value for "no probe server".
	healthProbeBindAddress := "0"
	if *healthProbePort != 0 {
		healthProbeBindAddress = fmt.Sprintf(":%d", *healthProbePort)
	}

	// core-provider hosts no admission webhooks (None conversion + an in-apiserver
	// MutatingAdmissionPolicy), so there is no webhook server or serving certificate.
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		HealthProbeBindAddress: healthProbeBindAddress,
		LeaderElection:         *leaderElection,
		LeaderElectionID:       fmt.Sprintf("leader-election-%s-provider", strcase.KebabCase(providerName)),
		// Hand the Lease over on shutdown, after the in-flight reconciles drain, instead of making the
		// next pod of a rolling update wait out the lease duration (#382). Safe because main returns
		// as soon as mgr.Start does: nothing runs after the Lease is released.
		LeaderElectionReleaseOnCancel: true,
		Cache: cache.Options{
			SyncPeriod: syncPeriod,
		},
		Metrics: metricsserver.Options{
			BindAddress: ":8080",
		},
		Controller: config.Controller{
			UsePriorityQueue: ptr.To(true),
		},
	})
	if err != nil {
		log.Error(err, "Cannot create controller manager")
		return 1
	}

	// Probes (#333). Liveness answers from the probe server alone, so a slow or failing reconcile
	// never restarts the engine. Readiness reports whether the engine can SEE what it reconciles:
	// not ready until the manager's informers have completed their first successful list. The chart
	// used to TCP-probe the always-bound metrics port instead, which a process that had never listed
	// anything satisfied.
	if err := health.Register(mgr); err != nil {
		log.Error(err, "Cannot register health probes")
		return 1
	}

	o := controller.Options{
		Logger:                  log,
		MaxConcurrentReconciles: *maxReconcileRate,
		PollInterval:            *pollInterval,
		GlobalRateLimiter:       ratelimiter.NewGlobalExponential(*minErrorRetryInterval, *maxErrorRetryInterval),
		UsePriorityQueue:        ptr.To(true),
		QueueWaitRecorder:       telemetryMetrics,
	}

	if err := apis.AddToScheme(mgr.GetScheme()); err != nil {
		log.Error(err, "Cannot add APIs to scheme")
		return 1
	}
	if err := compositiondefinitions.Setup(mgr, compositiondefinitions.Options{
		ControllerOptions:  o,
		Metrics:            telemetryMetrics,
		Pluralizer:         pluralizer.New(false),
		RemoveDanglingRBAC: *removeDanglingRBAC,
	}); err != nil {
		log.Error(err, "Cannot setup controllers")
		return 1
	}
	if err := kubernetestargets.Setup(mgr, kubernetestargets.Options{
		ControllerOptions: o,
	}); err != nil {
		log.Error(err, "Cannot setup KubernetesTarget controller")
		return 1
	}
	if err := compositionmirror.Setup(mgr, compositionmirror.Options{
		ControllerOptions: o,
	}); err != nil {
		log.Error(err, "Cannot setup composition mirror controller")
		return 1
	}
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "Cannot start controller manager")
		return 1
	}
	return 0
}

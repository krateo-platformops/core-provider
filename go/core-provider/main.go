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

func main() {
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
	leaderElection := flag.Bool("leader-election", env.Bool(fmt.Sprintf("%s_LEADER_ELECTION", envVarPrefix), false), "Use leader election for the controller manager.")
	metricsEnabled := flag.Bool("otel-enabled", env.Bool("OTEL_ENABLED", false), "Enable OTLP metrics export for provider-runtime telemetry.")
	tracingEnabled := flag.Bool("otel-tracing-enabled", env.Bool("OTEL_TRACING_ENABLED", false), "Enable OTLP trace export (distributed reconcile traces).")
	logsEnabled := flag.Bool("otel-logs-enabled", env.Bool("OTEL_LOGS_ENABLED", false), "Enable OTLP log export (logs as a first-class OTel signal, in addition to the stderr JSON stream).")
	metricsServiceName := flag.String("otel-service-name", fmt.Sprintf("%s-provider", strcase.KebabCase(providerName)), "The service name attached to exported OTLP metrics/traces.")
	metricsExportInterval := flag.Duration("otel-export-interval", env.Duration("OTEL_EXPORT_INTERVAL", defaultOtelExportInterval), "The interval used to export OTLP metrics.")
	maxErrorRetryInterval := flag.Duration("max-error-retry-interval", env.Duration(fmt.Sprintf("%s_MAX_ERROR_RETRY_INTERVAL", envVarPrefix), 1*time.Minute), "The maximum interval between retries when an error occurs. This should be less than the half of the poll interval.")
	minErrorRetryInterval := flag.Duration("min-error-retry-interval", env.Duration(fmt.Sprintf("%s_MIN_ERROR_RETRY_INTERVAL", envVarPrefix), 1*time.Second), "The minimum interval between retries when an error occurs. This should be less than max-error-retry-interval.")

	flag.Parse()

	log.Default().SetOutput(os.Stderr)

	logLevel := slog.LevelInfo
	if *debug {
		logLevel = slog.LevelDebug
	}

	// Identify ALL OTel signal pipelines (metrics, traces, logs) via the standard OTel env BEFORE any
	// exporter/handler is built. provider-runtime builds its metrics resource from resource.Default()
	// and IGNORES cfg.ServiceName, so service.name/version must arrive via OTEL_SERVICE_NAME /
	// OTEL_RESOURCE_ATTRIBUTES; the log + trace resources reuse the same, keeping all three signals
	// identical. core-provider is the engine/operator, so it carries no composition-id.
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
		os.Exit(1)
	}

	// Emit logs as one JSON object per line in the OTel log model (RFC3339Nano UTC `timestamp`,
	// SeverityNumber, service.name, trace correlation) for logs-ingester, AND — when OTEL_LOGS_ENABLED
	// — tee them to the OTLP pipeline. Shared handler from provider-runtime; see
	// docs/log-ingester-compatibility.md.
	log := logging.NewLogrLogger(logr.FromSlogHandler(logging.NewOTelHandler(logLevel, os.Stderr, *metricsServiceName)))

	// Announce the build stamp first: `kubectl logs` alone should answer "which commit is this pod
	// running", without needing an OTel pipeline or a metrics scrape (#105).
	log.Info("starting core-provider", "version", buildVersion, "commit", buildCommit)

	// controller-runtime logger at INFO (our logger above handles debug).
	ctrl.SetLogger(logr.FromSlogHandler(logging.NewOTelHandler(slog.LevelInfo, os.Stderr, *metricsServiceName)))

	defer func() {
		if err := logsShutdown(context.Background()); err != nil {
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

	telemetryMetrics, telemetryShutdown, err := telemetry.Setup(context.Background(), log, telemetry.Config{
		Enabled:        telemetryEnabled,
		ServiceName:    *metricsServiceName,
		ExportInterval: telemetryExportInterval,
	})
	if err != nil {
		log.Error(err, "Cannot initialize OpenTelemetry metrics")
		os.Exit(1)
	}
	defer func() {
		if err := telemetryShutdown(context.Background()); err != nil {
			log.Error(err, "Cannot shutdown OpenTelemetry metrics")
		}
	}()

	// Distributed reconcile traces (gated OTEL_TRACING_ENABLED, default off). Reuses the
	// OTEL_SERVICE_NAME / OTEL_RESOURCE_ATTRIBUTES set above, so the trace + metrics resources
	// are identical (same service.name/version).
	tracingShutdown, err := coretelemetry.SetupTracing(context.Background(), log, *tracingEnabled)
	if err != nil {
		log.Error(err, "Cannot initialize OpenTelemetry tracing")
		os.Exit(1)
	}
	defer func() {
		if err := tracingShutdown(context.Background()); err != nil {
			log.Error(err, "Cannot shutdown OpenTelemetry tracing")
		}
	}()

	cfg, err := ctrl.GetConfig()
	if err != nil {
		log.Error(err, "Cannot get API server rest config")
		os.Exit(1)
	}

	// core-provider hosts no admission webhooks (None conversion + an in-apiserver
	// MutatingAdmissionPolicy), so there is no webhook server or serving certificate.
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		LeaderElection:   *leaderElection,
		LeaderElectionID: fmt.Sprintf("leader-election-%s-provider", strcase.KebabCase(providerName)),
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
		os.Exit(1)
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
		os.Exit(1)
	}
	if err := compositiondefinitions.Setup(mgr, compositiondefinitions.Options{
		ControllerOptions: o,
		Metrics:           telemetryMetrics,
		Pluralizer:        pluralizer.New(false),
	}); err != nil {
		log.Error(err, "Cannot setup controllers")
		os.Exit(1)
	}
	if err := kubernetestargets.Setup(mgr, kubernetestargets.Options{
		ControllerOptions: o,
	}); err != nil {
		log.Error(err, "Cannot setup KubernetesTarget controller")
		os.Exit(1)
	}
	if err := compositionmirror.Setup(mgr, compositionmirror.Options{
		ControllerOptions: o,
	}); err != nil {
		log.Error(err, "Cannot setup composition mirror controller")
		os.Exit(1)
	}
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "Cannot start controller manager")
		os.Exit(1)
	}
}

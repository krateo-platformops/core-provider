package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/krateo-platformops/unstructured-runtime/pkg/controller"
	"github.com/krateo-platformops/unstructured-runtime/pkg/controller/builder"
	ctrlevent "github.com/krateo-platformops/unstructured-runtime/pkg/controller/event"
	"github.com/krateo-platformops/unstructured-runtime/pkg/metrics/server"
	"github.com/krateo-platformops/unstructured-runtime/pkg/telemetry"
	"github.com/krateo-platformops/unstructured-runtime/pkg/tools/statusprojection"

	"github.com/go-logr/logr"
	"github.com/krateo-platformops/composition-dynamic-controller/internal/authn"
	"github.com/krateo-platformops/composition-dynamic-controller/internal/composition"
	"github.com/krateo-platformops/composition-dynamic-controller/internal/metrics"
	"github.com/krateo-platformops/composition-dynamic-controller/internal/snowplow"
	"github.com/krateo-platformops/composition-dynamic-controller/internal/tools/archive"
	"github.com/krateo-platformops/composition-dynamic-controller/internal/tools/dynamic"
	"github.com/krateo-platformops/composition-dynamic-controller/pkg/meta"
	"github.com/krateo-platformops/plumbing/env"
	"github.com/krateo-platformops/plumbing/kubeutil/event"
	"github.com/krateo-platformops/plumbing/kubeutil/eventrecorder"
	"github.com/krateo-platformops/plumbing/ptr"

	"github.com/krateo-platformops/unstructured-runtime/pkg/logging"
	"github.com/krateo-platformops/unstructured-runtime/pkg/pluralizer"
	"github.com/krateo-platformops/unstructured-runtime/pkg/workqueue"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	serviceName               = "composition-dynamic-controller"
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
	// Flags
	kubeconfig := flag.String("kubeconfig", env.String("KUBECONFIG", ""),
		"absolute path to the kubeconfig file")
	debug := flag.Bool("debug",
		env.Bool("COMPOSITION_CONTROLLER_DEBUG", false), "dump verbose output")
	workers := flag.Int("workers", env.Int("COMPOSITION_CONTROLLER_WORKERS", 1), "number of workers")
	resyncInterval := flag.Duration("resync-interval",
		env.Duration("COMPOSITION_CONTROLLER_RESYNC_INTERVAL", time.Minute*3), "resync interval")
	resourceGroup := flag.String("group",
		env.String("COMPOSITION_CONTROLLER_GROUP", ""), "resource api group")
	resourceVersion := flag.String("version",
		env.String("COMPOSITION_CONTROLLER_VERSION", ""), "resource api version")
	resourceName := flag.String("resource",
		env.String("COMPOSITION_CONTROLLER_RESOURCE", ""), "resource plural name")
	namespace := flag.String("namespace",
		env.String("COMPOSITION_CONTROLLER_NAMESPACE", ""), "namespace to watch, empty for all namespaces")
	chart := flag.String("chart",
		env.String("COMPOSITION_CONTROLLER_CHART", ""), "chart")
	urlChartInspector := flag.String("urlChartInspector",
		env.String("URL_CHART_INSPECTOR", "http://chart-inspector.krateo-system.svc.cluster.local:8081/"), "url chart inspector")
	saName := flag.String("saName",
		env.String("COMPOSITION_CONTROLLER_SA_NAME", ""), "service account name")
	saNamespace := flag.String("saNamespace",
		env.String("COMPOSITION_CONTROLLER_SA_NAMESPACE", ""), "service account namespace")
	statusDataTemplate := flag.String("status-data-template",
		env.String("COMPOSITION_CONTROLLER_STATUS_DATA_TEMPLATE", ""),
		"JSON-encoded statusDataTemplate ([{forPath,expression}]) shipped by core-provider from the CompositionDefinition")
	apiRefName := flag.String("api-ref-name",
		env.String("COMPOSITION_CONTROLLER_API_REF_NAME", ""),
		"name of the RESTAction resolved (via snowplow) for the .api status-projection source; empty disables apiRef resolution")
	apiRefNamespace := flag.String("api-ref-namespace",
		env.String("COMPOSITION_CONTROLLER_API_REF_NAMESPACE", ""),
		"namespace of the apiRef RESTAction")
	apiRefExtras := flag.String("api-ref-extras",
		env.String("COMPOSITION_CONTROLLER_API_REF_EXTRAS", ""),
		"JSON object of static extras merged into the apiRef resolution (snowplow spec.apiRef.extras)")
	snowplowURL := flag.String("snowplow-url",
		env.String("URL_SNOWPLOW", "http://snowplow.krateo-system.svc.cluster.local:8081"),
		"snowplow base URL for resolving RESTActions (.api status source)")
	authnURL := flag.String("authn-url",
		env.String("URL_AUTHN", "http://authn.krateo-system.svc.cluster.local:8082"),
		"authn base URL for exchanging the projected ServiceAccount token for a service JWT")
	saTokenPath := flag.String("serviceaccount-token-path",
		env.String("COMPOSITION_CONTROLLER_SERVICEACCOUNT_TOKEN_PATH", authn.DefaultTokenPath),
		"path to the projected (authn-audience) ServiceAccount token used to authenticate to authn")
	maxErrorRetryInterval := flag.Duration("max-error-retry-interval",
		env.Duration("COMPOSITION_CONTROLLER_MAX_ERROR_RETRY_INTERVAL", 60*time.Second), "The maximum interval between retries when an error occurs. This should be less than the half of the poll interval.")
	minErrorRetryInterval := flag.Duration("min-error-retry-interval",
		env.Duration("COMPOSITION_CONTROLLER_MIN_ERROR_RETRY_INTERVAL", 1*time.Second), "The minimum interval between retries when an error occurs. This should be less than max-error-retry-interval.")
	maxErrorRetry := flag.Int("max-error-retries",
		env.Int("COMPOSITION_CONTROLLER_MAX_ERROR_RETRIES", 5), "How many times to retry the processing of a resource when an error occurs before giving up and dropping the resource.")
	metricsServerPort := flag.Int("metrics-server-port",
		env.Int("COMPOSITION_CONTROLLER_METRICS_SERVER_PORT", 0), "The address to bind the metrics server to. If empty, metrics server is disabled.")
	gracefulShutdownTimeout := flag.Duration("graceful-shutdown-timeout",
		env.Duration("COMPOSITION_CONTROLLER_GRACEFUL_SHUTDOWN_TIMEOUT", 30*time.Second),
		"Max time to let in-flight reconciles finish after SIGTERM before the process exits. Must be set below the pod's terminationGracePeriodSeconds. 0 disables the drain (abrupt shutdown); negative waits indefinitely.")
	safeReleaseName := flag.Bool("safe-release-name",
		env.Bool("COMPOSITION_CONTROLLER_SAFE_RELEASE_NAME", true), "If disabled the randmom suffix is not appended in the Helm release name. This can be useful for avoid having problems with complex helm charts. The use of this option is highly discouraged, as it can lead to release name collisions.")
	otelEnabled := flag.Bool("otel-enabled", env.Bool("OTEL_ENABLED", false), "Enable OTLP metrics export for provider-runtime telemetry.")
	otelTracingEnabled := flag.Bool("otel-tracing-enabled", env.Bool("OTEL_TRACING_ENABLED", false), "Enable OTLP trace export (distributed reconcile traces).")
	otelLogsEnabled := flag.Bool("otel-logs-enabled", env.Bool("OTEL_LOGS_ENABLED", false), "Enable OTLP log export (logs as a first-class OTel signal, in addition to the stderr JSON stream).")
	otelServiceName := flag.String("otel-service-name", serviceName, "The service name attached to exported OTLP metrics/traces.")
	otelExportInterval := flag.Duration("otel-export-interval", env.Duration("OTEL_EXPORT_INTERVAL", defaultOtelExportInterval), "The interval used to export OTLP metrics.")
	deploymentName := flag.String("deployment-name", env.String("DEPLOYMENT_NAME", ""), "The deployment name for stable resource identification in metrics.")
	// service.version prefers the binary's own build stamp over the SERVICE_VERSION the chart injects
	// (chart-set cdc.image.tag): the stamp describes the image actually running, the env var describes
	// what the chart believes it deployed, and they diverge whenever a tag is moved or an image is
	// pinned by digest. SERVICE_VERSION stays the fallback for unstamped (local/dev) builds (#105).
	serviceVersion := env.String("SERVICE_VERSION", "")
	if buildVersion != "dev" && buildVersion != "" {
		serviceVersion = buildVersion
	}

	flag.Usage = func() {
		fmt.Fprintln(flag.CommandLine.Output(), "Flags:")
		flag.PrintDefaults()
	}

	flag.Parse()

	go func() {
		log.Println(http.ListenAndServe("localhost:6060", nil))
	}()

	logLevel := slog.LevelInfo
	if *debug {
		logLevel = slog.LevelDebug
	}

	// Tag this controller's telemetry resource with the composition TYPE it manages
	// (krateo.io/composition-gvr); appended to OTEL_RESOURCE_ATTRIBUTES so resource.Default() picks it
	// up — shared by the metrics, trace AND log resources. Set BEFORE any exporter/handler is built.
	gvrAttr := fmt.Sprintf("krateo.io/composition-gvr=%s/%s/%s", *resourceGroup, *resourceVersion, *resourceName)
	if existing := os.Getenv("OTEL_RESOURCE_ATTRIBUTES"); existing != "" {
		gvrAttr = existing + "," + gvrAttr
	}
	os.Setenv("OTEL_RESOURCE_ATTRIBUTES", gvrAttr)

	// Install the OTLP LoggerProvider BEFORE building the log handler so its otelslog bridge captures
	// it and log records export over OTLP (gated OTEL_LOGS_ENABLED). No app logger exists yet, so a
	// bootstrap error goes to the stdlib logger.
	logsShutdown, err := telemetry.SetupOTLPLogs(context.Background(), *otelLogsEnabled, *otelServiceName)
	if err != nil {
		log.Printf("cannot initialize OpenTelemetry logs: %v", err)
		os.Exit(1)
	}

	// JSON logs on stderr in the OTel log model (RFC3339Nano "timestamp", SeverityText +
	// SeverityNumber, trace_id/span_id when a span is in context), compatible with logs-ingester, AND
	// — when OTEL_LOGS_ENABLED — teed to the OTLP pipeline. The shared handler lives in
	// unstructured-runtime (pkg/logging) so every composition controller is consistent.
	log := logging.NewLogrLogger(logr.FromSlogHandler(logging.NewOTelHandler(logLevel, os.Stderr, *otelServiceName)))

	// Announce the build stamp first: `kubectl logs` alone should answer "which commit is this
	// controller running", without needing an OTel pipeline or a metrics scrape (#105).
	log.Info("starting composition-dynamic-controller", "version", buildVersion, "commit", buildCommit)

	defer func() {
		if err := logsShutdown(context.Background()); err != nil {
			log.Error(err, "Cannot shutdown OpenTelemetry logs")
		}
	}()

	// Parse the declarative status projections shipped from the CompositionDefinition.
	// An invalid value is non-fatal: status projection is simply disabled (baseline status
	// is unaffected).
	var statusMappings []statusprojection.Mapping
	if s := strings.TrimSpace(*statusDataTemplate); s != "" {
		if err := json.Unmarshal([]byte(s), &statusMappings); err != nil {
			log.Info("ignoring invalid COMPOSITION_CONTROLLER_STATUS_DATA_TEMPLATE", "error", err.Error())
		}
	}

	// Build the apiRef resolver when an apiRef is declared: the CDC exchanges its projected
	// ServiceAccount token for an authn JWT (authn.Client.Token) and uses it as the Bearer for
	// snowplow's /call endpoint, resolving the RESTAction's status into the ".api" projection
	// source. Absent apiRef → nil resolver (the ".api" source is simply unavailable).
	var apiResolver composition.APIResolver
	if name := strings.TrimSpace(*apiRefName); name != "" {
		var extras map[string]any
		if s := strings.TrimSpace(*apiRefExtras); s != "" {
			if err := json.Unmarshal([]byte(s), &extras); err != nil {
				log.Info("ignoring invalid COMPOSITION_CONTROLLER_API_REF_EXTRAS", "error", err.Error())
			}
		}
		authnClient := authn.New(*authnURL, *saTokenPath)
		snowplowClient := snowplow.New(*snowplowURL, authnClient.Token)
		apiResolver = composition.NewSnowplowAPIResolver(
			snowplowClient,
			snowplow.ApiRef{Name: name, Namespace: *apiRefNamespace},
			extras,
		)
		log.Info("apiRef status source enabled", "name", name, "namespace", *apiRefNamespace, "snowplow", *snowplowURL)
	}

	// Kubernetes configuration
	var cfg *rest.Config
	if len(*kubeconfig) > 0 {
		cfg, err = clientcmd.BuildConfigFromFlags("", *kubeconfig)
	} else {
		cfg, err = builder.GetConfig()
	}
	if err != nil {
		log.Error(err, "Building kubeconfig.")
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), []os.Signal{
		os.Interrupt,
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGKILL,
		syscall.SIGHUP,
		syscall.SIGQUIT,
	}...)
	defer cancel()

	rec, err := eventrecorder.Create(ctx, cfg, "composition-dynamic-controller", nil)
	if err != nil {
		log.Error(err, "Creating event recorder.")
		os.Exit(1)
	}
	pluralizer := pluralizer.New()

	var pig archive.Getter
	if len(*chart) > 0 {
		pig = archive.Static(*chart)
	} else {
		pig, err = archive.Dynamic(cfg, pluralizer)
		if err != nil {
			log.Error(err, "Creating chart url info getter.")
			os.Exit(1)
		}
	}

	log.WithValues("debug", *debug).
		WithValues("resyncInterval", *resyncInterval).
		WithValues("group", *resourceGroup).
		WithValues("version", *resourceVersion).
		WithValues("resource", *resourceName).
		WithValues("namespace", *namespace).
		WithValues("workers", *workers).
		WithValues("minErrorRetryInterval", *minErrorRetryInterval).
		WithValues("maxErrorRetryInterval", *maxErrorRetryInterval).
		WithValues("maxErrorRetry", *maxErrorRetry).
		WithValues("metricsServerPort", *metricsServerPort).
		WithValues("safeReleaseName", *safeReleaseName).
		Info("Starting composition dynamic controller.")

	log.WithValues("deploymentName", *deploymentName).
		WithValues("otelEnabled", *otelEnabled).
		WithValues("otelServiceName", *otelServiceName).
		WithValues("otelExportInterval", *otelExportInterval).
		Info("[FLAG PARSE DEBUG] Telemetry configuration loaded from flags/env.")

	telemetryEnabled := *otelEnabled
	telemetryExportInterval := *otelExportInterval

	telemetryConfig := telemetry.Config{
		Enabled:        telemetryEnabled,
		TracingEnabled: *otelTracingEnabled,
		ServiceName:    *otelServiceName,
		ExportInterval: telemetryExportInterval,
		DeploymentName: *deploymentName,
		Version:        serviceVersion,
	}

	telemetryMetrics, telemetryShutdown, err := telemetry.Setup(context.Background(), log, telemetryConfig)
	if err != nil {
		log.Error(err, "Cannot initialize OpenTelemetry metrics")
		os.Exit(1)
	}
	defer telemetryShutdown(context.Background())

	// Distributed reconcile traces (gated OTEL_TRACING_ENABLED). Installs the W3C propagator
	// even when disabled so an inbound traceparent is honored; returns a no-op shutdown.
	tracingShutdown, err := telemetry.SetupTracing(context.Background(), log, telemetryConfig)
	if err != nil {
		log.Error(err, "Cannot initialize OpenTelemetry tracing")
		os.Exit(1)
	}
	defer tracingShutdown(context.Background())

	// Initialize CDC-specific metrics
	if err := metrics.InitMetrics(context.Background(), log, telemetryEnabled, *otelServiceName, telemetryExportInterval, *deploymentName); err != nil {
		log.Warn("Cannot initialize CDC metrics, continuing without metrics", "error", err)
	}
	metrics.DebugStatus()

	// Periodic metrics status logging for monitoring during stress tests
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				metrics.DebugStatus()
			}
		}
	}()

	// Create a label requirement for the composition version
	labelreq, err := labels.NewRequirement(meta.CompositionVersionLabel, selection.Equals, []string{*resourceVersion})
	if err != nil {
		log.Error(err, "Creating label requirement.")
		os.Exit(1)
	}
	labelselector := labels.NewSelector().Add(*labelreq)

	mapper, err := dynamic.NewRESTMapper(cfg)
	if err != nil {
		log.Error(err, "Creating RESTMapper.")
		os.Exit(1)
	}
	// Keep the RESTMapper's cached discovery fresh: watch CustomResourceDefinition events
	// and invalidate on any change, so newly-registered CRDs/versions (e.g. component CRDs
	// generated mid-rollout, or the `inst.crdExists` Pass-B gate) are visible without a
	// controller restart. Non-fatal: IsNamespaced still reactively resets on lookup miss.
	if err := dynamic.WatchCRDsAndInvalidate(ctx, cfg, mapper, log); err != nil {
		log.Error(err, "Starting CustomResourceDefinition watch for discovery invalidation; continuing without it.")
	}
	apiRecorder := event.NewAPIRecorder(rec)
	if apiRecorder == nil {
		log.Error(fmt.Errorf("creating API recorder"), "API recorder is nil, events will not be recorded.")
		os.Exit(1)
	}

	handler := composition.NewHandler(&composition.HandlerOptions{
		Kubeconfig:         cfg,
		Pluralizer:         pluralizer,
		PackageInfoGetter:  pig,
		EventRecorder:      *apiRecorder,
		ChartInspectorUrl:  *urlChartInspector,
		SaName:             *saName,
		SaNamespace:        *saNamespace,
		SafeReleaseName:    *safeReleaseName,
		Mapper:             mapper,
		StatusDataTemplate: statusMappings,
		APIResolver:        apiResolver,
	})

	opts := []builder.FuncOption{
		builder.WithLogger(log),
		builder.WithNamespace(*namespace),
		builder.WithResyncInterval(*resyncInterval),
		builder.WithGlobalRateLimiter(workqueue.NewExponentialTimedFailureRateLimiter[any](*minErrorRetryInterval, *maxErrorRetryInterval)),
		builder.WithMaxRetries(*maxErrorRetry),
		builder.WithGracefulShutdownTimeout(*gracefulShutdownTimeout),
		builder.WithListWatcher(controller.ListWatcherConfiguration{
			LabelSelector: ptr.To(labelselector.String()),
		}),
		builder.WithActionEvent(ctrlevent.CRUpdated, ctrlevent.Observe),
		builder.WithTelemetryMetrics(telemetryMetrics),
	}

	metricsServerBindAddress := ""
	if ptr.Deref(metricsServerPort, 0) != 0 {
		log.Info("Metrics server enabled", "bindAddress", fmt.Sprintf(":%d", *metricsServerPort))
		metricsServerBindAddress = fmt.Sprintf(":%d", *metricsServerPort)
		opts = append(opts, builder.WithMetrics(server.Options{
			BindAddress: metricsServerBindAddress,
		}))
	} else {
		log.Info("Metrics server disabled")
	}

	controller, err := builder.Build(ctx, builder.Configuration{
		Config: cfg,
		GVR: schema.GroupVersionResource{
			Group:    *resourceGroup,
			Version:  *resourceVersion,
			Resource: *resourceName,
		},
		ProviderName: serviceName,
	}, opts...)
	if err != nil {
		log.Error(err, "Creating controller.")
		os.Exit(1)
	}

	controller.SetExternalClient(handler)

	err = controller.Run(ctx, *workers)
	if err != nil {
		log.Error(err, "Running controller.")
		os.Exit(1)
	}
}

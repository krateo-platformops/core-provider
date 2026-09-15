package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	_ "github.com/krateo-platformops/chart-inspector/docs"
	"github.com/krateo-platformops/chart-inspector/internal/handlers"
	"github.com/krateo-platformops/chart-inspector/internal/handlers/health"
	getresources "github.com/krateo-platformops/chart-inspector/internal/handlers/resources/get"
	"github.com/krateo-platformops/chart-inspector/internal/telemetry"
	"github.com/krateo-platformops/plumbing/env"
	"github.com/krateo-platformops/plumbing/helm/getter/cache"
	helmv3 "github.com/krateo-platformops/plumbing/helm/v3"
	"github.com/krateo-platformops/plumbing/logger"
	plurals "github.com/krateo-platformops/unstructured-runtime/pkg/pluralizer"
	httpSwagger "github.com/swaggo/http-swagger"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	serviceName = "chart-inspector"
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

// @title 		 Chart Inspector API
// @version         1.0
// @description   This is the API for the Chart Inspector service. It provides endpoints for inspecting Helm charts.
// @BasePath		/
func main() {
	debugOn := flag.Bool("debug", env.Bool("DEBUG", false), "dump verbose output")
	port := flag.Int("port", env.Int("PLUGIN_PORT", 8081), "port to listen on")
	kubeconfig := flag.String("kubeconfig", env.String("KUBECONFIG", ""),
		"absolute path to the kubeconfig file")
	krateoNamespace := env.String("KRATEO_NAMESPACE", "krateo-system")

	flag.Parse()

	mux := http.NewServeMux()

	// Telemetry is gated and defaults to OFF: when disabled, Setup registers
	// nothing and the behaviour below is byte-identical to a build without it.
	telCtx := context.Background()
	telCfg := telemetry.ConfigFromEnv()
	tel, err := telemetry.Setup(telCtx, telCfg)
	if err != nil {
		// Fall back to the no-op provider; never fail startup on telemetry.
		logger.New(serviceName, *debugOn).Error("setting up telemetry", slog.Any("error", err))
		tel = &telemetry.Provider{}
	}
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tel.Shutdown(shutCtx)
	}()

	// Build the request logger. Default-off path is the unchanged plumbing
	// logger (byte-identical, incl. the debug env dump). When tracing is on,
	// wrap the same JSON handler to enrich records with trace_id/span_id so
	// OTel-JSON logs correlate with spans.
	log := logger.New(serviceName, *debugOn)
	if tel.TracingEnabled() {
		log = slog.New(tel.WithTraceCorrelation(logger.NewHandler(*debugOn, os.Stderr))).
			With(slog.String("service", serviceName))
	}

	// Announce the build stamp first: `kubectl logs` alone should answer "which commit is this pod
	// running", without needing an OTel pipeline or a metrics scrape (#105).
	log.Info("starting chart-inspector",
		slog.String("version", buildVersion), slog.String("commit", buildCommit))

	go func() {
		if err := http.ListenAndServe("localhost:6060", nil); err != nil {
			log.Error("pprof server stopped", slog.Any("error", err))
		}
	}()

	// Kubernetes configuration
	var cfg *rest.Config
	if len(*kubeconfig) > 0 {
		cfg, err = clientcmd.BuildConfigFromFlags("", *kubeconfig)
	} else {
		cfg, err = rest.InClusterConfig()
	}
	if err != nil {
		log.Error("Building kubeconfig.", "error", err)
		os.Exit(1)
	}

	cfg.QPS = -1 // rely on k8s api server rate limiting

	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		log.Error("Creating dynamic client.", "error", err)
		os.Exit(1)
	}

	pluralizer := plurals.New()

	// Initialize Helm client with global cache and CRD informer
	helmClient, err := helmv3.NewClient(cfg,
		helmv3.WithLogger(func(format string, v ...interface{}) {
			log.Debug(fmt.Sprintf(format, v...))
		}),
		helmv3.WithCache(
			cache.WithCleanupInterval(5*time.Minute),
			cache.WithTTL(1*time.Hour),
		),
		helmv3.WithCRDInformer(cfg, 30*time.Minute),
	)
	if err != nil {
		log.Error("Creating helm client.", "error", err)
		os.Exit(1)
	}

	opts := handlers.HandlerOptions{
		Log:             log,
		DynamicClient:   dyn,
		KrateoNamespace: krateoNamespace,
		RestConfig:      cfg,
		Plurarizer:      pluralizer,
		HelmClient:      helmClient,
	}

	healthy := int32(0)

	// Health probes are registered unwrapped (otelhttp/metrics filter them
	// anyway). The /resources inspection — the cdc's chart-inspection
	// dependency and the heavy endpoint — is instrumented so its server span
	// parents off the cdc's inbound traceparent, completing the
	// cdc -> chart-inspector trace chain.
	mux.Handle("/healthz", health.Live())
	mux.Handle("/readyz", health.Ready(&healthy))
	mux.Handle("/resources", tel.Instrument("/resources", getresources.GetResources(opts)))
	mux.Handle("/swagger/", httpSwagger.WrapHandler)

	server := &http.Server{
		Addr:        fmt.Sprintf(":%d", *port),
		Handler:     mux,
		ReadTimeout: 10 * time.Second,
		// Rendering a large umbrella (e.g. the installer) is a helm dry-run install plus a Pass-B
		// `lookup` sweep over the whole cluster; on a fully-deployed cluster that response takes ~1 min
		// to produce and write. At 50s the server closed the connection mid-write ("write ... i/o
		// timeout"), and the cdc saw EOF -> the version upgrade wedged (D9, 2026-07-08). 300s gives
		// headroom for large clusters. (The cdc's client read timeout is raised to match.)
		WriteTimeout: 300 * time.Second,
		IdleTimeout:  30 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), []os.Signal{
		os.Interrupt,
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGKILL,
		syscall.SIGHUP,
		syscall.SIGQUIT,
	}...)
	defer stop()

	go func() {
		atomic.StoreInt32(&healthy, 1)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("could not listen", slog.String("addr", server.Addr), slog.Any("error", err))
			os.Exit(1)
		}
	}()

	// Listen for the interrupt signal.
	log.Info("server is ready to handle requests", slog.Any("port", *port))
	<-ctx.Done()

	// Restore default behavior on the interrupt signal and notify user of shutdown.
	stop()
	log.Info("server is shutting down gracefully, press Ctrl+C again to force")
	atomic.StoreInt32(&healthy, 0)

	// Clean up helm client resources
	if helmClient != nil {
		if err := helmClient.Close(); err != nil {
			log.Error("closing helm client", "error", err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	server.SetKeepAlivesEnabled(false)
	if err := server.Shutdown(ctx); err != nil {
		log.Error("server forced to shutdown", slog.Any("error", err))
		os.Exit(1)
	}

	log.Info("server gracefully stopped")
}

package metrics

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/krateo-platformops/unstructured-runtime/pkg/logging"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
)

const (
	defaultServiceName    = "composition-dynamic-controller"
	defaultExportInterval = 30 * time.Second
)

// Global metrics instance for backward compatibility
var (
	instanceMetrics *Metrics
	metricsMutex    sync.RWMutex
)

// Config controls OpenTelemetry metrics export.
type Config struct {
	Enabled        bool
	ServiceName    string
	ExportInterval time.Duration
	DeploymentName string // Deployment name for stable resource identification
}

// Metrics exposes composition-dynamic-controller metrics.
//
// A Metrics is a VIEW: shared instruments plus the attributes it adds to every point. The instance
// Setup builds adds only the process identity; With adds a controller's (krateo.composition.gvr,
// docs/design/cdc-pooling.md step 3), so controllers sharing a process stay attributable.
type Metrics struct {
	log            logging.Logger
	deploymentName string

	// extra are this view's attributes, added to every point after the process identity.
	extra []attribute.KeyValue

	// Chart Inspector metrics
	chartInspectorDuration metric.Float64Histogram
	chartInspectorTotal    metric.Int64Counter
	chartInspectorErrors   metric.Int64Counter

	// RBAC Generation metrics
	rbacGenerationDuration  metric.Float64Histogram
	rbacGenerationTotal     metric.Int64Counter
	rbacGenerationErrors    metric.Int64Counter
	rbacGenerationDecisions metric.Int64Counter

	// RBAC Apply metrics
	rbacApplyDuration metric.Float64Histogram
	rbacApplyTotal    metric.Int64Counter
	rbacApplyErrors   metric.Int64Counter

	// Helm Install metrics
	helmInstallDuration metric.Float64Histogram
	helmInstallTotal    metric.Int64Counter
	helmInstallErrors   metric.Int64Counter

	// Helm Upgrade metrics
	helmUpgradeDuration metric.Float64Histogram
	helmUpgradeTotal    metric.Int64Counter
	helmUpgradeErrors   metric.Int64Counter

	// Helm Uninstall metrics
	helmUninstallDuration metric.Float64Histogram
	helmUninstallTotal    metric.Int64Counter
	helmUninstallErrors   metric.Int64Counter
}

// Setup creates and configures an OTLP metrics pipeline.
//
// When metrics are disabled, Setup returns a nil Metrics handle and a no-op
// shutdown function.
func Setup(ctx context.Context, log logging.Logger, cfg Config) (*Metrics, func(context.Context) error, error) {
	if !cfg.Enabled {
		return nil, func(context.Context) error { return nil }, nil
	}

	serviceName := cfg.ServiceName
	if serviceName == "" {
		serviceName = defaultServiceName
	}

	exportInterval := cfg.ExportInterval
	if exportInterval <= 0 {
		exportInterval = defaultExportInterval
	}

	exporter, err := otlpmetrichttp.New(ctx)
	if err != nil {
		return nil, nil, err
	}

	// Build resource attributes including deployment name for stable multi-instance identification
	attrs := []attribute.KeyValue{
		attribute.String("service.name", serviceName),
	}
	if cfg.DeploymentName != "" {
		attrs = append(attrs,
			attribute.String("k8s.deployment.name", cfg.DeploymentName),
			attribute.String("service.instance.id", cfg.DeploymentName),
		)
	}

	res, err := resource.Merge(resource.Default(),
		resource.NewSchemaless(attrs...))
	if err != nil {
		return nil, nil, err
	}

	// Define custom histogram buckets for better granularity
	// Ranges from 1ms to 10s with smaller steps for better quantile precision
	customBuckets := []float64{
		0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0,
		2.5, 5.0, 10.0, 25.0, 50.0, 100.0, 250.0, 500.0, 1000.0,
		2500.0, 5000.0, 10000.0,
	}

	reader := sdkmetric.NewPeriodicReader(exporter, sdkmetric.WithInterval(exportInterval))
	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(reader),
		sdkmetric.WithResource(res),
		// Add custom histogram buckets for all histograms
		sdkmetric.WithView(
			sdkmetric.NewView(
				sdkmetric.Instrument{Kind: sdkmetric.InstrumentKindHistogram},
				sdkmetric.Stream{Aggregation: sdkmetric.AggregationExplicitBucketHistogram{Boundaries: customBuckets}},
			),
		),
	)

	meter := provider.Meter("github.com/krateo-platformops/composition-dynamic-controller")
	m, err := newMetrics(meter, log, cfg.DeploymentName)
	if err != nil {
		_ = provider.Shutdown(ctx)
		return nil, nil, err
	}

	otel.SetMeterProvider(provider)
	log.Info("OpenTelemetry metrics initialized", "deploymentName", cfg.DeploymentName, "serviceName", serviceName, "exportInterval", exportInterval)

	return m, provider.Shutdown, nil
}

func newMetrics(meter metric.Meter, log logging.Logger, deploymentName string) (*Metrics, error) {
	var err error
	m := &Metrics{
		log:            log,
		deploymentName: deploymentName,
	}

	// Chart Inspector metrics
	m.chartInspectorDuration, err = meter.Float64Histogram(
		"composition_chart_inspector_duration_seconds",
		metric.WithDescription("Time spent calling chart-inspector service (seconds)"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, err
	}

	m.chartInspectorTotal, err = meter.Int64Counter(
		"composition_chart_inspector_total",
		metric.WithDescription("Total number of chart-inspector calls"),
	)
	if err != nil {
		return nil, err
	}

	m.chartInspectorErrors, err = meter.Int64Counter(
		"composition_chart_inspector_errors",
		metric.WithDescription("Total number of chart-inspector call failures"),
	)
	if err != nil {
		return nil, err
	}

	// RBAC Generation metrics
	m.rbacGenerationDuration, err = meter.Float64Histogram(
		"composition_rbac_generation_duration_seconds",
		metric.WithDescription("Time spent generating RBAC policies (seconds)"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, err
	}

	m.rbacGenerationTotal, err = meter.Int64Counter(
		"composition_rbac_generation_total",
		metric.WithDescription("Total number of RBAC generation operations"),
	)
	if err != nil {
		return nil, err
	}

	m.rbacGenerationErrors, err = meter.Int64Counter(
		"composition_rbac_generation_errors",
		metric.WithDescription("Total number of RBAC generation failures"),
	)
	if err != nil {
		return nil, err
	}

	m.rbacGenerationDecisions, err = meter.Int64Counter(
		"composition_rbac_generation_decisions_total",
		metric.WithDescription("Observe passes by what the derived-RBAC cache decided (#697): reason=skipped re-applied the cached rules without asking chart-inspector; lookup (the chart calls lookup), key_changed and unscannable asked it"),
	)
	if err != nil {
		return nil, err
	}

	// RBAC Apply metrics
	m.rbacApplyDuration, err = meter.Float64Histogram(
		"composition_rbac_apply_duration_seconds",
		metric.WithDescription("Time spent applying RBAC policies to cluster (seconds)"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, err
	}

	m.rbacApplyTotal, err = meter.Int64Counter(
		"composition_rbac_apply_total",
		metric.WithDescription("Total number of RBAC apply operations"),
	)
	if err != nil {
		return nil, err
	}

	m.rbacApplyErrors, err = meter.Int64Counter(
		"composition_rbac_apply_errors",
		metric.WithDescription("Total number of RBAC apply failures"),
	)
	if err != nil {
		return nil, err
	}

	// Helm Install metrics
	m.helmInstallDuration, err = meter.Float64Histogram(
		"composition_helm_install_duration_seconds",
		metric.WithDescription("Time spent installing Helm charts (seconds)"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, err
	}

	m.helmInstallTotal, err = meter.Int64Counter(
		"composition_helm_install_total",
		metric.WithDescription("Total number of Helm chart installations"),
	)
	if err != nil {
		return nil, err
	}

	m.helmInstallErrors, err = meter.Int64Counter(
		"composition_helm_install_errors",
		metric.WithDescription("Total number of Helm chart installation failures"),
	)
	if err != nil {
		return nil, err
	}

	// Helm Upgrade metrics
	m.helmUpgradeDuration, err = meter.Float64Histogram(
		"composition_helm_upgrade_duration_seconds",
		metric.WithDescription("Time spent upgrading Helm charts (seconds)"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, err
	}

	m.helmUpgradeTotal, err = meter.Int64Counter(
		"composition_helm_upgrade_total",
		metric.WithDescription("Total number of Helm chart upgrades"),
	)
	if err != nil {
		return nil, err
	}

	m.helmUpgradeErrors, err = meter.Int64Counter(
		"composition_helm_upgrade_errors",
		metric.WithDescription("Total number of Helm chart upgrade failures"),
	)
	if err != nil {
		return nil, err
	}

	// Helm Uninstall metrics
	m.helmUninstallDuration, err = meter.Float64Histogram(
		"composition_helm_uninstall_duration_seconds",
		metric.WithDescription("Time spent uninstalling Helm charts (seconds)"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, err
	}

	m.helmUninstallTotal, err = meter.Int64Counter(
		"composition_helm_uninstall_total",
		metric.WithDescription("Total number of Helm chart uninstallations"),
	)
	if err != nil {
		return nil, err
	}

	m.helmUninstallErrors, err = meter.Int64Counter(
		"composition_helm_uninstall_errors",
		metric.WithDescription("Total number of Helm chart uninstallation failures"),
	)
	if err != nil {
		return nil, err
	}

	return m, nil
}

// instanceAttrs returns metric attributes for instance identification.
// This enables metrics exported to Prometheus to include service.instance.id as exported_instance.
func (m *Metrics) instanceAttrs() []attribute.KeyValue {
	if m == nil {
		return nil
	}
	var attrs []attribute.KeyValue
	if m.deploymentName != "" {
		attrs = append(attrs, attribute.String("service.instance.id", m.deploymentName))
	}
	return append(attrs, m.extra...)
}

// With returns a view of m that adds attrs to every point, sharing m's instruments. A nil m returns
// nil, like every other method.
func (m *Metrics) With(attrs ...attribute.KeyValue) *Metrics {
	if m == nil {
		return nil
	}
	v := *m
	v.extra = append(append([]attribute.KeyValue(nil), m.extra...), attrs...)
	return &v
}

// AttrCompositionGVR names the controller on every cdc signal: metric points, reconcile root spans
// and log records (docs/design/cdc-pooling.md step 3). The value is <group>/<version>/<plural>, the
// same as the process-wide resource attribute krateo.io/composition-gvr, but it travels on each
// signal, so it stays right when one process hosts several controllers. A dedicated cdc sets it
// too, so a query keyed on it works the same before and after pooling (observability, 2026-11-15).
const AttrCompositionGVR = attribute.Key("krateo.composition.gvr")

// ControllerAttr is the krateo.composition.gvr attribute for the controller of group/version/resource.
func ControllerAttr(group, version, resource string) attribute.KeyValue {
	return AttrCompositionGVR.String(group + "/" + version + "/" + resource)
}

type viewKey struct{}

// NewContext carries a controller's view in ctx, so the timers below record it without a global.
// A nil view leaves ctx unchanged.
func NewContext(ctx context.Context, m *Metrics) context.Context {
	if m == nil {
		return ctx
	}
	return context.WithValue(ctx, viewKey{}, m)
}

// FromContext returns the controller's view carried in ctx, or the process instance when ctx carries
// none (nil when metrics are off).
func FromContext(ctx context.Context) *Metrics {
	if ctx != nil {
		if m, ok := ctx.Value(viewKey{}).(*Metrics); ok && m != nil {
			return m
		}
	}
	return GetInstance()
}

// Chart Inspector methods

func (m *Metrics) RecordChartInspectorDuration(ctx context.Context, duration float64) {
	if m == nil || m.chartInspectorDuration == nil {
		return
	}
	m.chartInspectorDuration.Record(ctx, duration, metric.WithAttributes(m.instanceAttrs()...))
}

func (m *Metrics) IncChartInspectorTotal(ctx context.Context) {
	if m == nil || m.chartInspectorTotal == nil {
		return
	}
	m.chartInspectorTotal.Add(ctx, 1, metric.WithAttributes(m.instanceAttrs()...))
}

func (m *Metrics) IncChartInspectorErrors(ctx context.Context) {
	if m == nil || m.chartInspectorErrors == nil {
		return
	}
	m.chartInspectorErrors.Add(ctx, 1, metric.WithAttributes(m.instanceAttrs()...))
}

// RBAC Generation methods

func (m *Metrics) RecordRBACGenerationDuration(ctx context.Context, duration float64) {
	if m == nil || m.rbacGenerationDuration == nil {
		return
	}
	m.rbacGenerationDuration.Record(ctx, duration, metric.WithAttributes(m.instanceAttrs()...))
}

func (m *Metrics) IncRBACGenerationTotal(ctx context.Context) {
	if m == nil || m.rbacGenerationTotal == nil {
		return
	}
	m.rbacGenerationTotal.Add(ctx, 1, metric.WithAttributes(m.instanceAttrs()...))
}

// IncRBACGenerationDecision counts an Observe pass under the reason the derived-RBAC cache decided as
// it did (#697). reason=skipped passes made no chart-inspector call; every other reason made one.
func (m *Metrics) IncRBACGenerationDecision(ctx context.Context, reason string) {
	if m == nil || m.rbacGenerationDecisions == nil {
		return
	}
	m.rbacGenerationDecisions.Add(ctx, 1, metric.WithAttributes(append(m.instanceAttrs(), attribute.String("reason", reason))...))
}

func (m *Metrics) IncRBACGenerationErrors(ctx context.Context) {
	if m == nil || m.rbacGenerationErrors == nil {
		return
	}
	m.rbacGenerationErrors.Add(ctx, 1, metric.WithAttributes(m.instanceAttrs()...))
}

// RBAC Apply methods

func (m *Metrics) RecordRBACApplyDuration(ctx context.Context, duration float64) {
	if m == nil || m.rbacApplyDuration == nil {
		return
	}
	m.rbacApplyDuration.Record(ctx, duration, metric.WithAttributes(m.instanceAttrs()...))
}

func (m *Metrics) IncRBACApplyTotal(ctx context.Context) {
	if m == nil || m.rbacApplyTotal == nil {
		return
	}
	m.rbacApplyTotal.Add(ctx, 1, metric.WithAttributes(m.instanceAttrs()...))
}

func (m *Metrics) IncRBACApplyErrors(ctx context.Context) {
	if m == nil || m.rbacApplyErrors == nil {
		return
	}
	m.rbacApplyErrors.Add(ctx, 1, metric.WithAttributes(m.instanceAttrs()...))
}

// Helm Install methods

func (m *Metrics) RecordHelmInstallDuration(ctx context.Context, duration float64) {
	if m == nil || m.helmInstallDuration == nil {
		return
	}
	m.helmInstallDuration.Record(ctx, duration, metric.WithAttributes(m.instanceAttrs()...))
}

func (m *Metrics) IncHelmInstallTotal(ctx context.Context) {
	if m == nil || m.helmInstallTotal == nil {
		return
	}
	m.helmInstallTotal.Add(ctx, 1, metric.WithAttributes(m.instanceAttrs()...))
}

func (m *Metrics) IncHelmInstallErrors(ctx context.Context) {
	if m == nil || m.helmInstallErrors == nil {
		return
	}
	m.helmInstallErrors.Add(ctx, 1, metric.WithAttributes(m.instanceAttrs()...))
}

// Helm Upgrade methods

func (m *Metrics) RecordHelmUpgradeDuration(ctx context.Context, duration float64) {
	if m == nil || m.helmUpgradeDuration == nil {
		return
	}
	m.helmUpgradeDuration.Record(ctx, duration, metric.WithAttributes(m.instanceAttrs()...))
}

func (m *Metrics) IncHelmUpgradeTotal(ctx context.Context) {
	if m == nil || m.helmUpgradeTotal == nil {
		return
	}
	m.helmUpgradeTotal.Add(ctx, 1, metric.WithAttributes(m.instanceAttrs()...))
}

func (m *Metrics) IncHelmUpgradeErrors(ctx context.Context) {
	if m == nil || m.helmUpgradeErrors == nil {
		return
	}
	m.helmUpgradeErrors.Add(ctx, 1, metric.WithAttributes(m.instanceAttrs()...))
}

// Helm Uninstall methods

func (m *Metrics) RecordHelmUninstallDuration(ctx context.Context, duration float64) {
	if m == nil || m.helmUninstallDuration == nil {
		return
	}
	m.helmUninstallDuration.Record(ctx, duration, metric.WithAttributes(m.instanceAttrs()...))
}

func (m *Metrics) IncHelmUninstallTotal(ctx context.Context) {
	if m == nil || m.helmUninstallTotal == nil {
		return
	}
	m.helmUninstallTotal.Add(ctx, 1, metric.WithAttributes(m.instanceAttrs()...))
}

func (m *Metrics) IncHelmUninstallErrors(ctx context.Context) {
	if m == nil || m.helmUninstallErrors == nil {
		return
	}
	m.helmUninstallErrors.Add(ctx, 1, metric.WithAttributes(m.instanceAttrs()...))
}

// Timer helper for measuring operation duration
type Timer struct {
	start time.Time
}

func NewTimer() *Timer {
	return &Timer{start: time.Now()}
}

func (t *Timer) Elapsed() float64 {
	return time.Since(t.start).Seconds()
}

// GetInstance returns the global metrics instance
func GetInstance() *Metrics {
	metricsMutex.RLock()
	defer metricsMutex.RUnlock()
	return instanceMetrics
}

// setInstance sets the global metrics instance (internal use only)
func setInstance(m *Metrics) {
	metricsMutex.Lock()
	defer metricsMutex.Unlock()
	instanceMetrics = m
}

// InitMetrics initializes the global metrics instance (for backward compatibility)
// This should be called from main with the deployment name
func InitMetrics(ctx context.Context, log logging.Logger, enabled bool, serviceName string, exportInterval time.Duration, deploymentName string) error {
	if !enabled {
		return nil
	}

	cfg := Config{
		Enabled:        true,
		ServiceName:    serviceName,
		ExportInterval: exportInterval,
		DeploymentName: deploymentName,
	}

	m, _, err := Setup(ctx, log, cfg)
	if err != nil {
		return err
	}

	setInstance(m)
	return nil
}

// DebugStatus logs the metrics status (for backward compatibility)
func DebugStatus() {
	metricsMutex.RLock()
	defer metricsMutex.RUnlock()

	initialized := instanceMetrics != nil
	slog.Info("CDC Metrics Status",
		"initialized", initialized,
		"deploymentName", func() string {
			if instanceMetrics != nil {
				return instanceMetrics.deploymentName
			}
			return ""
		}(),
	)
}

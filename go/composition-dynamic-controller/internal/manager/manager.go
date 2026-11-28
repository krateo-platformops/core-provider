// Package manager runs the composition controllers one cdc process hosts (core-provider
// docs/design/cdc-pooling.md, step 4: the multi-GVR manager, shipped hosting exactly one GVR).
//
// A controller is one Kind x version: one informer, one queue, one handler, one ownership Lease, one
// metrics view. Everything else is the PROCESS's and is shared by every member: the REST config,
// the event recorder, the chart getter, the REST mapper and its CRD watch, the saturation governor,
// the retry and resync settings, and the two servers a kubelet and a scraper talk to, metrics and
// health. Until this package, main.go wired all of it for one hard-coded GVR. Splitting the two is
// the refactor step 5's pool needs: a pool is this manager with more than one Member.
//
// With one member it behaves exactly as main.go did: same controller options, same probes, same exit
// on a lost Lease. The choices that only exist with several members are step 5's and are NOT made
// here: whether /readyz needs every member (it does, which for one member is the old behaviour) and
// whether a member losing its Lease restarts the process (it does, which kills every member).
package manager

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/krateo-platformops/composition-dynamic-controller/internal/auditrec"
	"github.com/krateo-platformops/composition-dynamic-controller/internal/composition"
	"github.com/krateo-platformops/composition-dynamic-controller/internal/metrics"
	"github.com/krateo-platformops/composition-dynamic-controller/internal/ownership"
	"github.com/krateo-platformops/composition-dynamic-controller/internal/tools/archive"
	"github.com/krateo-platformops/composition-dynamic-controller/internal/writtenby"
	"github.com/krateo-platformops/composition-dynamic-controller/pkg/meta"
	"github.com/krateo-platformops/plumbing/kubeutil/event"
	"github.com/krateo-platformops/plumbing/ptr"
	"github.com/krateo-platformops/unstructured-runtime/pkg/controller"
	"github.com/krateo-platformops/unstructured-runtime/pkg/controller/builder"
	ctrlevent "github.com/krateo-platformops/unstructured-runtime/pkg/controller/event"
	"github.com/krateo-platformops/unstructured-runtime/pkg/logging"
	metricsserver "github.com/krateo-platformops/unstructured-runtime/pkg/metrics/server"
	"github.com/krateo-platformops/unstructured-runtime/pkg/pluralizer"
	"github.com/krateo-platformops/unstructured-runtime/pkg/ratelimit"
	"github.com/krateo-platformops/unstructured-runtime/pkg/telemetry"
	"github.com/krateo-platformops/unstructured-runtime/pkg/tools/statusprojection"
	"github.com/krateo-platformops/unstructured-runtime/pkg/workqueue"
	otellog "go.opentelemetry.io/otel/log"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/client-go/rest"
)

// Process is what every member shares.
type Process struct {
	Config            *rest.Config
	Log               logging.Logger
	ProviderName      string
	Pluralizer        pluralizer.PluralizerInterface
	PackageInfoGetter archive.Getter
	EventRecorder     event.APIRecorder
	Mapper            apimeta.RESTMapper
	ChartInspectorURL string
	SAName            string
	SANamespace       string
	SafeReleaseName   bool
	// AuditLogs is the audit records' own OTLP provider (nil: none), shared by every member's
	// recorder.
	AuditLogs otellog.LoggerProvider
	// TelemetryMetrics is the runtime's root metrics view (nil: metrics off); each member records
	// through its own view of it.
	TelemetryMetrics *telemetry.Metrics
	// Governor slows every member when the process saturates (nil: off). It is the PROCESS's: a
	// pool's workers share one CPU.
	Governor ratelimit.Governor

	Namespace               string
	ResyncInterval          time.Duration
	MinErrorRetryInterval   time.Duration
	MaxErrorRetryInterval   time.Duration
	MaxErrorRetries         int
	GracefulShutdownTimeout time.Duration
	Workers                 int

	// PodNamespace and PodName are where and as whom each member takes its ownership Lease.
	PodNamespace string
	PodName      string
	// OwnershipTimings are every member's ownership Lease timings (ownership.TimingsFromEnv, #660):
	// one set for the process, so a pool's members renew alike. Required for a member with a Lease.
	OwnershipTimings ownership.Timings

	// MetricsBindAddress serves the process's Prometheus registry ("" or "0": off). One server for
	// the process: each member's controller would otherwise start its own on the same port.
	MetricsBindAddress string

	// WrittenBy is this binary's build stamp (core-provider#669), which every member's handler puts
	// on each composition status it writes.
	WrittenBy writtenby.Stamp
}

// Member is one controller the process hosts: one Kind x version, and what belongs to it alone.
type Member struct {
	GVR schema.GroupVersionResource
	// LeaseName is the ownership Lease this member must hold to reconcile ("": none).
	LeaseName string
	// StatusDataTemplate and APIResolver are the member's CompositionDefinition's status
	// projections and apiRef resolver (nil: none).
	StatusDataTemplate []statusprojection.Mapping
	APIResolver        composition.APIResolver
	// Workers and ResyncInterval override the process's for this member; zero means the process
	// value. A dedicated controller leaves them zero (its flags set the process's); a pool member
	// takes them from its own configuration (docs/design/cdc-pooling.md §5c).
	Workers        int
	ResyncInterval time.Duration
}

// Built is a member's controller, ready to run.
type Built struct {
	Member     Member
	Controller *controller.Controller
}

// Build assembles every member's controller. Nothing runs yet, so a member that cannot be built
// fails the process before any member reconciles.
func Build(ctx context.Context, p Process, members []Member) ([]Built, error) {
	if len(members) == 0 {
		return nil, errors.New("manager: no member to host")
	}
	seen := map[schema.GroupVersionResource]bool{}
	out := make([]Built, 0, len(members))
	for _, m := range members {
		if seen[m.GVR] {
			// Two members for one Kind x version in one process would reconcile it twice, which
			// is exactly what the ownership Lease exists to prevent across processes.
			return nil, fmt.Errorf("manager: %s is hosted twice", m.GVR)
		}
		seen[m.GVR] = true
		c, err := p.build(ctx, m)
		if err != nil {
			return nil, fmt.Errorf("manager: building the controller for %s: %w", m.GVR, err)
		}
		out = append(out, Built{Member: m, Controller: c})
	}
	return out, nil
}

// handlerOptions is what member m's handler is built from: the process's shared parts, m's own, and
// m's audit recorder and metrics view.
func (p Process) handlerOptions(m Member, audit *auditrec.Recorder, mx *metrics.Metrics) *composition.HandlerOptions {
	return &composition.HandlerOptions{
		Kubeconfig:         p.Config,
		Pluralizer:         p.Pluralizer,
		PackageInfoGetter:  p.PackageInfoGetter,
		EventRecorder:      p.EventRecorder,
		ChartInspectorUrl:  p.ChartInspectorURL,
		SaName:             p.SAName,
		SaNamespace:        p.SANamespace,
		SafeReleaseName:    p.SafeReleaseName,
		Mapper:             p.Mapper,
		StatusDataTemplate: m.StatusDataTemplate,
		APIResolver:        m.APIResolver,
		Audit:              audit,
		Metrics:            mx,
		WrittenBy:          p.WrittenBy,
	}
}

// build assembles one member's controller: everything main.go used to wire for the single GVR.
func (p Process) build(ctx context.Context, m Member) (*controller.Controller, error) {
	log := p.Log
	controllerAttr := metrics.ControllerAttr(m.GVR.Group, m.GVR.Version, m.GVR.Resource)
	if log != nil {
		log = log.WithValues(string(controllerAttr.Key), controllerAttr.Value.AsString())
	}

	// EXACT equality, the constraint the label exists for: an instance whose label does not match
	// this member's version is never delivered to it (see core-provider internal/tools/policy).
	labelreq, err := labels.NewRequirement(meta.CompositionVersionLabel, selection.Equals, []string{m.GVR.Version})
	if err != nil {
		return nil, fmt.Errorf("creating the label requirement: %w", err)
	}

	// core-provider#423: one audit record per child each helm operation writes, as this member's
	// ServiceAccount.
	audit, err := auditrec.New(p.Config, m.GVR.Resource, p.SANamespace, p.SAName, p.AuditLogs)
	if err != nil {
		return nil, fmt.Errorf("creating the audit recorder: %w", err)
	}

	handler := composition.NewHandler(p.handlerOptions(m, audit, metrics.GetInstance().With(controllerAttr)))

	opts := []builder.FuncOption{
		builder.WithLogger(log),
		builder.WithNamespace(p.Namespace),
		builder.WithResyncInterval(m.resync(p)),
		builder.WithGlobalRateLimiter(workqueue.NewExponentialTimedFailureRateLimiter[any](p.MinErrorRetryInterval, p.MaxErrorRetryInterval)),
		builder.WithMaxRetries(p.MaxErrorRetries),
		builder.WithGracefulShutdownTimeout(p.GracefulShutdownTimeout),
		builder.WithListWatcher(controller.ListWatcherConfiguration{
			LabelSelector: ptr.To(labels.NewSelector().Add(*labelreq).String()),
		}),
		builder.WithActionEvent(ctrlevent.CRUpdated, ctrlevent.Observe),
		// Pool step 3: name this member on every metric point and reconcile root.
		builder.WithTelemetryMetrics(p.TelemetryMetrics.With(controllerAttr)),
		builder.WithSpanAttributes(controllerAttr),
	}
	// Pool step 2: reconcile only while holding this Kind x version's Lease.
	// The timings are the PROCESS's: every member of a pool renews on the same ones (#660).
	ownershipOpts, err := ownership.Option(m.LeaseName, p.PodNamespace, p.PodName, p.OwnershipTimings)
	if err != nil {
		return nil, fmt.Errorf("configuring the ownership lease: %w", err)
	}
	opts = append(opts, ownershipOpts...)
	if p.Governor != nil {
		opts = append(opts, builder.WithRateLimitGovernor(p.Governor))
	}

	c, err := builder.Build(ctx, builder.Configuration{Config: p.Config, GVR: m.GVR, ProviderName: p.ProviderName}, opts...)
	if err != nil {
		return nil, err
	}
	c.SetExternalClient(handler)
	return c, nil
}

// Readiness answers /readyz for the process: ready only when EVERY member is (its informer has
// listed, and its Lease, if any, is readable). For one member that is exactly the member's own
// readiness, which is what the probe answered before.
type Readiness []Built

// ReadyzCheck implements health.Readier.
func (r Readiness) ReadyzCheck(req *http.Request) error {
	for _, b := range r {
		if err := b.Controller.ReadyzCheck(req); err != nil {
			return fmt.Errorf("%s: %w", b.Member.GVR, err)
		}
	}
	return nil
}

// Run starts the process's metrics server, then every member, and returns when they have all
// stopped. A member that stops with an error (ownership.Lost included) is returned, which makes the
// caller exit: with one member that is the old behaviour, and with several it is the provisional
// one (see the package comment).
func Run(ctx context.Context, p Process, built []Built) error {
	if addr := p.MetricsBindAddress; addr != "" && addr != "0" {
		srv, err := metricsserver.NewServer(metricsserver.Options{BindAddress: addr}, p.Config, http.DefaultClient)
		if err != nil {
			return fmt.Errorf("manager: creating the metrics server: %w", err)
		}
		if srv != nil {
			go func() {
				if err := srv.WithLogger(p.Log).Start(ctx); err != nil && p.Log != nil {
					p.Log.Error(err, "metrics server failed")
				}
			}()
		}
	}

	// The first member to fail stops the others (provisional for several members, see the package
	// comment): the caller exits, and a member left reconciling would outlive the process's intent.
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	errs := make(chan error, len(built))
	for _, b := range built {
		go func(b Built) {
			err := b.Controller.Run(runCtx, b.Member.workers(p))
			if err != nil {
				err = fmt.Errorf("%s: %w", b.Member.GVR, err)
			}
			errs <- err
		}(b)
	}
	var first error
	for range built {
		if err := <-errs; err != nil && first == nil {
			first = err
			stop()
		}
	}
	return first
}

// workers is the member's worker count: its own when set, else the process's.
func (m Member) workers(p Process) int {
	if m.Workers > 0 {
		return m.Workers
	}
	return p.Workers
}

// resync is the member's resync interval: its own when set, else the process's.
func (m Member) resync(p Process) time.Duration {
	if m.ResyncInterval > 0 {
		return m.ResyncInterval
	}
	return p.ResyncInterval
}

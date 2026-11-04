// Command testprovider is a provider built on the REAL provider-runtime, whose Observe can be made
// to fail per object. It exists for one purpose: to produce a genuine Ready=Unknown/NotObserved so
// that core-provider#310 — the cdc treating NotObserved as unevaluated rather than converging — can
// be verified deterministically.
//
// # Why this exists rather than a status patch
//
// installer rolled alert-provider 0.2.60 (the first v1.8.2-built provider) on demo: 27 Alert CRs
// under 4 parent compositions, all healthy throughout, and #310 was NEVER CAUGHT. That is
// structural, not bad luck. A child reads Ready=Unknown/NotObserved only when its provider's
// Observe FAILS, and a healthy rollout fails nothing. So per-rollout observation cannot verify
// #310, and five adopters each reporting NEVER CAUGHT would accumulate into something that reads
// like a track record while testing nothing.
//
// The alternative — patching Unknown/NotObserved into a child's status directly — would verify that
// the cdc maps a string, not that provider-runtime produces that string. Those are different
// claims, and the one worth testing is the pair. So the condition here is written by
// provider-runtime's own error path (reconciler.go:262-266), from a real reconcile, against a real
// apiserver.
//
// # Why a separate module and a separate process
//
// A Go module cannot import another module's internal/ packages, and the cdc's rollup lives in
// internal/composition. So the test itself must live inside the cdc module — which means this
// provider cannot be a library the test imports, or provider-runtime would enter the cdc's
// dependency graph, its release builds and its vulnerability surface for a component that imports
// it zero times in production.
//
// Running it as a subprocess keeps that graph clean AND is closer to production than an in-process
// reconciler would be: a separate process, its own manager, its own cache and watches, talking to
// the apiserver over the network.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/krateo-platformops/plumbing/kubeutil/event"
	"github.com/krateo-platformops/plumbing/kubeutil/eventrecorder"
	prv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	"github.com/krateo-platformops/provider-runtime/pkg/reconciler"
	"github.com/krateo-platformops/provider-runtime/pkg/resource"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/krateo-platformops/core-provider/go/testprovider/apis/v1alpha1"
)

func main() {
	var pollInterval time.Duration
	flag.DurationVar(&pollInterval, "poll", 2*time.Second, "reconcile poll interval")
	flag.Parse()

	if err := run(pollInterval); err != nil {
		fmt.Fprintf(os.Stderr, "testprovider: %v\n", err)
		os.Exit(1)
	}
}

func run(poll time.Duration) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return fmt.Errorf("adding TestChild to the scheme: %w", err)
	}

	cfg, err := config.GetConfig()
	if err != nil {
		return fmt.Errorf("loading kubeconfig (KUBECONFIG must point at the test apiserver): %w", err)
	}

	mgr, err := manager.New(cfg, manager.Options{
		Scheme: scheme,
		// Metrics served on no port: two of these can run concurrently in CI and a fixed port
		// would make them collide, which would surface as a test failure with nothing to do with
		// the thing under test.
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		return fmt.Errorf("creating the manager: %w", err)
	}

	log := logging.NewLogrLogger(zap.New(zap.UseDevMode(true)))

	// Built the same way the cdc builds its own (composition-dynamic-controller/main.go:256), so
	// the events this emits are shaped like production's rather than like a test's.
	rec, err := eventrecorder.Create(ctx, cfg, "testprovider", nil)
	if err != nil {
		return fmt.Errorf("creating the event recorder: %w", err)
	}

	// THE REAL RECONCILER. Everything that makes this verification meaningful is in this call:
	// provider-runtime decides, on its own error path, that an Observe which returned an error
	// without setting Ready leaves Ready at NotObserved. Nothing here writes that condition.
	r := reconciler.NewReconciler(mgr,
		resource.ManagedKind(v1alpha1.TestChildGroupVersionKind),
		reconciler.WithExternalConnecter(&connector{}),
		reconciler.WithPollInterval(poll),
		reconciler.WithLogger(log),
		// THE API RECORDER, not a nop, and that choice is load-bearing. provider-runtime emits
		// ReasonReadyNotSetAfterObserve when an Observe errors without setting Ready — the event is
		// exported "so providers' tests can assert on the event" (reconciler.go:63-65). Sending it
		// to the apiserver gives the test a SECOND, independent witness that the runtime took the
		// NotObserved path, beside the condition itself. A nop recorder would discard the only
		// evidence that is not the thing being measured.
		reconciler.WithRecorder(event.NewAPIRecorder(rec)),
	)

	if err := ctrl.NewControllerManagedBy(mgr).
		Named("testchild").
		For(&v1alpha1.TestChild{}).
		Complete(r); err != nil {
		return fmt.Errorf("building the controller: %w", err)
	}

	// Printed so the test can wait for readiness on a line rather than on a sleep: a sleep would
	// make the test pass or fail on CI load.
	fmt.Println("testprovider: starting manager")
	return mgr.Start(ctx)
}

type connector struct{}

// Connect returns the client. There is no external system to connect to — the "external resource"
// is the CR's own spec, which is what makes the failure switch exact rather than simulated.
func (c *connector) Connect(_ context.Context, _ resource.Managed) (reconciler.ExternalClient, error) {
	return &client{}, nil
}

type client struct{}

// errObserve is the failure. It is deliberately a plain error with NO ReadyAssessed wrapper:
// provider-runtime leaves Ready at NotObserved precisely when an Observe errors WITHOUT having
// assessed readiness (reconciler.go:262-266). Wrapping it with ReadyAssessed would opt out of the
// very behaviour under test, and a future reader who "fixes" this error by wrapping it would make
// the whole suite vacuous while every assertion still passed.
var errObserve = errors.New("simulated Observe failure: the external system is unreachable")

func (c *client) Observe(_ context.Context, mg resource.Managed) (reconciler.ExternalObservation, error) {
	cr, ok := mg.(*v1alpha1.TestChild)
	if !ok {
		return reconciler.ExternalObservation{}, fmt.Errorf("unexpected type %T", mg)
	}

	if cr.Spec.FailObserve {
		// No SetConditions call before returning. That omission IS the test fixture.
		return reconciler.ExternalObservation{}, errObserve
	}

	if cr.Spec.Unready {
		// A SUCCESSFUL Observe that reports the resource as still converging. This is the sibling
		// state arm (c) needs, and it has to come from a success: a failure would produce
		// NotObserved again and collapse the two states the arm exists to tell apart.
		cr.SetConditions(prv1.Creating())
		return reconciler.ExternalObservation{ResourceExists: true, ResourceUpToDate: true}, nil
	}

	cr.SetConditions(prv1.Available())
	return reconciler.ExternalObservation{ResourceExists: true, ResourceUpToDate: true}, nil
}

// Create/Update/Delete exist to satisfy the interface. Observe always reports ResourceExists: true
// for a non-failing child, so the runtime never routes here — and if a future change makes it do
// so, returning an error is louder than returning nil.
func (c *client) Create(_ context.Context, _ resource.Managed) error {
	return errors.New("Create should not be reached: Observe reports the resource as existing")
}

func (c *client) Update(_ context.Context, _ resource.Managed) error {
	return errors.New("Update should not be reached: Observe reports the resource as up to date")
}

func (c *client) Delete(_ context.Context, _ resource.Managed) error { return nil }

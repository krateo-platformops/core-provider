package health

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/krateo-platformops/plumbing/kubeutil/event"
	"github.com/krateo-platformops/plumbing/shortid"
	"github.com/krateo-platformops/unstructured-runtime/pkg/controller"
	"github.com/krateo-platformops/unstructured-runtime/pkg/logging"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/util/workqueue"
)

// get fetches path from the probe server and returns the status code and body.
func get(t *testing.T, base, path string) (int, string) {
	t.Helper()
	resp, err := http.Get(base + path)
	require.NoError(t, err)
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(b)
}

// checkerFunc adapts a test's checker to the Readier that Serve takes.
type checkerFunc Checker

func (f checkerFunc) ReadyzCheck(r *http.Request) error { return f(r) }

func serve(t *testing.T, r Readier) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	addr, err := Serve(ctx, "127.0.0.1:0", r, func(err error) { t.Errorf("probe server: %v", err) })
	require.NoError(t, err)
	return "http://" + addr.String()
}

// The two endpoints answer different questions, so they are asserted against a checker in BOTH
// states: /readyz must follow it, /healthz must not. A /healthz wired to the readiness check would
// restart-loop a controller whose CRD version is briefly unserved (#333) and kill it mid-helm
// operation (#57); it passes any test that only looks at the ready state.
func TestHandler_ReadyzFollowsTheCheck_HealthzDoesNot(t *testing.T) {
	var notReady atomic.Pointer[error]
	blind := errors.New("informer for composition.krateo.io/v1-2-3, Resource=widgets has not completed its first successful list")
	notReady.Store(&blind)
	var readyCalls atomic.Int32
	base := serve(t, checkerFunc(func(*http.Request) error {
		readyCalls.Add(1)
		if p := notReady.Load(); p != nil {
			return *p
		}
		return nil
	}))

	code, body := get(t, base, ReadinessPath)
	require.Equal(t, http.StatusServiceUnavailable, code, "readiness answered ready while the check failed")
	require.Contains(t, body, blind.Error(), "the probe body must carry the reason, so `kubectl describe pod` shows it")
	require.Equal(t, int32(1), readyCalls.Load(), "/readyz did not consult the check")

	code, body = get(t, base, LivenessPath)
	require.Equal(t, http.StatusOK, code, "liveness failed because readiness did: a blind controller would be restart-looped")
	require.Equal(t, "ok\n", body)
	require.Equal(t, int32(1), readyCalls.Load(), "/healthz consulted the readiness check")

	notReady.Store(nil)

	code, body = get(t, base, ReadinessPath)
	require.Equal(t, http.StatusOK, code, "readiness stayed not-ready after the check passed")
	require.Equal(t, "ok\n", body)
	code, _ = get(t, base, LivenessPath)
	require.Equal(t, http.StatusOK, code)
}

func TestServe_ABindFailureIsReturned(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ok := checkerFunc(func(*http.Request) error { return nil })
	addr, err := Serve(ctx, "127.0.0.1:0", ok, nil)
	require.NoError(t, err)
	_, err = Serve(ctx, addr.String(), ok, nil)
	require.Error(t, err, "a second bind of the same port must fail at Serve, not in a goroutine that only logs")
}

func TestServe_RequiresAChecker(t *testing.T) {
	_, err := Serve(context.Background(), "127.0.0.1:0", nil, nil)
	require.Error(t, err)
}

type nopRecorder struct{}

func (nopRecorder) Event(runtime.Object, event.Event) {}

// nopPluralizer satisfies the runtime's required option; nothing in a list/readiness path calls it.
type nopPluralizer struct{}

func (nopPluralizer) GVKtoGVR(gvk schema.GroupVersionKind) (schema.GroupVersionResource, error) {
	return schema.GroupVersionResource{}, errors.New("not used")
}

type nopClient struct{}

func (nopClient) Observe(context.Context, *unstructured.Unstructured) (controller.ExternalObservation, error) {
	return controller.ExternalObservation{ResourceExists: true, ResourceUpToDate: true}, nil
}
func (nopClient) Create(context.Context, *unstructured.Unstructured) error { return nil }
func (nopClient) Update(context.Context, *unstructured.Unstructured) error { return nil }
func (nopClient) Delete(context.Context, *unstructured.Unstructured) error { return nil }

// TestProbes_AgainstTheRuntimeController drives the probes from a REAL unstructured-runtime
// controller, the same Controller.ReadyzCheck main.go mounts, with a fake API server whose LIST fails
// the way #333 measured (404, "the server could not find the requested resource") and then succeeds.
// It asserts the transition over HTTP: 503 on /readyz after counted failed lists, 200 after a
// successful one, and 200 on /healthz throughout.
func TestProbes_AgainstTheRuntimeController(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v0-13-2", Resource: "kubernetesclusters"}
	listGVK := schema.GroupVersionKind{Group: gvr.Group, Version: gvr.Version, Kind: "KubernetesClusterList"}
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})
	fc := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{gvr: listGVK.Kind})

	var failing atomic.Bool
	var failures, successes atomic.Int32
	failing.Store(true)
	fc.PrependReactor("list", gvr.Resource, func(clienttesting.Action) (bool, runtime.Object, error) {
		if failing.Load() {
			failures.Add(1)
			return true, nil, apierrors.NewGenericServerResponse(http.StatusNotFound, "list", gvr.GroupResource(), "", "", 0, true)
		}
		successes.Add(1)
		return false, nil, nil
	})

	sid, err := shortid.New(1, shortid.DefaultABC, 2342)
	require.NoError(t, err)
	noDrain := time.Duration(0)
	ctrl, err := controller.New(sid, controller.Options{
		Client:                  fc,
		GVR:                     gvr,
		ResyncInterval:          time.Minute,
		Recorder:                nopRecorder{},
		ThrottledRecorder:       nopRecorder{},
		Logger:                  logging.NewNopLogger(),
		Pluralizer:              nopPluralizer{},
		GlobalRateLimiter:       workqueue.DefaultTypedControllerRateLimiter[any](),
		GracefulShutdownTimeout: &noDrain,
	})
	require.NoError(t, err)
	ctrl.SetExternalClient(nopClient{})

	// Mounted before Run, as main.go does, from the controller itself.
	base := serve(t, ctrl)
	code, _ := get(t, base, ReadinessPath)
	require.Equal(t, http.StatusServiceUnavailable, code, "ready before the controller ran")

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- ctrl.Run(ctx, 1) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-runDone:
		case <-time.After(10 * time.Second):
			t.Error("Run did not return after cancel")
		}
	})

	require.Eventually(t, func() bool { return failures.Load() >= 2 }, 15*time.Second, 10*time.Millisecond,
		"the informer never attempted the list")
	code, body := get(t, base, ReadinessPath)
	require.Equal(t, http.StatusServiceUnavailable, code, "ready while every list has failed: this is #333")
	require.True(t, strings.Contains(body, gvr.Resource), "the reason must name the resource: %q", body)
	code, _ = get(t, base, LivenessPath)
	require.Equal(t, http.StatusOK, code, "a failing list made the controller unlive")

	failing.Store(false)

	require.Eventually(t, func() bool {
		code, _ := get(t, base, ReadinessPath)
		return code == http.StatusOK
	}, 15*time.Second, 20*time.Millisecond, "still not ready after the list succeeded")
	require.GreaterOrEqual(t, successes.Load(), int32(1))
	code, _ = get(t, base, LivenessPath)
	require.Equal(t, http.StatusOK, code)
}

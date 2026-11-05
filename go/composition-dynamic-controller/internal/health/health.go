// Package health serves the composition-dynamic-controller's kubelet probe endpoints.
//
// Two endpoints, deliberately NOT of equal strength (krateo-platformops/core-provider#333):
//
//   - /readyz answers from the controller: not ready until its informer has completed the first
//     successful list of the resource it reconciles. A controller whose CRD version is no longer
//     served lists forever without success and stays not-ready, instead of reporting Ready because
//     its process started.
//   - /healthz answers from the process alone. It never consults the readiness check, because a
//     failing list is a real and recoverable state (a CRD version briefly unserved mid-bump) that a
//     restart does not repair, and a liveness kill in the middle of a helm operation leaves the
//     release locked (#57). Restart-looping through an unserved version is worse than reporting
//     not-ready through it.
package health

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

const (
	// LivenessPath and ReadinessPath are the probe paths the per-version Deployment template
	// (helm/core-provider/assets/cdc/deployment.yaml) points the kubelet at.
	LivenessPath  = "/healthz"
	ReadinessPath = "/readyz"
)

// Checker reports nil when ready and an error saying why not otherwise. It is the shape of
// unstructured-runtime's Controller.ReadyzCheck (and of controller-runtime's healthz.Checker).
type Checker func(*http.Request) error

// Readier is what Serve takes: unstructured-runtime's *controller.Controller satisfies it. Serve
// takes the controller rather than a bare Checker so main.go passes the controller itself and has no
// readiness function to choose; the decision lives in the runtime, where it is tested.
type Readier interface {
	ReadyzCheck(*http.Request) error
}

// NewHandler returns the probe mux. ready is consulted by /readyz only.
func NewHandler(ready Checker) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(LivenessPath, func(w http.ResponseWriter, _ *http.Request) {
		// Answering at all is the liveness signal: the process is up and serving.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc(ReadinessPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if err := ready(r); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = fmt.Fprintf(w, "not ready: %v\n", err)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintln(w, "ok")
	})
	return mux
}

// Serve binds addr and serves NewHandler(r.ReadyzCheck) on it until ctx is done. The bind happens before it
// returns, so a port that cannot be bound is an error the caller can exit on, not a goroutine that
// logs and leaves every probe failing. It returns the bound address (useful with ":0").
//
// The server is shut down on ctx cancellation; callers that want the probes to keep answering
// through a graceful drain pass a context that outlives the signal.
func Serve(ctx context.Context, addr string, r Readier, onError func(error)) (net.Addr, error) {
	if r == nil {
		return nil, errors.New("health: a readiness source is required")
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("health: binding %q: %w", addr, err)
	}
	srv := &http.Server{
		Handler:           NewHandler(r.ReadyzCheck),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) && onError != nil {
			onError(err)
		}
	}()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	return ln.Addr(), nil
}

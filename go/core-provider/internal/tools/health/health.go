// Package health holds core-provider's kubelet probe checks (#333).
//
// Liveness is controller-runtime's healthz.Ping: the manager's probe server answering at all. It
// deliberately depends on nothing core-provider does, so a slow or failing reconcile never restarts
// the engine. Readiness is CacheSynced below. Register wires both onto the manager, so which check
// backs which endpoint is decided here, where it is tested, and not in main.go.
package health

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
)

// ReadyzWait bounds one readiness probe's wait on the cache sync; keep it below the chart's
// readinessProbe.timeoutSeconds (5).
const ReadyzWait = time.Second

// Manager is the part of controller-runtime's manager.Manager that Register uses.
type Manager interface {
	AddHealthzCheck(name string, check healthz.Checker) error
	AddReadyzCheck(name string, check healthz.Checker) error
	GetCache() cache.Cache
}

// Register adds the liveness and readiness checks to mgr: liveness answers from the probe server
// alone (healthz.Ping), readiness from the cache (CacheSynced).
func Register(mgr Manager) error {
	if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
		return fmt.Errorf("adding liveness check: %w", err)
	}
	if err := mgr.AddReadyzCheck("informers-synced", CacheSynced(mgr.GetCache(), ReadyzWait)); err != nil {
		return fmt.Errorf("adding readiness check: %w", err)
	}
	return nil
}

// CacheSyncer is the part of controller-runtime's cache.Cache that CacheSynced reads.
type CacheSyncer interface {
	WaitForCacheSync(ctx context.Context) bool
}

// ErrCachesNotSynced is the readiness failure: the manager's informers have not all completed their
// first successful list.
var ErrCachesNotSynced = errors.New("informer caches have not completed their first successful list")

// CacheSynced is the readiness check: ready once every informer the manager's cache has started has
// completed its first successful list. That is the engine's version of the one fact #333 asks a
// controller to report: whether it can SEE what it reconciles, as opposed to whether its process
// started. An engine that cannot list CompositionDefinitions reconciles nothing, yet a TCP probe on
// the always-bound metrics port reported it Ready.
//
// wait bounds how long a single probe request may block on the sync. controller-runtime's
// WaitForCacheSync polls until synced or until the context is done, so without the bound a probe
// against an unsynced cache would hang until the kubelet's own timeout, and the kubelet would log a
// timeout instead of this error. Keep it below the probe's timeoutSeconds.
//
// Like the informer state it reads, the answer latches: once synced it stays ready. And it covers
// every informer the cache has started, including any started on demand by a cached read of a new
// type, so an informer that can never list (for example, missing list RBAC for a kind) holds the
// engine not-ready. That is the intended reading: such an engine is blind to that kind.
func CacheSynced(c CacheSyncer, wait time.Duration) healthz.Checker {
	return func(req *http.Request) error {
		ctx, cancel := context.WithTimeout(req.Context(), wait)
		defer cancel()
		if !c.WaitForCacheSync(ctx) {
			return ErrCachesNotSynced
		}
		return nil
	}
}

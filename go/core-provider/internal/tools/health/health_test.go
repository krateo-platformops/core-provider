package health

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
)

// fakeCache answers WaitForCacheSync the way controller-runtime's informer cache does: it blocks
// until the caches are synced or the context is done, and reports which happened.
type fakeCache struct {
	synced atomic.Bool
	calls  atomic.Int32
}

func (f *fakeCache) WaitForCacheSync(ctx context.Context) bool {
	f.calls.Add(1)
	for {
		if f.synced.Load() {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func probe(t *testing.T, h http.Handler, path string) int {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code
}

// TestCacheSynced_NotReadyBeforeSync_ReadyAfter asserts the transition on one cache, as #333 asks:
// an always-ready check passes an after-only test, a never-ready one passes a before-only test.
func TestCacheSynced_NotReadyBeforeSync_ReadyAfter(t *testing.T) {
	c := &fakeCache{}
	check := CacheSynced(c, 50*time.Millisecond)

	err := check(httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if !errors.Is(err, ErrCachesNotSynced) {
		t.Fatalf("before sync: err = %v, want ErrCachesNotSynced", err)
	}
	if c.calls.Load() != 1 {
		t.Fatalf("the check did not consult the cache (calls=%d)", c.calls.Load())
	}

	c.synced.Store(true)
	if err := check(httptest.NewRequest(http.MethodGet, "/readyz", nil)); err != nil {
		t.Fatalf("after sync: err = %v, want nil", err)
	}
}

// TestCacheSynced_BoundsTheWait: an unsynced cache must answer not-ready within the bound, not hold
// the probe request open until the kubelet times it out.
func TestCacheSynced_BoundsTheWait(t *testing.T) {
	check := CacheSynced(&fakeCache{}, 50*time.Millisecond)
	done := make(chan error, 1)
	// The request's own context never ends, as a kubelet probe's does not until its timeout, so only
	// the bound can end the wait.
	go func() { done <- check(httptest.NewRequest(http.MethodGet, "/readyz", nil)) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("ready while never synced")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the check is still blocked on an unsynced cache after 2s; the bound is 50ms")
	}
}

// TestProbes_ReadyzFollowsTheCache_HealthzDoesNot mounts the checks the way main.go registers them
// on the manager (a healthz.Handler per endpoint) and asserts that liveness stays OK while
// readiness is failing: an engine that cannot list must report not-ready, not be restarted.
func TestProbes_ReadyzFollowsTheCache_HealthzDoesNot(t *testing.T) {
	c := &fakeCache{}
	// controller-runtime's manager mounts each handler under its endpoint with http.StripPrefix.
	readyz := http.StripPrefix("/readyz", &healthz.Handler{Checks: map[string]healthz.Checker{"informers-synced": CacheSynced(c, 20*time.Millisecond)}})
	livez := http.StripPrefix("/healthz", &healthz.Handler{Checks: map[string]healthz.Checker{"ping": healthz.Ping}})

	if got := probe(t, readyz, "/readyz"); got != http.StatusInternalServerError {
		t.Fatalf("/readyz before sync = %d, want 500 (healthz.Handler's failure code)", got)
	}
	if got := probe(t, livez, "/healthz"); got != http.StatusOK {
		t.Fatalf("/healthz while unsynced = %d, want 200", got)
	}
	c.synced.Store(true)
	if got := probe(t, readyz, "/readyz"); got != http.StatusOK {
		t.Fatalf("/readyz after sync = %d, want 200", got)
	}
}

// fakeManager records what Register adds. Its cache is a fakeCache behind the cache.Cache interface;
// only WaitForCacheSync is ever called, and calling anything else panics on the nil embedded value.
type fakeManager struct {
	cache   *fakeCache
	healthz map[string]healthz.Checker
	readyz  map[string]healthz.Checker
}

type syncOnlyCache struct {
	cache.Cache
	*fakeCache
}

func (s syncOnlyCache) WaitForCacheSync(ctx context.Context) bool {
	return s.fakeCache.WaitForCacheSync(ctx)
}

func (m *fakeManager) AddHealthzCheck(name string, c healthz.Checker) error {
	m.healthz[name] = c
	return nil
}
func (m *fakeManager) AddReadyzCheck(name string, c healthz.Checker) error {
	m.readyz[name] = c
	return nil
}
func (m *fakeManager) GetCache() cache.Cache { return syncOnlyCache{fakeCache: m.cache} }

// TestRegister_LivenessNeverConsultsTheCache_ReadinessDoes is the wiring main.go delegates: with the
// cache unsynced, every liveness check passes WITHOUT touching the cache, and the readiness checks
// fail; once synced they pass. A liveness check tied to the cache would restart an engine that is
// merely blind, which #333 rules out.
func TestRegister_LivenessNeverConsultsTheCache_ReadinessDoes(t *testing.T) {
	m := &fakeManager{cache: &fakeCache{}, healthz: map[string]healthz.Checker{}, readyz: map[string]healthz.Checker{}}
	if err := Register(m); err != nil {
		t.Fatal(err)
	}
	if len(m.healthz) == 0 || len(m.readyz) == 0 {
		t.Fatalf("Register added %d liveness and %d readiness checks; want at least one of each", len(m.healthz), len(m.readyz))
	}
	req := func() *http.Request { return httptest.NewRequest(http.MethodGet, "/", nil) }

	for name, c := range m.healthz {
		if err := c(req()); err != nil {
			t.Errorf("liveness check %q failed while the cache is unsynced: %v", name, err)
		}
	}
	if n := m.cache.calls.Load(); n != 0 {
		t.Fatalf("liveness consulted the cache %d times", n)
	}
	failing := 0
	for _, c := range m.readyz {
		if c(req()) != nil {
			failing++
		}
	}
	if failing == 0 {
		t.Fatal("no readiness check failed while the cache is unsynced")
	}

	m.cache.synced.Store(true)
	for name, c := range m.readyz {
		if err := c(req()); err != nil {
			t.Errorf("readiness check %q still failing after sync: %v", name, err)
		}
	}
}

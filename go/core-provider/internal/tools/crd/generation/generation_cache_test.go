package generation

import (
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

func cacheTestSchema() []byte {
	return []byte(`{"type":"object","properties":{"spec":{"type":"object","properties":{"foo":{"type":"string"}}}}}`)
}

// GenerateCRD shells out to the Go toolchain (crdgen.Generate). The cold call runs it; a second
// call with the same (schema, gvk) must be served from crdGenCache instead of re-running the
// toolchain — otherwise Observe pays a compilation on every reconcile.
//
// This asserts the toolchain INVOCATION COUNT, not elapsed time. The earlier version timed both
// calls and required the second to be at least 4x faster, which was flaky and, worse, unsound:
// the property under test is "the toolchain did not run again", and duration is only a proxy for
// it. The proxy breaks whenever the toolchain's own build cache is already warm — an observed
// failure had cold=16ms warm=117ms, i.e. a "cold" call that never paid for a compilation at all,
// so the comparison was measuring GC and scheduler noise and flipped at random.
//
// Counting is exact, machine-independent, and tests the thing the cache exists to do.
func TestGenerateCRD_CacheAvoidsToolchainReinvocation(t *testing.T) {
	spec := cacheTestSchema()
	gvk := schema.GroupVersionKind{Group: "cache.example.org", Version: "v1", Kind: "Cachewidget"}

	before := crdGenRuns.Load()
	crd1, err := GenerateCRD(spec, gvk)
	if err != nil {
		t.Fatalf("first GenerateCRD: %v", err)
	}
	afterCold := crdGenRuns.Load()

	// POSITIVE CONTROL, and the reason the old test could pass while proving nothing. If this
	// (schema, gvk) was already cached, BOTH calls are hits, and an assertion that the second did
	// not run the toolchain is vacuously true. Establish that the first call really was a miss
	// before concluding anything from the second.
	if afterCold != before+1 {
		t.Fatalf("the first GenerateCRD did not invoke the toolchain (runs %d -> %d), so this test "+
			"never observed a cold start and the cache assertion below would be vacuous",
			before, afterCold)
	}

	crd2, err := GenerateCRD(spec, gvk)
	if err != nil {
		t.Fatalf("second GenerateCRD: %v", err)
	}
	if got := crdGenRuns.Load(); got != afterCold {
		t.Errorf("second GenerateCRD re-invoked the go toolchain (runs %d -> %d); Observe would pay "+
			"a compilation on every reconcile", afterCold, got)
	}

	if crd1.Name != crd2.Name {
		t.Fatalf("cached CRD differs from freshly generated: %q vs %q", crd1.Name, crd2.Name)
	}
}

// Reproduces the cold-start storm that wedged installer-release: many workers generate CRDs
// concurrently. crdgen.Generate execs `go mod tidy` + `go run controller-gen`; run in parallel
// those thrash the shared build/module cache into a livelock. The serialize-the-toolchain fix must
// let every worker finish (no deadlock) and stay race-free (go test -race).
func TestGenerateCRD_ConcurrentNoDeadlock(t *testing.T) {
	spec := cacheTestSchema()
	gvks := []schema.GroupVersionKind{
		{Group: "conc.example.org", Version: "v1", Kind: "Aaa"},
		{Group: "conc.example.org", Version: "v1", Kind: "Bbb"},
		{Group: "conc.example.org", Version: "v1", Kind: "Ccc"},
	}

	const workers = 12
	var wg sync.WaitGroup
	errCh := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			if _, err := GenerateCRD(spec, gvks[idx%len(gvks)]); err != nil {
				errCh <- err
			}
		}(i)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(180 * time.Second):
		t.Fatal("concurrent GenerateCRD deadlocked (did not finish in 180s)")
	}

	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent GenerateCRD error: %v", err)
		}
	}
}

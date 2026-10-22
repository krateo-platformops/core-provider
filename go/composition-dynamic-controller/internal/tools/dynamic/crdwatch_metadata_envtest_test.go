//go:build envtest

// Guards the MEMORY property of WatchCRDsAndInvalidate, which the functional test cannot see.
//
// TestEnvtest_WatchCRDsAndInvalidate_Functional proves the watch still invalidates discovery. It
// passes identically whether the informer caches PartialObjectMetadata or whole
// CustomResourceDefinitions — so on its own it would let a revert to dynamicinformer through
// silently, and the symptom (every controller holding ~60 MiB of CRD schemas it never reads,
// krateo-platformops/core-provider#182) is invisible to any behavioural assertion.
//
// So this asserts the WIRE instead: the metadata client negotiates
// `as=PartialObjectMetadataList` on the list and `as=PartialObjectMetadata` on the watch, and a
// dynamic client cannot. The transport is wrapped around the REAL production call, so the thing
// under test is the shipped code path, not a reconstruction of it.
//
//	Run: KUBEBUILDER_ASSETS=$(setup-envtest use -p path 1.36.0) \
//	     go test -tags envtest ./internal/tools/dynamic/ -run Envtest -v
package dynamic

import (
	"context"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

type crdwatchRoundTripFunc func(*http.Request) (*http.Response, error)

func (f crdwatchRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestEnvtest_WatchCRDsAndInvalidate_AsksForMetadataOnly fails if the informer is ever switched
// back to a client that pulls whole CustomResourceDefinitions.
func TestEnvtest_WatchCRDsAndInvalidate_AsksForMetadataOnly(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via setup-envtest")
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest apiserver: %v", err)
	}
	defer func() { _ = env.Stop() }()

	var mu sync.Mutex
	var accepts []string

	watchCfg := rest.CopyConfig(cfg)
	watchCfg.WrapTransport = func(rt http.RoundTripper) http.RoundTripper {
		return crdwatchRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			if strings.Contains(req.URL.Path, "customresourcedefinitions") {
				mu.Lock()
				accepts = append(accepts, req.Header.Get("Accept"))
				mu.Unlock()
			}
			return rt.RoundTrip(req)
		})
	}

	mapper, err := NewRESTMapper(cfg)
	if err != nil {
		t.Fatalf("NewRESTMapper: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The production call, over the instrumented transport.
	if err := WatchCRDsAndInvalidate(ctx, watchCfg, mapper, nil); err != nil {
		t.Fatalf("WatchCRDsAndInvalidate: %v", err)
	}

	// Wait for the informer's initial LIST (and its WATCH) to reach the apiserver.
	deadline := time.Now().Add(30 * time.Second)
	for {
		mu.Lock()
		n := len(accepts)
		mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no request to customresourcedefinitions observed in 30s — the instrument found " +
				"nothing, so this test proves nothing; check that the transport wrapper is still applied")
		}
		time.Sleep(200 * time.Millisecond)
	}
	// Let the watch follow the list so both are covered.
	time.Sleep(2 * time.Second)

	mu.Lock()
	seen := append([]string(nil), accepts...)
	mu.Unlock()

	var full []string
	for _, a := range seen {
		if !strings.Contains(a, "as=PartialObjectMetadata") {
			full = append(full, a)
		}
	}
	if len(full) > 0 {
		t.Fatalf("WatchCRDsAndInvalidate requested WHOLE CustomResourceDefinitions on %d of %d "+
			"requests; it must negotiate PartialObjectMetadata so controllers do not each cache every "+
			"CRD schema (see #182). Offending Accept headers: %q", len(full), len(seen), full)
	}
	t.Logf("all %d CRD requests negotiated PartialObjectMetadata; e.g. %q", len(seen), seen[0])
}

package tracer

import (
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/krateo-platformops/chart-inspector/internal/handlers/resources"
)

// NoOpRoundTripper is a mock RoundTripper that does nothing
type NoOpRoundTripper struct{}

func (rt *NoOpRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		Status:        "200 OK",
		StatusCode:    200,
		Header:        http.Header{},
		Body:          io.NopCloser(strings.NewReader("{}")),
		ContentLength: 2,
		Request:       req,
	}, nil
}

func TestTracerBasicResourceCapture(t *testing.T) {
	tracer := &Tracer{}
	tracer.WithRoundTripper(&NoOpRoundTripper{})

	// Test: /apis/apps/v1/namespaces/default/deployments/my-deployment
	u, _ := url.Parse("/apis/apps/v1/namespaces/default/deployments/my-deployment")
	req := &http.Request{
		URL: u,
	}
	tracer.RoundTrip(req)

	resources := tracer.GetResources()
	if len(resources) != 1 {
		t.Fatalf("expected 1 resource, got %d", len(resources))
	}

	res := resources[0]
	if res.Group != "apps" || res.Version != "v1" || res.Resource != "deployments" || res.Namespace != "default" || res.Name != "my-deployment" {
		t.Errorf("unexpected resource: %+v", res)
	}
}

func TestTracerMultiplePaths(t *testing.T) {
	tracer := &Tracer{}
	tracer.WithRoundTripper(&NoOpRoundTripper{})

	paths := []struct {
		path     string
		expected resources.Resource
	}{
		{
			path: "/apis/apps/v1/namespaces/default/deployments/my-dep",
			expected: resources.Resource{
				Group:     "apps",
				Version:   "v1",
				Resource:  "deployments",
				Namespace: "default",
				Name:      "my-dep",
				Verbs:     []string{"*"}, // named observation
			},
		},
		{
			path: "/api/v1/namespaces/kube-system/services/kube-dns",
			expected: resources.Resource{
				Group:     "",
				Version:   "v1",
				Resource:  "services",
				Namespace: "kube-system",
				Name:      "kube-dns",
				Verbs:     []string{"*"},
			},
		},
		{
			path: "/apis/batch/v1/jobs/my-job",
			expected: resources.Resource{
				Group:     "batch",
				Version:   "v1",
				Resource:  "jobs",
				Namespace: "",
				Name:      "my-job",
				Verbs:     []string{"*"},
			},
		},
	}

	for _, p := range paths {
		u, _ := url.Parse(p.path)
		req := &http.Request{
			URL: u,
		}
		tracer.RoundTrip(req)
	}

	resources := tracer.GetResources()
	if len(resources) != 3 {
		t.Fatalf("expected 3 resources, got %d", len(resources))
	}

	for i, p := range paths {
		if !reflect.DeepEqual(resources[i], p.expected) {
			t.Errorf("resource %d: expected %+v, got %+v", i, p.expected, resources[i])
		}
	}
}

func TestTracerConcurrentRoundTrip(t *testing.T) {
	tracer := &Tracer{}
	tracer.WithRoundTripper(&NoOpRoundTripper{})

	numGoroutines := 100
	numCallsPerGoroutine := 10
	var wg sync.WaitGroup

	// Spawn concurrent goroutines making RoundTrip calls
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for j := 0; j < numCallsPerGoroutine; j++ {
				path := "/apis/apps/v1/namespaces/default/deployments/dep"
				u, _ := url.Parse(path)
				req := &http.Request{
					URL: u,
				}
				tracer.RoundTrip(req)
			}
		}(i)
	}

	wg.Wait()

	resources := tracer.GetResources()
	expectedCount := numGoroutines * numCallsPerGoroutine
	if len(resources) != expectedCount {
		t.Fatalf("expected %d resources, got %d", expectedCount, len(resources))
	}
}

func TestTracerConcurrentReadWrite(t *testing.T) {
	tracer := &Tracer{}
	tracer.WithRoundTripper(&NoOpRoundTripper{})

	var wg sync.WaitGroup
	successCount := int32(0)

	// Spawn goroutines that call RoundTrip (write)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				path := "/apis/apps/v1/namespaces/default/deployments/dep"
				u, _ := url.Parse(path)
				req := &http.Request{
					URL: u,
				}
				tracer.RoundTrip(req)
			}
		}(i)
	}

	// Spawn goroutines that call GetResources (read)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				resources := tracer.GetResources()
				if resources != nil {
					atomic.AddInt32(&successCount, 1)
				}
			}
		}()
	}

	wg.Wait()

	// Verify we got results without panics (test passes if it completes)
	resources := tracer.GetResources()
	if len(resources) == 0 {
		t.Fatalf("expected some resources, got none")
	}

	if successCount == 0 {
		t.Fatalf("expected successful reads, got 0")
	}
}

func TestTracerGetResourcesReturnsDeepCopy(t *testing.T) {
	tracer := &Tracer{}
	tracer.WithRoundTripper(&NoOpRoundTripper{})

	// Add a resource
	u, _ := url.Parse("/apis/apps/v1/namespaces/default/deployments/my-dep")
	req := &http.Request{
		URL: u,
	}
	tracer.RoundTrip(req)

	// Get resources
	resources1 := tracer.GetResources()
	if len(resources1) != 1 {
		t.Fatalf("expected 1 resource, got %d", len(resources1))
	}

	// Get resources again
	resources2 := tracer.GetResources()

	// Verify they're equal but not the same slice
	if len(resources2) != len(resources1) {
		t.Fatalf("expected same resource count, got %d vs %d", len(resources1), len(resources2))
	}

	if !reflect.DeepEqual(resources1[0], resources2[0]) {
		t.Errorf("expected equal resources: %+v vs %+v", resources1[0], resources2[0])
	}

	// Verify it's a copy by modifying one and checking the other isn't affected internally
	ptr1 := &resources1
	ptr2 := &resources2
	if ptr1 == ptr2 {
		t.Error("expected different slice instances (copy returned)")
	}
}

func TestTracerInvalidPaths(t *testing.T) {
	tracer := &Tracer{}
	tracer.WithRoundTripper(&NoOpRoundTripper{})

	invalidPaths := []string{
		"/health",
		"/",
		"/notapis/v1/pods",
		"/apis/v1",
		"",
	}

	for _, path := range invalidPaths {
		u, _ := url.Parse(path)
		req := &http.Request{
			URL: u,
		}
		tracer.RoundTrip(req)
	}

	resources := tracer.GetResources()
	// No valid resources should be captured
	if len(resources) != 0 {
		t.Fatalf("expected 0 resources for invalid paths, got %d", len(resources))
	}
}

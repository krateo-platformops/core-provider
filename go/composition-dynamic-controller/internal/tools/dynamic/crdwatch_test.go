package dynamic

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/rest"
)

// A RESTMapper without a Reset() method must make WatchCRDsAndInvalidate a no-op that
// returns nil before it ever touches the rest.Config / builds a client.
func TestWatchCRDsAndInvalidate_NoResetIsNoop(t *testing.T) {
	staticMapper := meta.NewDefaultRESTMapper(nil) // has no Reset() method
	if _, ok := interface{}(staticMapper).(interface{ Reset() }); ok {
		t.Fatal("precondition: DefaultRESTMapper unexpectedly implements Reset()")
	}

	// nil rest.Config is fine: the no-op branch returns before using it.
	if err := WatchCRDsAndInvalidate(context.Background(), nil, staticMapper, nil); err != nil {
		t.Fatalf("WatchCRDsAndInvalidate() with a non-resettable mapper = %v, want nil", err)
	}
}

// A resettable mapper takes the ACTIVE path: WatchCRDsAndInvalidate builds a dynamic client from the
// config and starts the CRD watch, returning nil once setup succeeds. A dummy Host constructs the
// client without dialing; the informer then never syncs, which the watch handles gracefully (it logs
// and degrades rather than erroring). The async add->Reset behaviour itself is proven end-to-end
// against a real apiserver in crdwatch_envtest_test.go (build tag: envtest).
func TestWatchCRDsAndInvalidate_ResettableMapperSetupOK(t *testing.T) {
	mock := &MockResettableMapper{MockRESTMapper: &MockRESTMapper{scopeName: meta.RESTScopeNameNamespace}}
	if _, ok := interface{}(mock).(interface{ Reset() }); !ok {
		t.Fatal("precondition: MockResettableMapper must implement Reset()")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Setup must succeed (return nil) even though the watch will never sync against this dummy host.
	if err := WatchCRDsAndInvalidate(ctx, &rest.Config{Host: "http://127.0.0.1:1"}, mock, nil); err != nil {
		t.Fatalf("WatchCRDsAndInvalidate() setup with a resettable mapper = %v, want nil", err)
	}
}

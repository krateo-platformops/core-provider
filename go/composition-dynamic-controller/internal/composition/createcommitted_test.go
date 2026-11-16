package composition

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/krateo-platformops/unstructured-runtime/pkg/controller"
	"k8s.io/client-go/rest"
)

// unstructured-runtime#97 (v1.22.0): only a failure the handler classified as NOT COMMITTED is
// re-created at once; any other is confirmed absent first and an absent release waits out a 2m
// grace. A failure before the helm call wrote no release, so it must be classified, or a chart
// that cannot be fetched, a refused RBAC apply or an unreachable apiserver would each cost 2m per
// retry where they cost nothing before.
func TestACreateThatFailsBeforeHelmIsNotCommitted(t *testing.T) {
	h := &handler{kubeconfig: &rest.Config{Host: "http://127.0.0.1:9", Timeout: 2 * time.Second}}
	err := h.Create(context.Background(), fireworksMg(1))
	if err == nil {
		t.Fatal("Create against an unreachable apiserver succeeded")
	}
	if !errors.Is(err, controller.ErrCreateNotCommitted) {
		t.Fatalf("a failure before any helm call is not classified as not committed: %v", err)
	}
	t.Logf("failed before helm, classified: %v", err)
}

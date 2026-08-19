package composition

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/gobuffalo/flect"
	"github.com/krateo-platformops/unstructured-runtime/pkg/pluralizer"
	"github.com/krateo-platformops/unstructured-runtime/pkg/tools"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

type statusRetryPluralizer struct{}

var _ pluralizer.PluralizerInterface = statusRetryPluralizer{}

func (statusRetryPluralizer) GVKtoGVR(gvk schema.GroupVersionKind) (schema.GroupVersionResource, error) {
	return schema.GroupVersionResource{
		Group:    gvk.Group,
		Version:  gvk.Version,
		Resource: flect.Pluralize(strings.ToLower(gvk.Kind)),
	}, nil
}

var srGVR = schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v0-1-0", Resource: "portals"}

func srPortal(rv, statusMarker string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: "composition.krateo.io", Version: "v0-1-0", Kind: "Portal"})
	u.SetName("portal")
	u.SetNamespace("krateo-system")
	u.SetResourceVersion(rv)
	_ = unstructured.SetNestedField(u.Object, statusMarker, "status", "marker")
	return u
}

func srFakeDyn(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	scheme := runtime.NewScheme()
	gvrToListKind := map[schema.GroupVersionResource]string{srGVR: "PortalList"}
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, gvrToListKind, objs...)
}

func srOpts(dyn *dynamicfake.FakeDynamicClient) tools.UpdateOptions {
	return tools.UpdateOptions{Pluralizer: statusRetryPluralizer{}, DynamicClient: dyn}
}

// A 409 on the first status write (as happens during a GVK-migration handover) must be retried
// after a re-fetch, and the status this reconcile computed must ultimately land. Regression guard
// for krateo-platformops/core-provider#57.
func TestUpdateStatusWithRetry_RecoversFromConflict(t *testing.T) {
	// The stored object was concurrently modified (rv=2) after we read it (rv=1).
	dyn := srFakeDyn(srPortal("2", "concurrent"))

	var statusUpdates int
	dyn.PrependReactor("update", "portals", func(action clienttesting.Action) (bool, runtime.Object, error) {
		ua, ok := action.(clienttesting.UpdateAction)
		if !ok || ua.GetSubresource() != "status" {
			return false, nil, nil
		}
		statusUpdates++
		if statusUpdates == 1 {
			return true, nil, apierrors.NewConflict(srGVR.GroupResource(), "portal", fmt.Errorf("the object has been modified"))
		}
		return false, nil, nil // fall through to the tracker (success)
	})

	// Our reconcile computed a "healthy" status against the now-stale rv=1.
	mg := srPortal("1", "healthy")
	got, err := updateStatusWithRetry(context.Background(), mg, srOpts(dyn))
	if err != nil {
		t.Fatalf("expected recovery from the conflict, got error: %v", err)
	}
	if statusUpdates < 2 {
		t.Fatalf("expected a retry (>=2 status writes), got %d", statusUpdates)
	}
	if m, _, _ := unstructured.NestedString(got.Object, "status", "marker"); m != "healthy" {
		t.Fatalf("computed status should win after re-fetch; got marker=%q", m)
	}
}

// A non-conflict error must propagate immediately and never be retried.
func TestUpdateStatusWithRetry_NonConflictNotRetried(t *testing.T) {
	dyn := srFakeDyn(srPortal("1", "x"))

	var statusUpdates int
	dyn.PrependReactor("update", "portals", func(action clienttesting.Action) (bool, runtime.Object, error) {
		ua, ok := action.(clienttesting.UpdateAction)
		if !ok || ua.GetSubresource() != "status" {
			return false, nil, nil
		}
		statusUpdates++
		return true, nil, apierrors.NewInternalError(fmt.Errorf("boom"))
	})

	_, err := updateStatusWithRetry(context.Background(), srPortal("1", "healthy"), srOpts(dyn))
	if err == nil {
		t.Fatal("expected the non-conflict error to propagate")
	}
	if statusUpdates != 1 {
		t.Fatalf("a non-conflict error must not be retried; got %d attempts", statusUpdates)
	}
}

// The happy path: no conflict -> a single write, computed status returned.
func TestUpdateStatusWithRetry_SuccessFirstTry(t *testing.T) {
	dyn := srFakeDyn(srPortal("1", "x"))

	var statusUpdates int
	dyn.PrependReactor("update", "portals", func(action clienttesting.Action) (bool, runtime.Object, error) {
		if ua, ok := action.(clienttesting.UpdateAction); ok && ua.GetSubresource() == "status" {
			statusUpdates++
		}
		return false, nil, nil
	})

	got, err := updateStatusWithRetry(context.Background(), srPortal("1", "healthy"), srOpts(dyn))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if statusUpdates != 1 {
		t.Fatalf("expected exactly 1 status write, got %d", statusUpdates)
	}
	if m, _, _ := unstructured.NestedString(got.Object, "status", "marker"); m != "healthy" {
		t.Fatalf("expected healthy marker, got %q", m)
	}
}

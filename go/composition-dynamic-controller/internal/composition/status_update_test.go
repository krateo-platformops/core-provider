package composition

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gobuffalo/flect"
	"github.com/krateo-platformops/unstructured-runtime/pkg/pluralizer"
	"github.com/krateo-platformops/unstructured-runtime/pkg/tools"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// #359. A 409 means this pass computed its status from a stale read, so the newer write must
// SURVIVE, and the pass must get the conflict back to requeue. The test this replaces asserted the
// opposite ("computed status should win after re-fetch"), which is the lost update: on a snowplow
// bench cluster a stale recovery pass wiped status.managed this way.
//
// The stored object carries what a newer write recorded: a different marker AND a field the stale
// pass never saw (managed). Both must still be there afterwards.
func TestUpdateStatus_ConflictIsReturnedAndTheNewerWriteSurvives(t *testing.T) {
	stored := srPortal("2", "newer")
	_ = unstructured.SetNestedSlice(stored.Object, []any{map[string]any{"name": "child"}}, "status", "managed")
	dyn := srFakeDyn(stored)

	var statusUpdates int
	dyn.PrependReactor("update", "portals", func(action clienttesting.Action) (bool, runtime.Object, error) {
		ua, ok := action.(clienttesting.UpdateAction)
		if !ok || ua.GetSubresource() != "status" {
			return false, nil, nil
		}
		statusUpdates++
		// The apiserver's answer to a write carrying a stale resourceVersion.
		return true, nil, apierrors.NewConflict(srGVR.GroupResource(), "portal", fmt.Errorf("the object has been modified"))
	})

	// This pass read rv=1, before the newer write, and computed a status without `managed`.
	_, err := updateStatus(context.Background(), srPortal("1", "stale"), srOpts(dyn))
	if err == nil || !apierrors.IsConflict(errors.Unwrap(err)) {
		t.Fatalf("want the conflict returned (wrapped), got %v", err)
	}
	// THE CONTRACT THE RUNTIME READS. unstructured-runtime (>= v1.9.0, #63) requeues a pass quietly,
	// with no ReconcileError, no event and no failure metric, only when errors.Is(err,
	// tools.ErrStatusConflict). IsConflict alone is not enough: a conflict that loses the sentinel on
	// the way up is counted as an ordinary failure.
	if !errors.Is(err, tools.ErrStatusConflict) {
		t.Fatalf("the returned error does not match tools.ErrStatusConflict, so the runtime would count "+
			"this lost race as a failure instead of a quiet requeue: %v", err)
	}
	if statusUpdates != 1 {
		t.Fatalf("the stale status was re-submitted after the conflict (%d writes): that is the lost update", statusUpdates)
	}
	got, gErr := dyn.Resource(srGVR).Namespace("krateo-system").Get(context.Background(), "portal", metav1.GetOptions{})
	if gErr != nil {
		t.Fatal(gErr)
	}
	if m, _, _ := unstructured.NestedString(got.Object, "status", "marker"); m != "newer" {
		t.Errorf("the newer status was replaced: marker=%q", m)
	}
	if managed, found, _ := unstructured.NestedSlice(got.Object, "status", "managed"); !found || len(managed) != 1 {
		t.Errorf("status.managed written by the newer pass was removed (found=%v, %v)", found, managed)
	}
}

// A non-conflict error must propagate immediately and never be retried.
func TestUpdateStatus_NonConflictNotRetried(t *testing.T) {
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

	_, err := updateStatus(context.Background(), srPortal("1", "healthy"), srOpts(dyn))
	if err == nil {
		t.Fatal("expected the non-conflict error to propagate")
	}
	if statusUpdates != 1 {
		t.Fatalf("a non-conflict error must not be retried; got %d attempts", statusUpdates)
	}
}

// The happy path: no conflict -> a single write, computed status returned.
func TestUpdateStatus_SuccessFirstTry(t *testing.T) {
	dyn := srFakeDyn(srPortal("1", "x"))

	var statusUpdates int
	dyn.PrependReactor("update", "portals", func(action clienttesting.Action) (bool, runtime.Object, error) {
		if ua, ok := action.(clienttesting.UpdateAction); ok && ua.GetSubresource() == "status" {
			statusUpdates++
		}
		return false, nil, nil
	})

	got, err := updateStatus(context.Background(), srPortal("1", "healthy"), srOpts(dyn))
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

// The pending-operation condition write (#359 follow-through). A lost race must reach the runtime so
// the condition is recorded on the next pass; any other failure stays best-effort. The real
// conflict error is produced by updateStatus over a fake 409, not constructed, so the test follows
// the wrapping chain production uses.
func TestPendingConditionWriteOutcome(t *testing.T) {
	dyn := srFakeDyn(srPortal("2", "newer"))
	dyn.PrependReactor("update", "portals", func(action clienttesting.Action) (bool, runtime.Object, error) {
		if ua, ok := action.(clienttesting.UpdateAction); ok && ua.GetSubresource() == "status" {
			return true, nil, apierrors.NewConflict(srGVR.GroupResource(), "portal", fmt.Errorf("the object has been modified"))
		}
		return false, nil, nil
	})
	_, conflict := updateStatus(context.Background(), srPortal("1", "stale"), srOpts(dyn))
	if conflict == nil {
		t.Fatal("setup: the fake 409 produced no error")
	}

	if got := pendingConditionWriteOutcome(conflict); !errors.Is(got, tools.ErrStatusConflict) {
		t.Errorf("a status conflict was swallowed (got %v): the composition would keep claiming "+
			"Ready=True for the whole pending grace period instead of being requeued in 1s", got)
	}
	// The control: an ordinary failure stays best-effort, as before.
	if got := pendingConditionWriteOutcome(fmt.Errorf("etcd timeout")); got != nil {
		t.Errorf("a non-conflict failure of a cosmetic write must not fail the pass, got %v", got)
	}
	if got := pendingConditionWriteOutcome(nil); got != nil {
		t.Errorf("success must not fail the pass, got %v", got)
	}
}

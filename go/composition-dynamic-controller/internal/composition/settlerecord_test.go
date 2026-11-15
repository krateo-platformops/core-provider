package composition

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
	"github.com/krateo-platformops/unstructured-runtime/pkg/logging"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clienttesting "k8s.io/client-go/testing"
)

func goneLookup(gone string) podLookup {
	return func(_ context.Context, ns, name string) error {
		if ns+"/"+name == gone {
			return apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, name)
		}
		return nil
	}
}

// core-provider#520, krateo-057 installer-platform: the draining old controller's upgrade holds the
// release pending, recorded as started by it; the new pod records its own reconcile and is REFUSED
// by helm. The record the old pod wrote must be what the cluster holds afterwards, so the next
// Observe clears the orphaned operation at once instead of after the 16m grace.
func TestSettleHelmOperationRecord_RefusedCallRestoresTheHoldersRecord(t *testing.T) {
	ctx := context.Background()
	held := time.Date(2026, 11, 15, 10, 44, 45, 0, time.UTC)
	for _, tc := range []struct {
		name       string
		callErr    error
		wantOrphan bool
	}{
		{"refused: the holder's record is restored and the orphan is found", fmt.Errorf("upgrade failed: %w", helmconfig.ErrOperationInProgress), true},
		{"control, an ordinary failure: this pod's own record is cleared, and nothing remains to find", errors.New("upgrade failed: boom"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			podName, podNamespace = "installers-v0-3-651-new", "krateo-system"
			t.Cleanup(func() { podName, podNamespace = "", "" })

			obj := srPortal("1", "x")
			_ = unstructured.SetNestedStringMap(obj.Object, map[string]string{
				"phase": "reconcile", "startedAt": held.Format(time.RFC3339Nano), "startedBy": "krateo-system/installers-v0-3-650-old",
			}, "status", "helmOperation")
			dyn := srFakeDyn(obj.DeepCopy())
			opts := srOpts(dyn)
			mg := obj.DeepCopy()

			prior := priorHelmOperationRecord(mg)
			mg, err := recordHelmOperation(ctx, mg, "reconcile", held.Add(17*time.Second), opts, logging.NewNopLogger())
			if err != nil {
				t.Fatal(err)
			}
			mg = settleHelmOperationRecord(ctx, mg, prior, tc.callErr, opts, logging.NewNopLogger())

			// What the next pass sees: for a refusal, the cluster (it must have been written); for a
			// failure, the in-memory object the handler's next status write persists.
			seen := mg
			if errors.Is(tc.callErr, helmconfig.ErrOperationInProgress) {
				got, gerr := dyn.Resource(srGVR).Namespace("krateo-system").Get(ctx, "portal", metav1.GetOptions{})
				if gerr != nil {
					t.Fatal(gerr)
				}
				seen = got
			}
			rel := &helmconfig.Release{Status: helmconfig.StatusPendingUpgrade, Updated: held.Add(time.Second)}
			orphan, why := orphanedOperation(ctx, seen, rel, goneLookup("krateo-system/installers-v0-3-650-old"))
			if orphan != tc.wantOrphan {
				t.Fatalf("orphanedOperation = %v (%s), want %v", orphan, why, tc.wantOrphan)
			}
		})
	}
}

// Clearing removes only this pod's record (or a starter-less one), never another pod's.
func TestClearOwnHelmOperationRecord(t *testing.T) {
	podName, podNamespace = "me", "ns"
	t.Cleanup(func() { podName, podNamespace = "", "" })
	for _, tc := range []struct {
		by        string
		wantClear bool
	}{{"ns/me", true}, {"", true}, {"ns/other", false}} {
		mg := &unstructured.Unstructured{Object: map[string]any{}}
		_ = unstructured.SetNestedStringMap(mg.Object, map[string]string{"phase": "reconcile", "startedBy": tc.by}, "status", "helmOperation")
		clearOwnHelmOperationRecord(mg)
		_, found, _ := unstructured.NestedMap(mg.Object, "status", "helmOperation")
		if found == tc.wantClear {
			t.Errorf("startedBy %q: record found=%v after clearing, want cleared=%v", tc.by, found, tc.wantClear)
		}
	}
}

// #526: the reconcile's record hook returns a record that did not land, so plumbing aborts the
// revision; a record that landed returns nil and keeps the record it replaced for the settle.
func TestOperationRecorder(t *testing.T) {
	ctx := context.Background()
	t.Run("a lost race is returned, so the operation does not start", func(t *testing.T) {
		dyn := srFakeDyn(srPortal("2", "newer"))
		dyn.PrependReactor("update", "portals", func(a clienttesting.Action) (bool, runtime.Object, error) {
			if ua, ok := a.(clienttesting.UpdateAction); ok && ua.GetSubresource() == "status" {
				return true, nil, apierrors.NewConflict(schema.GroupResource{Group: srGVR.Group, Resource: srGVR.Resource}, "portal", nil)
			}
			return false, nil, nil
		})
		mg := srPortal("1", "stale")
		var prior map[string]interface{}
		if err := operationRecorder(&mg, &prior, srOpts(dyn), logging.NewNopLogger())(ctx); err == nil {
			t.Fatal("a conflicted record must be returned, so plumbing aborts the revision")
		}
	})
	t.Run("a landed record returns nil and keeps the replaced one", func(t *testing.T) {
		obj := srPortal("1", "x")
		_ = unstructured.SetNestedStringMap(obj.Object, map[string]string{"phase": "install", "startedBy": "ns/old"}, "status", "helmOperation")
		dyn := srFakeDyn(obj.DeepCopy())
		mg := obj.DeepCopy()
		var prior map[string]interface{}
		if err := operationRecorder(&mg, &prior, srOpts(dyn), logging.NewNopLogger())(ctx); err != nil {
			t.Fatal(err)
		}
		if prior["startedBy"] != "ns/old" {
			t.Fatalf("prior = %v, want the replaced record (startedBy ns/old)", prior)
		}
		if rec, _, _ := unstructured.NestedStringMap(mg.Object, "status", "helmOperation"); rec["phase"] != "reconcile" {
			t.Fatalf("mg record = %v, want this reconcile's", rec)
		}
	})
}

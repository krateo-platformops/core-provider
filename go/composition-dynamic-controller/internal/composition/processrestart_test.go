package composition

import (
	"context"
	"testing"
	"time"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// openstack r067 (gnocchi): an API-server outage crossed the ownership Lease's renew deadline, the
// controller exited by design, and the kubelet restarted the container IN THE SAME POD. The new
// process read its dead predecessor's record as "started by this pod", so the release it had left
// pending-upgrade waited the full pendingOperationGrace (16m). The record now names the process too.
func TestOrphanedOperation_SamePodAnotherProcess(t *testing.T) {
	podName, podNamespace = "gnocchis-v0-3-67-controller-f74958f88-lr99g", "krateo-system"
	prev := processID
	t.Cleanup(func() { podName, podNamespace, processID = "", "", prev })
	updated := time.Date(2026, 11, 16, 8, 27, 48, 0, time.UTC)
	rel := &helmconfig.Release{Status: helmconfig.StatusPendingUpgrade, Updated: updated}
	self := podNamespace + "/" + podName
	record := func(process string) *unstructured.Unstructured {
		mg := &unstructured.Unstructured{Object: map[string]any{}}
		rec := map[string]any{"phase": "reconcile", "startedAt": updated.Add(-2 * time.Second).Format(time.RFC3339Nano), "startedBy": self}
		if process != "" {
			rec["startedByProcess"] = process
		}
		_ = unstructured.SetNestedMap(mg.Object, rec, "status", "helmOperation")
		return mg
	}
	// The pod itself is ALIVE (it is this one): a pod lookup could never say "gone" here, which is
	// exactly why the pod name alone could not catch the restart.
	alive := func(context.Context, string, string) error { return nil }
	gone := func(context.Context, string, string) error {
		return apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "x")
	}
	processID = "b2"
	for _, tc := range []struct {
		name   string
		mg     *unstructured.Unstructured
		lookup podLookup
		want   bool
	}{
		{"this pod, an EARLIER process (the container restarted)", record("a1"), alive, true},
		{"this pod, this very process: still in flight here", record("b2"), alive, false},
		{"this pod, no process recorded (an older controller): keep the grace", record(""), alive, false},
		{"control: another pod that is gone is still orphaned", func() *unstructured.Unstructured {
			mg := record("a1")
			_ = unstructured.SetNestedField(mg.Object, "krateo-system/old-pod", "status", "helmOperation", "startedBy")
			return mg
		}(), gone, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, why := orphanedOperation(context.Background(), tc.mg, rel, tc.lookup)
			if got != tc.want {
				t.Errorf("orphaned = %v (%s), want %v", got, why, tc.want)
			}
		})
	}
}

// The record is written with the process, and only the process that wrote it clears it.
func TestHelmOperationRecordNamesTheProcess(t *testing.T) {
	podName, podNamespace = "p", "ns"
	prev := processID
	t.Cleanup(func() { podName, podNamespace, processID = "", "", prev })

	processID = "a1"
	mg := &unstructured.Unstructured{Object: map[string]any{}}
	setHelmOperationRecord(mg, "upgrade", time.Now())
	if got, _, _ := unstructured.NestedString(mg.Object, "status", "helmOperation", "startedByProcess"); got != "a1" {
		t.Fatalf("startedByProcess = %q, want the writing process a1", got)
	}

	processID = "b2" // the container restarted: same pod, new process
	clearOwnHelmOperationRecord(mg)
	if _, found, _ := unstructured.NestedMap(mg.Object, "status", "helmOperation"); !found {
		t.Fatal("a new process cleared its predecessor's record: that record is the orphan the recovery must see")
	}
	processID = "a1"
	clearOwnHelmOperationRecord(mg)
	if _, found, _ := unstructured.NestedMap(mg.Object, "status", "helmOperation"); found {
		t.Fatal("control: the writing process must clear its own record")
	}
}

// Two processes never share an identity.
func TestNewProcessIDIsDistinct(t *testing.T) {
	a, b := newProcessID(), newProcessID()
	if a == "" || b == "" || a == b {
		t.Fatalf("process identities %q and %q must be non-empty and distinct", a, b)
	}
}

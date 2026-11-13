package composition

import (
	"context"
	"errors"
	"testing"
	"time"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// #461 (c), Diego 2026-11-13: a pending operation is cleared at once only on positive evidence that
// its starter pod is gone AND the record is this operation's. One arm per input state; every arm but
// the first must keep the grace.
func TestOrphanedOperation(t *testing.T) {
	podName, podNamespace = "fireworks-v1-controller-new", "krateo-system"
	t.Cleanup(func() { podName, podNamespace = "", "" })

	updated := time.Date(2026, 11, 13, 13, 11, 12, 0, time.UTC) // the pending revision's LastDeployed
	rel := &helmconfig.Release{Status: helmconfig.StatusPendingUpgrade, Updated: updated}
	withRecord := func(startedAt time.Time, startedBy string) *unstructured.Unstructured {
		mg := &unstructured.Unstructured{Object: map[string]any{}}
		rec := map[string]any{"phase": "reconcile", "startedAt": startedAt.Format(time.RFC3339Nano)}
		if startedBy != "" {
			rec["startedBy"] = startedBy
		}
		_ = unstructured.SetNestedMap(mg.Object, rec, "status", "helmOperation")
		return mg
	}
	gone := func(context.Context, string, string) error {
		return apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "x")
	}
	alive := func(context.Context, string, string) error { return nil }
	broken := func(context.Context, string, string) error { return errors.New("apiserver unavailable") }
	old := "krateo-system/fireworks-v0-controller-old"

	for _, tc := range []struct {
		name   string
		mg     *unstructured.Unstructured
		lookup podLookup
		want   bool
	}{
		{"the record is this revision's and its starter pod is NotFound", withRecord(updated.Add(-2*time.Second), old), gone, true},
		{"the starter pod is alive", withRecord(updated.Add(-2*time.Second), old), alive, false},
		{"the lookup failed: could not determine is not dead", withRecord(updated.Add(-2*time.Second), old), broken, false},
		{"a stale record from an earlier operation", withRecord(updated.Add(-2*time.Hour), old), gone, false},
		{"a record written after the revision (not its starter)", withRecord(updated.Add(time.Second), old), gone, false},
		{"started by this very pod", withRecord(updated.Add(-2*time.Second), "krateo-system/fireworks-v1-controller-new"), gone, false},
		{"no starter recorded", withRecord(updated.Add(-2*time.Second), ""), gone, false},
		{"no record at all", &unstructured.Unstructured{Object: map[string]any{}}, gone, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, why := orphanedOperation(context.Background(), tc.mg, rel, tc.lookup)
			if got != tc.want {
				t.Errorf("orphaned = %v (%s), want %v", got, why, tc.want)
			}
		})
	}
}

// The record names the pod as namespace/name, in the same object the condition is written to, and
// clearing removes it; with no pod identity configured no starter is recorded (today's behaviour).
func TestHelmOperationRecord(t *testing.T) {
	podName, podNamespace = "p", "ns"
	t.Cleanup(func() { podName, podNamespace = "", "" })
	mg := &unstructured.Unstructured{Object: map[string]any{}}
	at := time.Date(2026, 11, 13, 13, 0, 0, 0, time.UTC)
	setHelmOperationRecord(mg, "rollback", at)
	rec, _, _ := unstructured.NestedStringMap(mg.Object, "status", "helmOperation")
	if rec["phase"] != "rollback" || rec["startedBy"] != "ns/p" || rec["startedAt"] != at.Format(time.RFC3339Nano) {
		t.Errorf("record = %v", rec)
	}
	clearHelmOperationRecord(mg)
	if _, found, _ := unstructured.NestedMap(mg.Object, "status", "helmOperation"); found {
		t.Error("clear left the record")
	}
	podName, podNamespace = "", ""
	setHelmOperationRecord(mg, "install", at)
	if rec, _, _ := unstructured.NestedStringMap(mg.Object, "status", "helmOperation"); rec["startedBy"] != "" {
		t.Errorf("no pod identity must record no starter, got %q", rec["startedBy"])
	}
}

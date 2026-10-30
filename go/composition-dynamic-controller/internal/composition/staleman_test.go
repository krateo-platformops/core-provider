package composition

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// #261. A chart that RENAMES a child leaves the old name in status.managed and the new one absent.
// Only Create/Update write that field, and if the digest is unchanged nothing dispatches an Update
// — so the composition named a deleted resource forever. Measured on a live cluster: ~50 Observes,
// zero Updates, Ready pinned at the transition that first read the stale set.
//
// The repair is for Observe to report DRIFT, handing it to Update, which already decodes the
// upgraded release and rewrites the set from it. Observe cannot do it directly: getting the current
// render costs a full manifest decode, measured at 15-27x the digest-only path and 230KB/11.5MB/125MB
// of allocation for a 10KB/500KB/5MB manifest against 32-40 bytes — on every reconcile that would
// dwarf the allocation reduction #250 just achieved.
//
// The RENAME shape is the one to test: the resource SET changes while the COUNT stays the same. A
// test that adds or removes a child passes against code that only ever appends.
func TestAbsentManagedChildIsSurfacedForDriftReporting(t *testing.T) {
	h := &handler{}

	// One recorded child, present in the cluster; one recorded child, absent — exactly a rename
	// where the old name lingers in the record.
	present := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "kept", "namespace": "ns"},
	}}
	mg := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "composition.krateo.io/v0-1-0", "kind": "Portal",
		"metadata": map[string]any{"name": "p", "namespace": "ns"},
		"status": map[string]any{"managed": []any{
			map[string]any{"apiVersion": "apps/v1", "resource": "deployments", "name": "kept", "namespace": "ns"},
			map[string]any{"apiVersion": "apps/v1", "resource": "deployments", "name": "renamed-away", "namespace": "ns"},
		}},
	}}

	v := h.rollupManagedChildren(context.Background(), fakeDynWith(present), mg)

	if len(v.absent) != 1 || v.absent[0] == "" {
		t.Fatalf("absent = %v, want exactly the one renamed-away child. Without this Observe cannot "+
			"tell a stale record from a healthy one and the composition never recovers (#261)", v.absent)
	}
	// The operator-facing output must be unchanged: absent children stay NAMED in converging.
	// Asserted as a SUBSET rather than a count — the present child here has no readyReplicas, so
	// it is legitimately converging too, and a count would be asserting something about the
	// fixture rather than about the invariant.
	inConverging := false
	for _, c := range v.converging {
		if c == v.absent[0] {
			inConverging = true
		}
	}
	if !inConverging {
		t.Errorf("absent child %q is not named in converging %v; absent must be a SUBSET, or the "+
			"operator loses the name they need to reach it", v.absent[0], v.converging)
	}
	if v.reason != "Creating" {
		t.Errorf("reason = %q, want Creating — #261 changes what Observe DOES with this, not what "+
			"an operator reads", v.reason)
	}

	// CONTROL: a composition whose recorded children all exist must report NO absent children, or
	// every healthy composition would dispatch an Update on every resync.
	okMg := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "composition.krateo.io/v0-1-0", "kind": "Portal",
		"metadata": map[string]any{"name": "q", "namespace": "ns"},
		"status": map[string]any{"managed": []any{
			map[string]any{"apiVersion": "apps/v1", "resource": "deployments", "name": "kept", "namespace": "ns"},
		}},
	}}
	okPresent := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "kept", "namespace": "ns"},
	}}
	okV := h.rollupManagedChildren(context.Background(), fakeDynWith(okPresent), okMg)
	if len(okV.absent) != 0 {
		t.Errorf("control: a fully-present managed set reported %v absent; every healthy "+
			"composition would then churn an Update per resync", okV.absent)
	}
	_ = okMg
}

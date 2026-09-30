package composition

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// The converging branch must NAME the children it counted, as the failing branch always has.
//
// "1 of 86 managed children are not ready" gave an operator a number and no way to reach the child.
// status.managed lists Kubernetes objects with no readiness field to sort by, so finding the one
// meant sweeping every kind in composition.krateo.io and checking each CR's own conditions — about
// forty kinds, and ambiguous because compositions flap Ready=False transiently during a roll.
//
// On krateo-057 that produced three wrong attributions in a single evening, including blaming a
// composition that was not in status.managed at all. The count could never have been produced
// without knowing which child it was; not saying so was the whole defect.
func TestRollup_ConvergingNamesTheChild(t *testing.T) {
	h := &handler{}
	mg := &unstructured.Unstructured{Object: map[string]any{}}
	// Not seeded => NotFound => converging.
	setManaged(mg, map[string]any{"apiVersion": "apps/v1", "resource": "deployments", "name": "web", "namespace": "ns"})

	v := h.rollupManagedChildren(context.Background(), fakeDynWith(), mg)

	if v.ready || v.reason != "Creating" {
		t.Fatalf("expected Creating/not-ready, got ready=%v reason=%s", v.ready, v.reason)
	}
	if len(v.converging) != 1 || v.converging[0] != "deployments/ns/web" {
		t.Errorf("converging must carry the child id: got %v", v.converging)
	}
	// The message is what an operator actually reads — the portal renders the condition, not the
	// struct. A count without an identifier is the thing being fixed.
	if !strings.Contains(v.message, "deployments/ns/web") {
		t.Errorf("the condition message must name the child, or the count is unactionable: %q", v.message)
	}
	// The count stays: "1 of 3" tells you whether this is one straggler or a broad failure, which
	// changes what you do next.
	if !strings.Contains(v.message, "1 of 1") {
		t.Errorf("the message must keep the count alongside the name: %q", v.message)
	}
}

// Naming must stay bounded. A composition with 86 children mid-roll could otherwise put dozens of
// identifiers into a condition message that has to remain readable — and Kubernetes truncates
// oversized conditions, which would lose the count as well as the names.
func TestRollup_ConvergingNamesAreCapped(t *testing.T) {
	h := &handler{}
	mg := &unstructured.Unstructured{Object: map[string]any{}}
	refs := make([]any, 0, 8)
	for _, n := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		refs = append(refs, map[string]any{"apiVersion": "apps/v1", "resource": "deployments", "name": n, "namespace": "ns"})
	}
	_ = unstructured.SetNestedSlice(mg.Object, refs, "status", "managed")

	v := h.rollupManagedChildren(context.Background(), fakeDynWith(), mg)

	if len(v.converging) != 8 {
		t.Fatalf("expected all 8 counted, got %d", len(v.converging))
	}
	if !strings.Contains(v.message, "+5 more") {
		t.Errorf("message must cap the list and say how many were omitted: %q", v.message)
	}
	if !strings.Contains(v.message, "8 of 8") {
		t.Errorf("the total must survive capping — it is what distinguishes one straggler from a "+
			"broad failure: %q", v.message)
	}
}

// The failed branch keeps naming, unchanged. Guarding against a refactor that unifies the two
// branches and drops one side's identifiers.
func TestRollup_FailedStillNamesTheChild(t *testing.T) {
	h := &handler{}
	bad := krateoCR("False", "Unavailable", true)
	_ = unstructured.SetNestedField(bad.Object, "db", "metadata", "name")
	_ = unstructured.SetNestedField(bad.Object, "ns", "metadata", "namespace")
	mg := &unstructured.Unstructured{Object: map[string]any{}}
	setManaged(mg, map[string]any{"apiVersion": "composition.krateo.io/v0-1-0", "resource": "portals", "name": "db", "namespace": "ns"})

	v := h.rollupManagedChildren(context.Background(), fakeDynWith(bad), mg)

	if v.ready || v.reason != "Unavailable" {
		t.Fatalf("expected Unavailable, got ready=%v reason=%s", v.ready, v.reason)
	}
	if !strings.Contains(v.message, "portals/ns/db") {
		t.Errorf("failed children must stay named: %q", v.message)
	}
}

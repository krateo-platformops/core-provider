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

// #121's tier-2 caveat was computed on every reconcile and discarded every time.
//
// The rollup baked "(N of M managed children not health-evaluated)" into its own message;
// resolveReady then chose between the projected message and the caller's phase message and never
// read the rollup's, so the caveat could not reach the condition. It shipped in 2.13.15 as the
// answer to "tier 2 needs something that makes its absence visible" and made nothing visible.
//
// Worth recording how it survived: I "verified it live" by observing compositions on 057 reporting
// "Composition is up-to-date" with no suffix and concluding unevaluated == 0. The suffix could not
// render, so that observation was compatible with any value of unevaluated.
func TestResolveReady_UnevaluatedCaveatReachesTheCondition(t *testing.T) {
	v := healthVerdict{ready: true, reason: "Available",
		message: availableMessage(3, 10), unevaluated: 3, total: 10}

	out := resolveReady(false, false, "", "Composition is up-to-date", v)

	if out.reason != "Available" {
		t.Fatalf("expected Available, got %s", out.reason)
	}
	if !strings.Contains(out.message, "3 of 10 managed children not health-evaluated") {
		t.Errorf("the coverage caveat must reach the condition, or tier 2 reports nothing: %q", out.message)
	}
}

// The caveat appends to the caller's PHASE message rather than replacing it. Using v.message would
// overwrite "Composition values updated" with "Composition is up-to-date" at the Update call site.
func TestResolveReady_CaveatPreservesThePhaseMessage(t *testing.T) {
	v := healthVerdict{ready: true, reason: "Available", unevaluated: 2, total: 5}

	out := resolveReady(false, false, "", "Composition values updated", v)

	if !strings.HasPrefix(out.message, "Composition values updated") {
		t.Errorf("phase message must lead, not be replaced: %q", out.message)
	}
	if !strings.Contains(out.message, "2 of 5") {
		t.Errorf("caveat must still be appended: %q", out.message)
	}
}

// An author's projected message also keeps the caveat: it qualifies OUR coverage, not their claim.
func TestResolveReady_CaveatSurvivesAProjectedMessage(t *testing.T) {
	v := healthVerdict{ready: true, reason: "Available", unevaluated: 1, total: 4}

	out := resolveReady(true, true, "app reports healthy", "Composition is up-to-date", v)

	if !strings.HasPrefix(out.message, "app reports healthy") {
		t.Errorf("the author's message must lead: %q", out.message)
	}
	if !strings.Contains(out.message, "1 of 4") {
		t.Errorf("a projected message must not suppress the coverage caveat: %q", out.message)
	}
}

// No unevaluated children: no caveat, message untouched.
func TestResolveReady_NoCaveatWhenFullyEvaluated(t *testing.T) {
	v := healthVerdict{ready: true, reason: "Available", unevaluated: 0, total: 9}

	out := resolveReady(false, false, "", "Composition is up-to-date", v)

	if out.message != "Composition is up-to-date" {
		t.Errorf("no caveat expected when everything was evaluated: %q", out.message)
	}
}

// A projected ready=false short-circuits the rollup, which discarded the failing children's names —
// the one case where both signals AGREE was the one case that lost the identifiers.
func TestResolveReady_ProjectedFalseKeepsTheFailingNames(t *testing.T) {
	v := healthVerdict{ready: false, reason: "Unavailable",
		message: "managed children not healthy: portals/ns/db",
		failing: []string{"portals/ns/db"}, total: 6}

	out := resolveReady(true, false, "health check failed", "Composition is up-to-date", v)

	if out.reason != "Unavailable" {
		t.Fatalf("expected Unavailable, got %s", out.reason)
	}
	if !strings.HasPrefix(out.message, "health check failed") {
		t.Errorf("the author's verdict is the primary reason and must lead: %q", out.message)
	}
	if !strings.Contains(out.message, "portals/ns/db") {
		t.Errorf("the rollup's names must survive a projected false — they are the actionable half: %q", out.message)
	}
}

// A projected ready=true must still NOT override an observed failed child, and must not smuggle the
// author's message over the rollup's. Guarding the #96 rule while the message plumbing changes.
func TestResolveReady_ProjectedTrueCannotMaskAFailedChild(t *testing.T) {
	v := healthVerdict{ready: false, reason: "Unavailable",
		message: "managed children not healthy: portals/ns/db",
		failing: []string{"portals/ns/db"}, total: 6}

	out := resolveReady(true, true, "app reports healthy", "Composition is up-to-date", v)

	if out.reason != "Unavailable" {
		t.Fatalf("a projected true must not override an observed failure, got %s", out.reason)
	}
	if !strings.Contains(out.message, "portals/ns/db") {
		t.Errorf("the rollup's message must win here, naming the child: %q", out.message)
	}
	if strings.Contains(out.message, "app reports healthy") {
		t.Errorf("the author's optimistic message must not appear when a child is observed sick: %q", out.message)
	}
}

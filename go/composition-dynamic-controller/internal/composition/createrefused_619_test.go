package composition

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// #619: a child at Ready=False/CreateRefusedPersistently (oasgen rest-dynamic-controller) is FAILED and
// named by its reason (Diego, 2026-11-22). Before, it fell to the converging default and held its parent
// at Creating indefinitely. CONTROLS: Creating stays converging; the permanent sibling CreateUnreachable
// stays failed with no reason named; a True status with the same reason string is not this arm.
func TestRollup_619_CreateRefusedPersistently(t *testing.T) {
	childWithMessage := func(name, status, reason, message string) *unstructured.Unstructured {
		c := childComposition(name, status, reason)
		conds, _, _ := unstructured.NestedSlice(c.Object, "status", "conditions")
		conds[0].(map[string]any)["message"] = message
		_ = unstructured.SetNestedSlice(c.Object, conds, "status", "conditions")
		return c
	}
	for _, tc := range []struct {
		status, reason string
		want           childState
		wantDetail     string
	}{
		{"False", "CreateRefusedPersistently", childFailed, "CreateRefusedPersistently"},
		{"False", "Creating", childConverging, ""},      // CONTROL: a create in flight
		{"False", "CreateUnreachable", childFailed, ""}, // CONTROL: the permanent sibling, unnamed
		{"True", "CreateRefusedPersistently", childHealthy, ""},
	} {
		ev := krateoEval(childComposition("c", tc.status, tc.reason))
		if ev.state != tc.want || ev.detail != tc.wantDetail {
			t.Errorf("Ready=%s/%s: state %d detail %q; want %d %q", tc.status, tc.reason, ev.state, ev.detail, tc.want, tc.wantDetail)
		}
	}

	ref := func(name string) map[string]any {
		return map[string]any{"apiVersion": "composition.krateo.io/v0-1-0", "resource": "portals", "name": name, "namespace": "ns"}
	}
	const vendorMsg = "the vendor has refused this create and the resource has not been Ready for 1h0m0s (last status 422): name taken"
	h := &handler{}
	mg := &unstructured.Unstructured{Object: map[string]any{}}
	setManaged(mg, ref("refused"), ref("ok"))
	v := h.rollupManagedChildren(context.Background(), fakeDynWith(
		childWithMessage("refused", "False", "CreateRefusedPersistently", vendorMsg),
		childComposition("ok", "True", "Available"),
	), mg)
	if v.ready || v.reason != "Unavailable" {
		t.Fatalf("a child the vendor persistently refuses must fail the parent, not hold it converging: ready=%v reason=%s message=%q", v.ready, v.reason, v.message)
	}
	if len(v.failing) != 1 || v.failing[0] != "portals/ns/refused (CreateRefusedPersistently)" || !strings.Contains(v.message, "(CreateRefusedPersistently)") {
		t.Errorf("the parent must name the child AND the reason: failing=%v message=%q", v.failing, v.message)
	}
	// The child's message embeds a growing duration, so it must NOT reach the parent's message: that
	// would rewrite the parent's Ready on every reconcile.
	if strings.Contains(v.message, "has not been Ready for") {
		t.Errorf("the parent's message must not copy the child's message (it embeds a duration): %q", v.message)
	}

	// Not sticky: once the vendor accepts, the child turns Ready=True and the parent recomputes.
	v = h.rollupManagedChildren(context.Background(), fakeDynWith(
		childComposition("refused", "True", "Available"),
		childComposition("ok", "True", "Available"),
	), mg)
	if !v.ready {
		t.Errorf("after the child turns Ready=True the parent must recompute to ready: reason=%s message=%q", v.reason, v.message)
	}
}

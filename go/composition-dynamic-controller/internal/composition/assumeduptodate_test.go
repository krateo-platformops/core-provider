package composition

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// oasgen-provider#709. A child whose Ready=True was ASSERTED (rest-dynamic-controller's
// assume-up-to-date path, reason AssumedUpToDate) is unevaluated, not healthy. Every other True
// stays healthy: the arm keys on the reason, so an observed True is unaffected.
func TestAssumedUpToDate_ChildIsUnevaluated(t *testing.T) {
	if got := krateoReady(krateoCR("True", "AssumedUpToDate", true)); got != childUnevaluated {
		t.Fatalf("Ready=True/AssumedUpToDate maps to %d, want childUnevaluated (%d): an asserted True is not evidence", got, childUnevaluated)
	}
	if got := krateoReady(krateoCR("True", "Available", true)); got != childHealthy {
		t.Errorf("control: Ready=True/Available maps to %d, want childHealthy (%d): only the asserted reason is excluded", got, childHealthy)
	}
}

// The parent's verdict. A composition whose ONLY child is asserted-up-to-date has no evidence, so
// it is not Ready (ZeroEvidence, Unknown); with an observed healthy sibling it is Ready on partial
// coverage. Before the arm, the asserted child alone made the parent Ready.
func TestAssumedUpToDate_ParentVerdict(t *testing.T) {
	child := func(name, reason string) *unstructured.Unstructured {
		c := krateoCR("True", reason, true)
		_ = unstructured.SetNestedField(c.Object, name, "metadata", "name")
		_ = unstructured.SetNestedField(c.Object, "ns", "metadata", "namespace")
		return c
	}
	ref := func(name string) map[string]any {
		return map[string]any{"apiVersion": "composition.krateo.io/v0-1-0", "resource": "portals", "name": name, "namespace": "ns"}
	}

	t.Run("only an asserted child: not Ready, zero evidence", func(t *testing.T) {
		mg := &unstructured.Unstructured{Object: map[string]any{}}
		setManaged(mg, ref("asserted"))
		v := (&handler{}).rollupManagedChildren(context.Background(), fakeDynWith(child("asserted", "AssumedUpToDate")), mg)
		if v.ready {
			t.Fatalf("parent ready=true (reason=%s %q) over a child whose health was only asserted", v.reason, v.message)
		}
	})
	t.Run("control: the same child observed Available makes the parent Ready", func(t *testing.T) {
		mg := &unstructured.Unstructured{Object: map[string]any{}}
		setManaged(mg, ref("observed"))
		v := (&handler{}).rollupManagedChildren(context.Background(), fakeDynWith(child("observed", "Available")), mg)
		if !v.ready {
			t.Fatalf("control failed: parent ready=false (reason=%s %q) over an observed healthy child", v.reason, v.message)
		}
	})
}

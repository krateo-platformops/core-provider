package composition

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// Diego, 2026-11-16: status.unevaluatedChildren says WHY each unevaluated child is unevaluated. Each
// cause is pinned to the classifier path that produces it, and every evaluated state carries none.
func TestUnevaluatedCauseOfEachPath(t *testing.T) {
	noReady := krateoCR("", "", false)
	_ = unstructured.SetNestedSlice(noReady.Object, []any{map[string]any{"type": "Synced", "status": "True"}}, "status", "conditions")
	vendorNoReady := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "trust.cert-manager.io/v1alpha1", "kind": "Bundle",
		"status": map[string]any{"conditions": []any{map[string]any{"type": "Synced", "status": "True", "reason": "Synced"}}}}}
	vendorNoConds := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "trust.cert-manager.io/v1alpha1", "kind": "Bundle"}}
	modelConfigNoAccepted := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "kagent.dev/v1alpha2", "kind": "ModelConfig",
		"status": map[string]any{"conditions": []any{map[string]any{"type": "Other", "status": "True"}}}}}

	cases := []struct {
		name      string
		group     string
		obj       *unstructured.Unstructured
		wantState childState
		wantCause unevaluatedCause
	}{
		{"krateo: no conditions", "composition.krateo.io", krateoCR("", "", false), childUnevaluated, causeNoReadyCondition},
		{"krateo: conditions without Ready", "composition.krateo.io", noReady, childUnevaluated, causeNoReadyCondition},
		{"krateo: Ready asserted (AssumedUpToDate)", "composition.krateo.io", krateoCR("True", "AssumedUpToDate", true), childUnevaluated, causeAssertedReady},
		{"krateo: NotObserved", "composition.krateo.io", krateoCR("Unknown", "NotObserved", true), childUnevaluated, causeNotObserved},
		{"krateo: ObserveFailed", "composition.krateo.io", krateoCR("False", "ObserveFailed", true), childUnevaluated, causeNotObserved},
		{"krateo: child at ZeroEvidence", "composition.krateo.io", krateoCR("Unknown", "ZeroEvidence", true), childUnevaluated, causeNoEvidenceBelow},
		{"vendor: conditions without Ready", "trust.cert-manager.io", vendorNoReady, childUnevaluated, causeNoReadyCondition},
		{"vendor: no conditions", "trust.cert-manager.io", vendorNoConds, childUnevaluated, causeNoReadyCondition},
		{"vendor kind arm: ModelConfig without Accepted", "kagent.dev", modelConfigNoAccepted, childUnevaluated, causeNoReadyCondition},
		{"control: observed healthy carries no cause", "composition.krateo.io", krateoCR("True", "Available", true), childHealthy, ""},
		{"control: failed carries no cause", "composition.krateo.io", krateoCR("False", "Unavailable", true), childFailed, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classifyChildEval(c.group, c.obj)
			if got.state != c.wantState || got.cause != c.wantCause {
				t.Fatalf("state=%d cause=%q, want state=%d cause=%q", got.state, got.cause, c.wantState, c.wantCause)
			}
			if classifyChild(c.group, c.obj) != got.state {
				t.Fatal("the childState wrapper disagrees with the evaluator it wraps")
			}
		})
	}
}

// The by-cause counts sum to the unevaluated count on EVERY rollup path, including a FAILING parent,
// the path where unevaluated used to be left at zero (so healthEvaluatedChildren counted unevaluated
// children as evaluated). The written object carries every cause, zero included.
func TestUnevaluatedChildrenSumToTheUnevaluatedCount(t *testing.T) {
	named := func(o *unstructured.Unstructured, name string) *unstructured.Unstructured {
		_ = unstructured.SetNestedField(o.Object, name, "metadata", "name")
		_ = unstructured.SetNestedField(o.Object, "ns", "metadata", "namespace")
		return o
	}
	ref := func(name string) map[string]any {
		return map[string]any{"apiVersion": "composition.krateo.io/v0-1-0", "resource": "portals", "name": name, "namespace": "ns"}
	}
	objs := []runtime.Object{
		named(krateoCR("True", "AssumedUpToDate", true), "asserted"),
		named(krateoCR("Unknown", "ZeroEvidence", true), "below"),
		named(krateoCR("", "", false), "noready"),
		named(krateoCR("False", "Unavailable", true), "broken"), // makes the parent FAIL
		named(krateoCR("True", "Available", true), "good"),
	}
	mg := &unstructured.Unstructured{Object: map[string]any{}}
	setManaged(mg, ref("asserted"), ref("below"), ref("noready"), ref("broken"), ref("good"))
	v := (&handler{}).rollupManagedChildren(context.Background(), fakeDynWith(objs...), mg)
	if v.ready {
		t.Fatalf("precondition: the parent must be failing (one child Unavailable), got ready")
	}
	if v.unevaluated != 3 {
		t.Fatalf("unevaluated=%d on a failing parent, want 3: it must be counted on every path", v.unevaluated)
	}
	got := unevaluatedChildren(v)
	want := map[string]int64{"noReadyCondition": 1, "assertedReady": 1, "notObserved": 0, "noEvidenceBelow": 1}
	sum := int64(0)
	for k, w := range want {
		g, ok := got[k].(int64)
		if !ok {
			t.Fatalf("status.unevaluatedChildren.%s missing or not an integer: %#v (zero must be written, not omitted)", k, got[k])
		}
		if g != w {
			t.Errorf("unevaluatedChildren.%s=%d, want %d", k, g, w)
		}
		sum += g
	}
	if len(got) != len(want) {
		t.Errorf("unevaluatedChildren has %d keys, want exactly %d: %v", len(got), len(want), got)
	}
	cov := coverageScalars(v)
	if int64(cov["managedChildrenCount"]-cov["healthEvaluatedChildren"]-cov["unreadableChildrenCount"]) != sum {
		t.Errorf("causes sum to %d, but managed-evaluated-unreadable = %d-%d-%d", sum,
			cov["managedChildrenCount"], cov["healthEvaluatedChildren"], cov["unreadableChildrenCount"])
	}
}

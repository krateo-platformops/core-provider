package composition

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// #574 (Diego, 2026-11-17): a child whose own controller could not observe it (Ready
// ObserveFailed/NotObserved) stays UNEVALUATED, so the parent's verdict does not move, but it is
// counted in status.unevaluatedChildren.notObserved and NAMED in the parent's Ready message. Before,
// a parent read Ready=True while such a child sat at Synced=False/ReconcileError, and nothing on the
// parent said so (measured on demo, krateo-keycloak-realm).
func TestRollup_UnobservableChildrenAreCountedAndNamed(t *testing.T) {
	child := func(name, status, reason string) *unstructured.Unstructured {
		c := krateoCR(status, reason, true)
		_ = unstructured.SetNestedField(c.Object, name, "metadata", "name")
		_ = unstructured.SetNestedField(c.Object, "ns", "metadata", "namespace")
		return c
	}
	ref := func(name string) map[string]any {
		return map[string]any{"apiVersion": "composition.krateo.io/v0-1-0", "resource": "portals", "name": name, "namespace": "ns"}
	}
	rollup := func(children ...*unstructured.Unstructured) (healthVerdict, readyOutcome) {
		mg := &unstructured.Unstructured{Object: map[string]any{}}
		var refs []any
		for _, c := range children {
			refs = append(refs, ref(c.GetName()))
		}
		_ = unstructured.SetNestedSlice(mg.Object, refs, "status", "managed")
		objs := make([]runtime.Object, 0, len(children))
		for _, c := range children {
			objs = append(objs, c)
		}
		v := (&handler{}).rollupManagedChildren(context.Background(), fakeDynWith(objs...), mg)
		return v, resolveReady(projectedHealth{}, readyMessageAvailable, v)
	}

	healthy := child("healthy", "True", "Available")
	v, out := rollup(healthy, child("obs-failed", "Unknown", "ObserveFailed"), child("not-observed", "Unknown", "NotObserved"), child("asserted", "True", "AssumedUpToDate"))

	if got := unevaluatedChildren(v)["notObserved"]; got != int64(2) {
		t.Errorf("status.unevaluatedChildren.notObserved = %v, want 2 (ObserveFailed and NotObserved)", got)
	}
	if got := unevaluatedChildren(v)["assertedReady"]; got != int64(1) {
		t.Errorf("control: assertedReady = %v, want 1", got)
	}
	if len(v.unobservable) != 2 || !strings.Contains(strings.Join(v.unobservable, ","), "obs-failed") || !strings.Contains(strings.Join(v.unobservable, ","), "not-observed") {
		t.Errorf("unobservable = %v, want exactly the ObserveFailed and NotObserved children", v.unobservable)
	}
	for _, want := range []string{"could NOT BE OBSERVED", "portals/ns/obs-failed", "portals/ns/not-observed", "2 of 4"} {
		if !strings.Contains(out.message, want) {
			t.Errorf("Ready message %q lacks %q", out.message, want)
		}
	}
	if strings.Contains(out.message, "portals/ns/asserted") {
		t.Errorf("an asserted-ready child is unevaluated but WAS observed; it must not be named as unobservable: %q", out.message)
	}

	// The verdict does not move: the same parent with those two children merely asserted (equally
	// unevaluated) reads the same reason and readiness, and names nobody.
	vc, outc := rollup(healthy, child("obs-failed", "True", "AssumedUpToDate"), child("not-observed", "True", "AssumedUpToDate"), child("asserted", "True", "AssumedUpToDate"))
	if vc.ready != v.ready || outc.reason != out.reason {
		t.Errorf("verdict moved: with unobservable children %v/%s, control %v/%s", v.ready, out.reason, vc.ready, outc.reason)
	}
	if strings.Contains(outc.message, "could NOT BE OBSERVED") {
		t.Errorf("control: no unobservable child, yet the message says so: %q", outc.message)
	}
}

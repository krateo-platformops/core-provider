package composition

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func corePod(phase string, readyCond *bool) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Pod"}}
	if phase != "" {
		_ = unstructured.SetNestedField(u.Object, phase, "status", "phase")
	}
	if readyCond != nil {
		st := "False"
		if *readyCond {
			st = "True"
		}
		_ = unstructured.SetNestedSlice(u.Object, []any{
			map[string]any{"type": "Ready", "status": st},
		}, "status", "conditions")
	}
	return u
}

func corePVC(phase string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "PersistentVolumeClaim"}}
	if phase != "" {
		_ = unstructured.SetNestedField(u.Object, phase, "status", "phase")
	}
	return u
}

// Tier 1 of krateo-core-provider#121: core/v1 kinds that actually carry readiness. Phase is
// authoritative for terminal outcomes; Running defers to the Ready condition because a Running Pod
// can still be failing its probe and serving nothing.
func TestClassifyChild_Pod(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		name string
		obj  *unstructured.Unstructured
		want childState
	}{
		{"succeeded is healthy, not unready", corePod("Succeeded", nil), childHealthy},
		{"failed is terminal", corePod("Failed", nil), childFailed},
		{"running + Ready=True", corePod("Running", &yes), childHealthy},
		{"running + Ready=False (probe failing)", corePod("Running", &no), childConverging},
		{"pending", corePod("Pending", nil), childConverging},
		{"unknown (node lost) can still recover", corePod("Unknown", nil), childConverging},
		// The asymmetry that keeps this safe, and the mistake krateoReady/jobReady each made:
		// a field the object does not carry is evidence of NOTHING, never of unhealth.
		{"running with NO Ready condition is not unhealth", corePod("Running", nil), childHealthy},
		{"no phase written yet is fail-safe", corePod("", nil), childHealthy},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyChild("", c.obj); got != c.want {
				t.Errorf("got %d want %d", got, c.want)
			}
		})
	}
}

// Pending is CONVERGING, deliberately: WaitForFirstConsumer leaves a claim Pending until a consumer
// Pod schedules, which is normal and self-resolving. Lost is terminal.
func TestClassifyChild_PVC(t *testing.T) {
	cases := []struct {
		name string
		obj  *unstructured.Unstructured
		want childState
	}{
		{"bound", corePVC("Bound"), childHealthy},
		{"lost is terminal", corePVC("Lost"), childFailed},
		{"pending (incl. WaitForFirstConsumer)", corePVC("Pending"), childConverging},
		{"no phase written yet is fail-safe", corePVC(""), childHealthy},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyChild("", c.obj); got != c.want {
				t.Errorf("got %d want %d", got, c.want)
			}
		})
	}
}

// The rest of core/v1 is CONFIG — it has no notion of being unready, so it must keep falling through
// to existence-is-health. Regressing this would flip every composition that ships a ConfigMap.
func TestClassifyChild_OtherCoreKindsUnchanged(t *testing.T) {
	for _, kind := range []string{"ConfigMap", "Secret", "Service", "ServiceAccount"} {
		t.Run(kind, func(t *testing.T) {
			u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": kind}}
			if got := classifyChild("", u); got != childHealthy {
				t.Errorf("%s: got %d, want healthy — config kinds carry no readiness", kind, got)
			}
		})
	}
}

// #121's specimen, and the honest limit of tier 1: a custom resource reporting failure through its
// OWN vocabulary (.status.phase, no conditions) is still healthy-by-existence. Tier 1 cannot fix
// this; only a tier-2 RESTAction on the CompositionDefinition can. Pinned so nobody mistakes tier 1
// for covering it.
func TestClassifyChild_CustomResourcePhaseStillNotEvaluated(t *testing.T) {
	mongo := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "mongodbcommunity.mongodb.com/v1", "kind": "MongoDBCommunity",
	}}
	_ = unstructured.SetNestedField(mongo.Object, "Failed", "status", "phase")

	// Reported as UNEVALUATED rather than silently healthy: tier 1 still does not interpret a
	// third-party .status.phase (that is tier 2's job), but the gap is now counted and surfaced
	// instead of disappearing.
	if got := classifyChild("mongodbcommunity.mongodb.com", mongo); got != childUnevaluated {
		t.Errorf("got %d, want childUnevaluated — tier 1 must not interpret a third-party "+
			".status.phase, but it must not hide that it did not either", got)
	}
}

// Unevaluated children must be COUNTED and SURFACED, but must never change the verdict — the whole
// point is to expose the gap without inventing a judgement (#121 tier 2).
func TestRollup_UnevaluatedIsReportedButNeverActedOn(t *testing.T) {
	h := &handler{}
	// A custom resource we cannot interpret, plus a ConfigMap that is legitimately nothing-to-assess.
	mongo := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "mongodbcommunity.mongodb.com/v1", "kind": "MongoDBCommunity",
		"metadata": map[string]any{"name": "m", "namespace": "ns"},
	}}
	_ = unstructured.SetNestedField(mongo.Object, "Failed", "status", "phase")
	cm := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": "c", "namespace": "ns"},
	}}

	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		{Group: "mongodbcommunity.mongodb.com", Version: "v1", Resource: "mongodbcommunities"}: "MongoDBCommunityList",
		{Group: "", Version: "v1", Resource: "configmaps"}:                                     "ConfigMapList",
	}, mongo, cm)

	mg := &unstructured.Unstructured{Object: map[string]any{}}
	_ = unstructured.SetNestedSlice(mg.Object, []any{
		map[string]any{"apiVersion": "mongodbcommunity.mongodb.com/v1", "resource": "mongodbcommunities", "name": "m", "namespace": "ns"},
		map[string]any{"apiVersion": "v1", "resource": "configmaps", "name": "c", "namespace": "ns"},
	}, "status", "managed")

	v := h.rollupManagedChildren(context.Background(), dyn, mg)

	// Verdict unchanged: still Available. An unevaluated child is NOT evidence of unhealth.
	if !v.ready || v.reason != "Available" {
		t.Fatalf("unevaluated children must not change the verdict; got ready=%v reason=%s", v.ready, v.reason)
	}
	// Only the custom resource counts — the ConfigMap has no readiness to evaluate, so it is not a gap.
	if v.unevaluated != 1 {
		t.Errorf("unevaluated = %d, want 1 (the custom resource only; a ConfigMap is not a gap)", v.unevaluated)
	}
	// And the gap is visible where people actually look.
	if !strings.Contains(v.message, "not health-evaluated") {
		t.Errorf("message must surface the gap, got %q", v.message)
	}
}

// With nothing unevaluated the message stays exactly as before — no new noise on compositions whose
// children are all assessable.
func TestRollup_MessageUnchangedWhenAllEvaluated(t *testing.T) {
	if got := availableMessage(0, 3); got != "Composition is up-to-date" {
		t.Errorf("got %q, want the unchanged message when nothing is unevaluated", got)
	}
	if got := availableMessage(2, 3); got != "Composition is up-to-date (2 of 3 managed children not health-evaluated)" {
		t.Errorf("unexpected qualified message: %q", got)
	}
}

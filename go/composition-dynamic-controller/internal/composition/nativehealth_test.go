package composition

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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

	if got := classifyChild("mongodbcommunity.mongodb.com", mongo); got != childHealthy {
		t.Errorf("got %d; tier 1 deliberately does NOT interpret a third-party .status.phase — "+
			"if this changed, #121's tier-2 design needs revisiting", got)
	}
}

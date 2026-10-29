package composition

import (
	"context"
	"errors"
	"github.com/krateo-platformops/plumbing/kubeutil/event"
	unstructuredtools "github.com/krateo-platformops/unstructured-runtime/pkg/tools/unstructured"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"strings"
	"testing"

	"github.com/krateo-platformops/composition-dynamic-controller/internal/snowplow"
	"github.com/krateo-platformops/unstructured-runtime/pkg/tools/statusprojection"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// fakeResolver is a test double for APIResolver.
type fakeResolver struct {
	api    map[string]any
	err    error
	gotMg  *unstructured.Unstructured
	called int
}

func (f *fakeResolver) Resolve(_ context.Context, mg *unstructured.Unstructured) (map[string]any, error) {
	f.called++
	f.gotMg = mg
	return f.api, f.err
}

func fireworksMg(gen int64) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "composition.krateo.io/v1-0-0",
		"kind":       "FireworksApp",
		"metadata":   map[string]any{"name": "demo", "namespace": "apps", "generation": gen, "uid": "uid-123"},
		"spec":       map[string]any{"host": "demo.example.com"},
	}}
}

// setStatus must feed the resolved apiRef map into the projection root under ".api" so
// declared mappings can read it.
func TestSetStatus_ProjectsAPISource(t *testing.T) {
	fr := &fakeResolver{api: map[string]any{
		"health": map[string]any{"status": "UP"},
		"svc":    map[string]any{"ip": "1.2.3.4"},
	}}
	h := &handler{
		apiResolver: fr,
		statusDataTemplate: []statusprojection.Mapping{
			{ForPath: "health", Expression: `${ .api.health.status }`},
			{ForPath: "endpoint", Expression: `${ .api.svc.ip }`},
		},
	}
	mg := fireworksMg(1)

	if err := h.setStatus(context.Background(), mg, &statusManagerOpts{
		chartURL: "u", chartVersion: "v", conditionType: ConditionTypeAvailable,
	}); err != nil {
		t.Fatalf("setStatus: %v", err)
	}
	if fr.called != 1 {
		t.Errorf("resolver called %d times, want 1", fr.called)
	}
	get := func(p string) any {
		v, _, _ := unstructured.NestedFieldNoCopy(mg.Object, "status", p)
		return v
	}
	if got := get("health"); got != "UP" {
		t.Errorf("status.health = %v, want UP", got)
	}
	if got := get("endpoint"); got != "1.2.3.4" {
		t.Errorf("status.endpoint = %v, want 1.2.3.4", got)
	}
}

// A resolver error is degrade-only: api-dependent fields skip, helm/built-in fields still
// project, and observedGeneration is still stamped.
func TestSetStatus_APIResolveError_Degrades(t *testing.T) {
	fr := &fakeResolver{err: errors.New("snowplow unreachable")}
	// A real recorder, because the failure path now emits a Warning event (#251) and production
	// always has one. A nil recorder here would be a test that does not represent the handler.
	rec := events.NewFakeRecorder(10)
	h := &handler{
		apiResolver:   fr,
		eventRecorder: *event.NewAPIRecorder(rec),
		statusDataTemplate: []statusprojection.Mapping{
			{ForPath: "health", Expression: `${ .api.health.status }`},
			{ForPath: "chartVersion", Expression: `${ .helm.version }`},
		},
	}
	mg := fireworksMg(4)

	if err := h.setStatus(context.Background(), mg, &statusManagerOpts{
		chartURL: "u", chartVersion: "9.9.9", conditionType: ConditionTypeAvailable,
	}); err != nil {
		t.Fatalf("setStatus: %v", err)
	}
	// api-dependent field carries no real data: with ".api" absent, the jq path yields null,
	// so the field is null rather than holding stale/invented data.
	if v, found, _ := unstructured.NestedFieldNoCopy(mg.Object, "status", "health"); found && v != nil {
		t.Errorf("status.health = %v, want nil/absent on resolve error", v)
	}
	// #251: the failure must be VISIBLE, not merely degraded. Before this the degrade was silent
	// — logged at Info, no condition, no event — so "authn was never installed here" looked
	// identical to a template that never set the field.
	// NOTE: unstructuredtools.GetCondition matches Type AND Reason, so passing "" for the reason
	// silently returns nil rather than "any reason" — an API that invites exactly this mistake.
	// Scan by type instead, since the point is that SOME ApiResolved condition exists.
	var cond *metav1.Condition
	for _, c := range unstructuredtools.GetConditions(mg) {
		if c.Type == ConditionTypeApiResolved {
			cc := c
			cond = &cc
		}
	}
	if cond == nil {
		t.Fatalf("no %s condition: the degrade is still silent", ConditionTypeApiResolved)
	}
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("%s status = %v, want False", ConditionTypeApiResolved, cond.Status)
	}
	if cond.Reason != reasonResolveFailed {
		t.Errorf("reason = %q, want %q (a non-authn failure)", cond.Reason, reasonResolveFailed)
	}
	// Ready must be untouched: the degrade is deliberate and a resolver failure must not make a
	// composition with a healthy release report unready.
	for _, c := range unstructuredtools.GetConditions(mg) {
		if c.Type == "Ready" && c.Status != metav1.ConditionTrue {
			t.Errorf("Ready = %v/%s, want it left True — ApiResolved must not regress readiness",
				c.Status, c.Reason)
		}
	}
	select {
	case ev := <-rec.Events:
		if !strings.Contains(ev, "Warning") || !strings.Contains(ev, "ApiRefUnresolved") {
			t.Errorf("event = %q, want a Warning/ApiRefUnresolved", ev)
		}
	default:
		t.Error("no event emitted: an operator watching events sees nothing")
	}

	// helm-sourced field still projected
	if v, _, _ := unstructured.NestedString(mg.Object, "status", "chartVersion"); v != "9.9.9" {
		t.Errorf("status.chartVersion = %q, want 9.9.9", v)
	}
	if v, _, _ := unstructured.NestedInt64(mg.Object, "status", "observedGeneration"); v != 4 {
		t.Errorf("observedGeneration = %d, want 4", v)
	}
}

// On the gracefully-paused path the resolver must not even be invoked (no projection occurs).
func TestSetStatus_APIResolver_NotCalledWhenPaused(t *testing.T) {
	fr := &fakeResolver{api: map[string]any{"health": map[string]any{"status": "UP"}}}
	h := &handler{
		apiResolver:        fr,
		statusDataTemplate: []statusprojection.Mapping{{ForPath: "health", Expression: `${ .api.health.status }`}},
	}
	if err := h.setStatus(context.Background(), fireworksMg(2), &statusManagerOpts{
		chartURL: "u", chartVersion: "v", conditionType: ConditionTypeReconcileGracefullyPaused,
	}); err != nil {
		t.Fatalf("setStatus: %v", err)
	}
	if fr.called != 0 {
		t.Errorf("resolver called %d times on paused path, want 0", fr.called)
	}
}

func TestMergedExtras_PerInstanceWins(t *testing.T) {
	r := NewSnowplowAPIResolver(nil, snowplow.ApiRef{Name: "x", Namespace: "y"}, map[string]any{
		"region":          "eu",
		"compositionName": "STATIC-SHOULD-LOSE",
	})
	ex := r.mergedExtras(fireworksMg(1))
	if ex["region"] != "eu" {
		t.Errorf("static extra region = %v, want eu", ex["region"])
	}
	if ex["compositionName"] != "demo" {
		t.Errorf("compositionName = %v, want demo (per-instance must win)", ex["compositionName"])
	}
	if ex["compositionNamespace"] != "apps" {
		t.Errorf("compositionNamespace = %v, want apps", ex["compositionNamespace"])
	}
	if ex["compositionId"] != "uid-123" {
		t.Errorf("compositionId = %v, want uid-123", ex["compositionId"])
	}
}

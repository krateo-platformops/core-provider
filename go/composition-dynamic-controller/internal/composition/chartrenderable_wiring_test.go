package composition

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/krateo-platformops/composition-dynamic-controller/internal/tools/archive"
)

// #331. THE WIRING, which did not exist and could not have.
//
// Two mutants survived the transition test: removing markChartRenderable from Create's RBAC path,
// and removing it from applyDerivedRBAC. Both survived for the same reason — the two paths were
// INLINE COPIES of each other, and neither could be reached by a test, so the success arm had to
// be wired twice and was verified in neither place.
//
// Consolidating Create onto applyDerivedRBAC is what makes this test possible at all:
// applyDerivedRBAC takes everything it needs as arguments, so a chart-inspector that answers and
// a fake dynamic client are enough. The duplication was not just untidy — it was the reason the
// wiring was unverifiable.
func TestApplyDerivedRBAC_MarksChartRenderableOnSuccess(t *testing.T) {
	// A chart-inspector that answers with one resource, so rbgen.Generate succeeds.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"group":"","version":"v1","resource":"configmaps","verbs":["*"]}]`))
	}))
	defer srv.Close()

	h := &handler{
		pluralizer:        stubPluralizerForRBAC{},
		chartInspectorUrl: srv.URL,
		saName:            "cdc",
		saNamespace:       "krateo-system",
	}
	mg := &unstructured.Unstructured{Object: map[string]any{}}
	mg.SetName("fw")
	mg.SetNamespace("krateo-system")
	mg.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "composition.krateo.io", Version: "v1-0-0", Kind: "Firework",
	})
	pkg := &archive.Info{CompositionDefinitionInfo: &archive.CompositionDefinitionInfo{
		Name: "fireworksapp", Namespace: "krateo-system",
		GVR: schema.GroupVersionResource{Group: "core.krateo.io", Version: "v1alpha1", Resource: "compositiondefinitions"},
	}}

	// The RBAC APPLY will fail against an empty fake client, and that is fine and deliberate: the
	// condition is stamped after Generate succeeds and before the apply, so this exercises exactly
	// the arm under test without needing a cluster that accepts RBAC writes.
	_ = h.applyDerivedRBAC(context.Background(), mg, pkg, fakeDynWith(), "fw-abcdefgh")

	status, reason, found := readChartRenderable(t, mg)
	if !found {
		t.Fatal("applyDerivedRBAC did not stamp ChartRenderable at all after a SUCCESSFUL render. " +
			"A condition that is only ever absent or False looks like one that is simply not " +
			"interesting yet, which is how #331 went unnoticed")
	}
	if status != string(metav1.ConditionTrue) || reason != reasonChartRenderable {
		t.Errorf("ChartRenderable = %s/%s after a successful render, want True/%s",
			status, reason, reasonChartRenderable)
	}
}

// And the failure arm through the same function, so the two are asserted at the same seam. Without
// this, a change that stamped True unconditionally would pass the test above.
func TestApplyDerivedRBAC_MarksChartRenderableFalseOnFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"no matches for kind","reason":"Invalid","code":422}`))
	}))
	defer srv.Close()

	h := &handler{
		pluralizer:        stubPluralizerForRBAC{},
		chartInspectorUrl: srv.URL,
		saName:            "cdc",
		saNamespace:       "krateo-system",
	}
	mg := &unstructured.Unstructured{Object: map[string]any{}}
	mg.SetName("fw")
	mg.SetNamespace("krateo-system")
	mg.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "composition.krateo.io", Version: "v1-0-0", Kind: "Firework",
	})
	pkg := &archive.Info{CompositionDefinitionInfo: &archive.CompositionDefinitionInfo{
		Name: "fireworksapp", Namespace: "krateo-system",
		GVR: schema.GroupVersionResource{Group: "core.krateo.io", Version: "v1alpha1", Resource: "compositiondefinitions"},
	}}

	if err := h.applyDerivedRBAC(context.Background(), mg, pkg, fakeDynWith(), "fw-abcdefgh"); err == nil {
		t.Fatal("applyDerivedRBAC returned nil on a 422 from chart-inspector")
	}

	status, reason, found := readChartRenderable(t, mg)
	if !found || status != string(metav1.ConditionFalse) {
		t.Fatalf("ChartRenderable found=%v status=%q after a 422, want False", found, status)
	}
	if reason != reasonUnservedKind {
		t.Errorf("reason = %q after a 422, want %q: the whole point of this condition is telling a "+
			"blueprint's own problem apart from chart-inspector being broken (#265)",
			reason, reasonUnservedKind)
	}
}

type stubPluralizerForRBAC struct{}

func (stubPluralizerForRBAC) GVKtoGVR(gvk schema.GroupVersionKind) (schema.GroupVersionResource, error) {
	return schema.GroupVersionResource{Group: gvk.Group, Version: gvk.Version, Resource: "fireworks"}, nil
}

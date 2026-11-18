package composition

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/krateo-platformops/composition-dynamic-controller/internal/chartinspector"
	"github.com/krateo-platformops/composition-dynamic-controller/internal/conditionreason"
	"github.com/krateo-platformops/composition-dynamic-controller/internal/tools/archive"
	unstructuredtools "github.com/krateo-platformops/unstructured-runtime/pkg/tools/unstructured"
	"github.com/krateo-platformops/unstructured-runtime/pkg/tools/unstructured/condition"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// condOf is the stored condition of type condType, whatever its reason.
func condOf(mg *unstructured.Unstructured, condType string) *metav1.Condition {
	for _, c := range unstructuredtools.GetConditions(mg) {
		if c.Type == condType {
			c := c
			return &c
		}
	}
	return nil
}

func fireworkFor577(t *testing.T, stored ...metav1.Condition) *unstructured.Unstructured {
	t.Helper()
	mg := &unstructured.Unstructured{Object: map[string]any{}}
	mg.SetName("fw")
	mg.SetNamespace("krateo-system")
	mg.SetGroupVersionKind(schema.GroupVersionKind{Group: "composition.krateo.io", Version: "v1-0-0", Kind: "Firework"})
	if len(stored) > 0 {
		if err := unstructuredtools.SetConditions(mg, stored...); err != nil {
			t.Fatal(err)
		}
	}
	return mg
}

var pkgFor577 = &archive.Info{CompositionDefinitionInfo: &archive.CompositionDefinitionInfo{
	Name: "fireworksapp", Namespace: "krateo-system",
	GVR: schema.GroupVersionResource{Group: "core.krateo.io", Version: "v1alpha1", Resource: "compositiondefinitions"},
}}

// rbacFailureFrom runs the real applyDerivedRBAC against a chart-inspector at url and returns its
// error: the chain Observe hands to failObserveOnDerivedRBAC, through rbacgen's wrapping.
func rbacFailureFrom(t *testing.T, mg *unstructured.Unstructured, url string) error {
	t.Helper()
	h := &handler{pluralizer: stubPluralizerForRBAC{}, chartInspectorUrl: url, saName: "cdc", saNamespace: "krateo-system"}
	_, err := h.applyDerivedRBAC(context.Background(), mg, pkgFor577, fakeDynWith(), "fw-abcdefgh")
	if err == nil {
		t.Fatal("applyDerivedRBAC succeeded against a failing chart-inspector")
	}
	return err
}

func refusedURL() string {
	srv := httptest.NewServer(http.NotFoundHandler())
	u := srv.URL
	srv.Close()
	return u
}

// #577. A refused chart-inspector reaches Observe still classified as unreachable (rbacgen keeps the
// chain), ChartRenderable says so, and RBACComplete is left as last assessed: nothing was assessed,
// and the grants applied then are still in place. CONTROL: a 500 is an answer, and RBACComplete
// records NotEvaluated at once, as before.
func TestApplyDerivedRBAC_UnreachableAssessesNothing(t *testing.T) {
	rbacComplete := metav1.Condition{Type: "RBACComplete", Status: metav1.ConditionTrue, Reason: "Complete", Message: "last pass", LastTransitionTime: metav1.NewTime(time.Now().Add(-time.Hour))}

	mg := fireworkFor577(t, rbacComplete)
	err := rbacFailureFrom(t, mg, refusedURL())
	if !chartinspector.IsUnreachable(err) {
		t.Fatalf("a refused connection reached Observe as %v, not unreachable", err)
	}
	if cr := condOf(mg, conditionreason.TypeChartRenderable); cr == nil || cr.Status != metav1.ConditionFalse || cr.Reason != conditionreason.ChartInspectorUnavailable.String() {
		t.Errorf("ChartRenderable = %+v, want False/%s", cr, conditionreason.ChartInspectorUnavailable)
	}
	if rc := condOf(mg, "RBACComplete"); rc == nil || rc.Status != metav1.ConditionTrue || rc.Message != "last pass" {
		t.Errorf("RBACComplete = %+v after an unreachable chart-inspector, want the last assessment kept", rc)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"render failed"}`, http.StatusInternalServerError)
	}))
	defer srv.Close()
	mg = fireworkFor577(t, rbacComplete)
	err = rbacFailureFrom(t, mg, srv.URL)
	if chartinspector.IsUnreachable(err) {
		t.Fatalf("control: a 500 read as unreachable: %v", err)
	}
	if rc := condOf(mg, "RBACComplete"); rc == nil || rc.Reason != conditionreason.RBACNotEvaluated.String() {
		t.Errorf("control: RBACComplete = %+v after a 500, want %s", rc, conditionreason.RBACNotEvaluated)
	}
}

// The grace decision, with every input state it distinguishes. ChartRenderable is written by the
// real writer (applyDerivedRBAC), so the anchor is whatever condition.Upsert keeps: the test does not
// get to pick the lastTransitionTime the decision reads.
func TestKeepReadyWhileInspectorUnreachable(t *testing.T) {
	ready := condition.Available()
	ready.LastTransitionTime = metav1.NewTime(time.Now().Add(-48 * time.Hour))
	old := metav1.NewTime(time.Now().Add(-10 * time.Minute).Truncate(time.Second)) // conditions store seconds

	unreachable := func(t *testing.T, stored ...metav1.Condition) (*unstructured.Unstructured, error) {
		mg := fireworkFor577(t, stored...)
		return mg, rbacFailureFrom(t, mg, refusedURL())
	}

	t.Run("first unreachable pass after a True ChartRenderable: kept, for 5 minutes", func(t *testing.T) {
		mg, err := unreachable(t, ready, metav1.Condition{Type: conditionreason.TypeChartRenderable, Status: metav1.ConditionTrue, Reason: conditionreason.ChartRenderable.String(), LastTransitionTime: old})
		now := time.Now()
		keep, deadline := keepReadyWhileInspectorUnreachable(mg, err, now)
		if !keep {
			t.Fatal("not kept on the first unreachable pass")
		}
		if d := deadline.Sub(now); d < 4*time.Minute || d > 5*time.Minute+time.Second {
			t.Errorf("deadline %s from now, want ~5m", d)
		}
		if keep, _ := keepReadyWhileInspectorUnreachable(mg, err, deadline.Add(time.Second)); keep {
			t.Error("still kept after the deadline")
		}
	})

	t.Run("ChartRenderable already False for another reason: the older anchor, so not kept", func(t *testing.T) {
		mg, err := unreachable(t, ready, metav1.Condition{Type: conditionreason.TypeChartRenderable, Status: metav1.ConditionFalse, Reason: conditionreason.ChartUnservedKind.String(), LastTransitionTime: old})
		if cr := condOf(mg, conditionreason.TypeChartRenderable); !cr.LastTransitionTime.Equal(&old) {
			t.Fatalf("premise: Upsert should keep the False transition's time %s, got %s", old, cr.LastTransitionTime)
		}
		if keep, _ := keepReadyWhileInspectorUnreachable(mg, err, time.Now()); keep {
			t.Error("kept on an anchor 10 minutes old")
		}
	})

	t.Run("no Ready stored: nothing to keep", func(t *testing.T) {
		mg, err := unreachable(t)
		if keep, _ := keepReadyWhileInspectorUnreachable(mg, err, time.Now()); keep {
			t.Error("kept a Ready that was never assessed")
		}
	})

	t.Run("an answer (500), not unreachable: not kept", func(t *testing.T) {
		mg := fireworkFor577(t, ready)
		_ = setChartRenderableCondition(mg, conditionreason.ChartInspectorUnavailable, "x")
		err := &chartinspector.StatusError{Code: 500}
		if keep, _ := keepReadyWhileInspectorUnreachable(mg, err, time.Now()); keep {
			t.Error("kept on a 500")
		}
	})

	t.Run("unreachable but ChartRenderable not written: no anchor, not kept", func(t *testing.T) {
		mg := fireworkFor577(t, ready)
		err := &chartinspector.UnreachableError{Err: errors.New("dial tcp: connection refused")}
		if keep, _ := keepReadyWhileInspectorUnreachable(mg, err, time.Now()); keep {
			t.Error("kept with no ChartRenderable anchor")
		}
	})
}

package composition

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// readChartRenderable returns the condition's status, reason and whether it is present at all.
// Presence is returned separately because ABSENT is a third state a consumer sees, and conflating
// it with False is how #331 read as "fine" on a composition that had never failed.
func readChartRenderable(t *testing.T, mg *unstructured.Unstructured) (status, reason string, found bool) {
	t.Helper()
	conds, ok, err := unstructured.NestedSlice(mg.Object, "status", "conditions")
	if err != nil {
		t.Fatalf("reading status.conditions: %v", err)
	}
	if !ok {
		return "", "", false
	}
	for _, c := range conds {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if cm["type"] != ConditionTypeChartRenderable {
			continue
		}
		s, _ := cm["status"].(string)
		r, _ := cm["reason"].(string)
		return s, r, true
	}
	return "", "", false
}

// #331. THE TRANSITION, which is the only thing that distinguishes the fix from the defect.
//
// Asserting "a successful render sets True" alone would pass against a version that never writes
// False. Asserting "a failed render sets False" alone is what the repository already had, and it
// passed for as long as the condition could never leave False. So the arms are asserted as a
// SEQUENCE on one object: fail, then recover.
//
// Measured on a live cluster by the openstack-ironic-operator-kog session before this existed:
// ChartRenderable stayed False/InspectorUnavailable with Ready and Synced both back to True
// beside it.
func TestChartRenderable_RecoversFromFalseToTrue(t *testing.T) {
	ctx := context.Background()
	mg := &unstructured.Unstructured{Object: map[string]any{}}

	// ABSENT first, and this is a real state: a composition that has never failed a render carries
	// no condition at all, so `absent` and not `True` is what a consumer sees on a healthy one.
	if _, _, found := readChartRenderable(t, mg); found {
		t.Fatal("the condition is present on an object nothing has stamped")
	}

	// Arm 1: a failure stamps False with a classifying reason.
	if err := setChartRenderableCondition(mg, metav1.ConditionFalse, reasonInspectorUnavailable,
		"chart-inspector failed to render the chart (HTTP 500)"); err != nil {
		t.Fatalf("setChartRenderableCondition: %v", err)
	}
	status, reason, found := readChartRenderable(t, mg)
	if !found || status != string(metav1.ConditionFalse) || reason != reasonInspectorUnavailable {
		t.Fatalf("after a failure: found=%v status=%q reason=%q; want True-absent, False, %q",
			found, status, reason, reasonInspectorUnavailable)
	}

	// Arm 2: THE ONE THAT DID NOT EXIST. A successful render must clear it.
	markChartRenderable(ctx, mg)
	status, reason, found = readChartRenderable(t, mg)
	if !found {
		t.Fatal("the condition vanished on recovery; it must say True, not go absent")
	}
	if status != string(metav1.ConditionTrue) {
		t.Errorf("after recovery status = %q, want True. A condition that cannot leave False is a "+
			"durable lie about current state, and an alert that never clears is one an operator "+
			"learns to ignore — which removes the value #265 added this condition for", status)
	}
	if reason != reasonChartRenderable {
		t.Errorf("after recovery reason = %q, want %q. That constant is documented as \"the render "+
			"succeeded\" and was emitted ZERO times before this fix", reason, reasonChartRenderable)
	}
}

// The success message must say what succeeded, because an operator who sees the condition flip
// should not have to read this source to learn which step it describes.
func TestChartRenderable_SuccessMessageNamesTheRender(t *testing.T) {
	mg := &unstructured.Unstructured{Object: map[string]any{}}
	markChartRenderable(context.Background(), mg)

	conds, _, _ := unstructured.NestedSlice(mg.Object, "status", "conditions")
	var msg string
	for _, c := range conds {
		if cm, ok := c.(map[string]any); ok && cm["type"] == ConditionTypeChartRenderable {
			msg, _ = cm["message"].(string)
		}
	}
	if !strings.Contains(msg, "render") {
		t.Errorf("success message = %q; it must name the render", msg)
	}
}

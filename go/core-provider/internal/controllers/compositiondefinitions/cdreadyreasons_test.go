package compositiondefinitions

import (
	"strings"
	"testing"
	"time"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/cdreason"
	rtv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
	"github.com/krateo-platformops/provider-runtime/pkg/reconciler"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// The served-version add (Diego, 2026-11-16): only a refused add (a held version and no room) is
// Ready=False/Unavailable. A bump in progress, waiting or not, keeps the last real Ready, or says
// Creating "update pending" when there is none. Each case names the reason AND a message fragment
// only that arm writes, so a case cannot pass on another arm's output.
func TestServedVersionAddReady(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1-2-4", Resource: "fireworksapps"}
	wait := &servedVersionAddWaits{gvr: gvr, waiting: []string{"v1-2-2"}, dueAt: time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)}
	tooLarge := &servedVersionAddTooLarge{gvr: gvr, bytes: 1_500_000, budget: servedVersionAddBudget,
		held: []compositiondefinitionsv1alpha1.HeldVersion{{Version: "v1-2-2", Reason: "helm release fw rev 3"}}}
	available := cdreason.Available.Condition()

	for _, c := range []struct {
		name       string
		start      *rtv1.Condition
		plan       servedVersionAddPlan
		wantReason rtv1.ConditionReason
		wantStatus metav1.ConditionStatus
		wantInMsg  string
	}{
		{"a bump keeps Available", &available, servedVersionAddPlan{}, rtv1.ReasonAvailable, metav1.ConditionTrue, ""},
		{"a bump waiting for a prune grace keeps Available", &available, servedVersionAddPlan{wait: wait}, rtv1.ReasonAvailable, metav1.ConditionTrue, ""},
		{"a bump with no earlier verdict is Creating, update pending", nil, servedVersionAddPlan{}, rtv1.ReasonCreating, metav1.ConditionFalse, "update pending: adding served version v1-2-4"},
		{"a wait with no earlier verdict is Creating and says why", nil, servedVersionAddPlan{wait: wait}, rtv1.ReasonCreating, metav1.ConditionFalse, "prune grace"},
		{"a refused add is Unavailable, over Available", &available, servedVersionAddPlan{tooLarge: tooLarge}, rtv1.ReasonUnavailable, metav1.ConditionFalse, "held by helm release fw rev 3"},
		{"a refused add is Unavailable with no earlier verdict", nil, servedVersionAddPlan{tooLarge: tooLarge}, rtv1.ReasonUnavailable, metav1.ConditionFalse, "budget"},
	} {
		t.Run(c.name, func(t *testing.T) {
			cr := &compositiondefinitionsv1alpha1.CompositionDefinition{}
			if c.start != nil {
				cr.SetConditions(*c.start)
			}
			servedVersionAddReady(cr, c.plan, gvr)
			got := cr.GetCondition(rtv1.TypeReady)
			if got.Reason != c.wantReason || got.Status != c.wantStatus {
				t.Fatalf("Ready = %s/%s (%q), want %s/%s", got.Status, got.Reason, got.Message, c.wantStatus, c.wantReason)
			}
			if !strings.Contains(got.Message, c.wantInMsg) {
				t.Errorf("Ready message %q does not contain %q", got.Message, c.wantInMsg)
			}
		})
	}
}

// contradictingSchema carries a `default` into the CRD that fails its own subschema: the shape
// plumbing#205's own doc gives, which the apiserver refused before crdgen checked it.
const contradictingSchema = `{"type":"object","properties":{"security":{"type":"object","default":{},` +
	`"required":["authentication"],"properties":{"authentication":{"type":"object"}}}}}`

// The SchemaDefaultContradiction verdict, and what it leaves alone. The control is the same chart
// shape with the contradiction removed: it must not stop Observe, or a pass here would say nothing.
func TestSchemaContradiction(t *testing.T) {
	gvk := schema.GroupVersionKind{Group: "composition.krateo.io", Version: "v1-0-0", Kind: "Fireworks"}

	t.Run("a carried default that fails its subschema: Ready=False/SchemaDefaultContradiction", func(t *testing.T) {
		cr := &compositiondefinitionsv1alpha1.CompositionDefinition{}
		// A definition that worked before: its status names the CRD it serves.
		cr.Status.ApiVersion, cr.Status.Kind, cr.Status.Resource = "composition.krateo.io/v0-9-0", "Fireworks", "fireworks"
		cr.SetConditions(cdreason.Available.Condition())
		before := cr.Status.DeepCopy()

		_, err, stop := schemaContradiction(cr, []byte(contradictingSchema), gvk)
		if !stop {
			t.Fatalf("not stopped (err %v): a schema no CRD can be generated from went on to the rest of Observe", err)
		}
		if !reconciler.IsReadyAssessed(err) {
			t.Errorf("error %v is not ReadyAssessed: the runtime would overwrite Ready with NotObserved", err)
		}
		got := cr.GetCondition(rtv1.TypeReady)
		if !cdreason.SchemaDefaultContradiction.Is(got) {
			t.Fatalf("Ready = %s/%s, want False/SchemaDefaultContradiction", got.Status, got.Reason)
		}
		t.Logf("message: %s", got.Message)
		if !strings.Contains(got.Message, "#/properties/security") {
			t.Errorf("Ready message %q does not name the JSON pointer", got.Message)
		}
		if err == nil || err.Error() != got.Message {
			t.Errorf("Synced would carry %v, Ready says %q: they must agree", err, got.Message)
		}
		if cr.Status.ApiVersion != before.ApiVersion || cr.Status.Kind != before.Kind || cr.Status.Resource != before.Resource {
			t.Errorf("status moved from %s %s %s to %s %s %s: nothing was generated, so it must keep its last pass",
				before.ApiVersion, before.Kind, before.Resource, cr.Status.ApiVersion, cr.Status.Kind, cr.Status.Resource)
		}
	})

	t.Run("control: the same shape without the contradiction does not stop", func(t *testing.T) {
		cr := &compositiondefinitionsv1alpha1.CompositionDefinition{}
		cr.SetConditions(cdreason.Available.Condition())
		clean := strings.Replace(contradictingSchema, `"required":["authentication"],`, "", 1)
		if clean == contradictingSchema {
			t.Fatal("the control did not remove the contradiction")
		}
		if _, err, stop := schemaContradiction(cr, []byte(clean), gvk); stop {
			t.Fatalf("stopped on a schema that generates (err %v)", err)
		}
		if got := cr.GetCondition(rtv1.TypeReady); !cdreason.Available.Is(got) {
			t.Errorf("Ready = %s/%s, want it left Available", got.Status, got.Reason)
		}
	})
}

package compositiondefinitions

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/tools/restactionrbac"
	rtv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func withApiRef() *compositiondefinitionsv1alpha1.CompositionDefinition {
	cr := &compositiondefinitionsv1alpha1.CompositionDefinition{}
	cr.Spec.ApiRef = &compositiondefinitionsv1alpha1.ApiReference{Name: "ra", Namespace: "ns"}
	return cr
}

func TestRecordApiRefRBAC_NamesTheCause(t *testing.T) {
	cr := withApiRef()
	// Wrapped, as Observe's path wraps it: the cause must survive that.
	err := fmt.Errorf("error deploying dynamic controller in dry-run mode: %w", restactionrbac.NotConfigured())

	recordApiRefRBAC(cr, err)

	c := cr.GetCondition(conditionTypeApiRefRBACComplete)
	if c.Status != metav1.ConditionFalse || c.Reason != "SnowplowNotConfigured" {
		t.Fatalf("got %s/%s", c.Status, c.Reason)
	}
	if !strings.Contains(c.Message, "CORE_PROVIDER_SNOWPLOW_URL") {
		t.Fatalf("the message does not say what to do: %q", c.Message)
	}
}

func TestRecordApiRefRBAC_TrueOnSuccess(t *testing.T) {
	cr := withApiRef()
	recordApiRefRBAC(cr, nil)
	if c := cr.GetCondition(conditionTypeApiRefRBACComplete); c.Status != metav1.ConditionTrue {
		t.Fatalf("got %s/%s", c.Status, c.Reason)
	}
}

// A False must clear by itself: the RESTAction appearing is the whole recovery.
func TestRecordApiRefRBAC_ClearsOnTheNextGoodPass(t *testing.T) {
	cr := withApiRef()
	recordApiRefRBAC(cr, restactionrbac.NotConfigured())
	recordApiRefRBAC(cr, nil)
	if c := cr.GetCondition(conditionTypeApiRefRBACComplete); c.Status != metav1.ConditionTrue {
		t.Fatalf("a recovered read-set still reads %s/%s", c.Status, c.Reason)
	}
}

// A Deploy can fail before this step. That says nothing about the read-set, in either direction.
func TestRecordApiRefRBAC_AnUnrelatedErrorChangesNothing(t *testing.T) {
	for name, seed := range map[string]error{"from True": nil, "from False": restactionrbac.NotConfigured()} {
		t.Run(name, func(t *testing.T) {
			cr := withApiRef()
			recordApiRefRBAC(cr, seed)
			before := cr.GetCondition(conditionTypeApiRefRBACComplete)

			recordApiRefRBAC(cr, errors.New("error creating ConfigMap for JSON schema"))

			after := cr.GetCondition(conditionTypeApiRefRBACComplete)
			if after.Status != before.Status || after.Reason != before.Reason {
				t.Fatalf("%s/%s became %s/%s", before.Status, before.Reason, after.Status, after.Reason)
			}
		})
	}
}

func TestRecordApiRefRBAC_RemovedWithTheApiRef(t *testing.T) {
	cr := withApiRef()
	cr.SetConditions(rtv1.Condition{Type: conditionTypeSchemaFullyEnforced, Status: metav1.ConditionTrue, Reason: reasonSchemaFullyEnforced})
	recordApiRefRBAC(cr, restactionrbac.NotConfigured())

	cr.Spec.ApiRef = nil
	recordApiRefRBAC(cr, nil)

	if n := len(cr.Status.Conditions); n != 1 {
		t.Fatalf("want only the unrelated condition left, got %d: %+v", n, cr.Status.Conditions)
	}
	if cr.Status.Conditions[0].Type != conditionTypeSchemaFullyEnforced {
		t.Fatalf("the wrong condition survived: %s", cr.Status.Conditions[0].Type)
	}
}

// The common case must stay byte-identical: no apiRef, no condition.
func TestRecordApiRefRBAC_AbsentWithoutAnApiRef(t *testing.T) {
	cr := &compositiondefinitionsv1alpha1.CompositionDefinition{}
	recordApiRefRBAC(cr, nil)
	recordApiRefRBAC(cr, errors.New("anything"))
	if len(cr.Status.Conditions) != 0 {
		t.Fatalf("a definition with no apiRef gained %+v", cr.Status.Conditions)
	}
}

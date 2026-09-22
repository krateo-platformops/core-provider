package compositiondefinitions

import (
	"testing"

	rtv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
)

// The regression for core-provider#131.
//
// A definition whose very first reconcile fails used to end up with Synced=False and no Ready
// condition at all. A sweep filtering `Ready != True` catches that; one filtering `Ready == False`
// reports a clean fleet — which is how a broken definition stayed invisible on 057 for five days.
func TestReadyIsSeededWhenAbsent(t *testing.T) {
	cr := &compositiondefinitionsv1alpha1.CompositionDefinition{}

	if got := cr.GetCondition(rtv1.TypeReady).Reason; got != "" {
		t.Fatalf("precondition: a fresh CR should carry no Ready reason, got %q", got)
	}

	seedReadyIfAbsent(cr)

	cond := cr.GetCondition(rtv1.TypeReady)
	if cond.Reason == "" {
		t.Fatal("Ready was not seeded: an early failure would leave the CR with no Ready condition")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("seeded Ready must be False, got %v", cond.Status)
	}
}

// Seeding must never overwrite a verdict the controller already reached — especially Ready=True,
// which would make a healthy definition flap to False on every reconcile.
func TestReadyIsNotOverwrittenWhenPresent(t *testing.T) {
	cases := []struct {
		name string
		cond rtv1.Condition
	}{
		{"healthy", rtv1.Available()},
		{"failed", rtv1.Unavailable()},
		{"deleting", rtv1.Deleting()},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cr := &compositiondefinitionsv1alpha1.CompositionDefinition{}
			cr.SetConditions(tc.cond)

			seedReadyIfAbsent(cr)

			got := cr.GetCondition(rtv1.TypeReady)
			if got.Reason != tc.cond.Reason {
				t.Errorf("existing Ready was overwritten: want reason %q, got %q", tc.cond.Reason, got.Reason)
			}
			if got.Status != tc.cond.Status {
				t.Errorf("existing Ready status changed: want %v, got %v", tc.cond.Status, got.Status)
			}
		})
	}
}

// Seeding is idempotent: the second call must not disturb what the first wrote.
func TestSeedingIsIdempotent(t *testing.T) {
	cr := &compositiondefinitionsv1alpha1.CompositionDefinition{}

	seedReadyIfAbsent(cr)
	first := cr.GetCondition(rtv1.TypeReady)

	seedReadyIfAbsent(cr)
	second := cr.GetCondition(rtv1.TypeReady)

	if first.Reason != second.Reason || first.Status != second.Status {
		t.Errorf("seeding is not idempotent: first %v/%v, second %v/%v",
			first.Reason, first.Status, second.Reason, second.Status)
	}

	// Exactly one Ready condition — seeding twice must not append a duplicate.
	n := 0
	for _, c := range cr.Status.Conditions {
		if c.Type == rtv1.TypeReady {
			n++
		}
	}
	if n != 1 {
		t.Errorf("expected exactly one Ready condition, got %d", n)
	}
}

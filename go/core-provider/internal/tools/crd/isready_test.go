package crd

import (
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// IsReady had no test and no caller until #269. It is now the gate on a CompositionDefinition
// reporting Available, so its contract is load-bearing: Available must mean "the API answers",
// not "I wrote a CRD".
func TestIsReady(t *testing.T) {
	withConds := func(cs ...apiextensionsv1.CustomResourceDefinitionCondition) *apiextensionsv1.CustomResourceDefinition {
		return &apiextensionsv1.CustomResourceDefinition{
			ObjectMeta: metav1.ObjectMeta{Name: "fireworks.composition.krateo.io"},
			Status:     apiextensionsv1.CustomResourceDefinitionStatus{Conditions: cs},
		}
	}
	cond := func(t apiextensionsv1.CustomResourceDefinitionConditionType, s apiextensionsv1.ConditionStatus) apiextensionsv1.CustomResourceDefinitionCondition {
		return apiextensionsv1.CustomResourceDefinitionCondition{Type: t, Status: s}
	}

	for _, tc := range []struct {
		name string
		crd  *apiextensionsv1.CustomResourceDefinition
		want bool
		why  string
	}{
		{
			// THE LOAD-BEARING FIXTURE, and the reason this is not a no-conditions test. A real
			// apiserver passes through NamesAccepted=True with no Established condition yet. A test
			// that only used a CRD with NO conditions would pass against an implementation that
			// merely checked len(Conditions) > 0, which is the plausible wrong gate.
			name: "accepted but not yet established",
			crd:  withConds(cond(apiextensionsv1.NamesAccepted, apiextensionsv1.ConditionTrue)),
			want: false,
			why:  "NamesAccepted is not Established; the API does not answer yet",
		},
		{
			// The state a names collision leaves behind, where Established may NEVER arrive.
			name: "names rejected",
			crd: withConds(
				cond(apiextensionsv1.NamesAccepted, apiextensionsv1.ConditionFalse),
				cond(apiextensionsv1.Established, apiextensionsv1.ConditionFalse),
			),
			want: false,
			why:  "a CRD whose names were rejected never becomes servable",
		},
		{
			name: "established true",
			crd: withConds(
				cond(apiextensionsv1.NamesAccepted, apiextensionsv1.ConditionTrue),
				cond(apiextensionsv1.Established, apiextensionsv1.ConditionTrue),
			),
			want: true,
			why:  "the only state in which the API answers",
		},
		{
			// Established present but NOT True must be false. Keying on the type alone is the
			// other plausible wrong gate.
			name: "established present but false",
			crd:  withConds(cond(apiextensionsv1.Established, apiextensionsv1.ConditionFalse)),
			want: false,
			why:  "the condition's presence is not its value",
		},
		{
			name: "established unknown",
			crd:  withConds(cond(apiextensionsv1.Established, apiextensionsv1.ConditionUnknown)),
			want: false,
			why:  "Unknown is not True; an unknown state must not read as ready",
		},
		{
			name: "no conditions at all",
			crd:  withConds(),
			want: false,
			why:  "a freshly admitted CRD, before the apiserver has said anything",
		},
		{
			// Must not panic. Observe's caller checks crd == nil separately and reports Unavailable,
			// but a predicate that panics on nil is one refactor away from a crash.
			name: "nil crd",
			crd:  nil,
			want: false,
			why:  "no CRD is not a ready CRD, and must not panic",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsReady(tc.crd); got != tc.want {
				t.Errorf("IsReady = %v, want %v — %s", got, tc.want, tc.why)
			}
		})
	}
}

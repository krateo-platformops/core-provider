package compositiondefinitions

import (
	"fmt"
	"testing"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// #494: which versions stay served beside the one being added and are held. One arm per exclusion.
func TestHeldBeside(t *testing.T) {
	live := &apiextensionsv1.CustomResourceDefinition{Spec: apiextensionsv1.CustomResourceDefinitionSpec{
		Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
			{Name: "vacuum", Served: false, Storage: true},
			{Name: "v1", Served: true}, {Name: "v2", Served: true}, {Name: "v3", Served: true}, {Name: "v4", Served: false},
		}}}
	hv := func(pairs ...string) []compositiondefinitionsv1alpha1.HeldVersion {
		var out []compositiondefinitionsv1alpha1.HeldVersion
		for i := 0; i < len(pairs); i += 2 {
			out = append(out, compositiondefinitionsv1alpha1.HeldVersion{Version: pairs[i], Reason: pairs[i+1]})
		}
		return out
	}
	for _, tc := range []struct {
		name string
		own  string
		drop map[string]bool
		held []compositiondefinitionsv1alpha1.HeldVersion
		want string
	}{
		{"a held transitional version stays", "v1", nil, hv("v2", "release-revisions=1:[default/parent(rev 3)]"), "[{v2 release-revisions=1:[default/parent(rev 3)]}]"},
		{"the definition's own version is not counted", "v1", nil, hv("v1", "instances=1"), "[]"},
		{"a version this write drops is not counted", "v1", map[string]bool{"v2": true}, hv("v2", "referenced"), "[]"},
		{"an unserved version is not counted", "v1", nil, hv("v4", "instances=2"), "[]"},
		{"nothing held, nothing counted", "v1", nil, nil, "[]"},
		{"unaccounted instances hold every served version but own", "v1", nil, hv("*", "unaccounted-instances=2:[a b]"),
			"[{v2 unaccounted-instances=2:[a b]} {v3 unaccounted-instances=2:[a b]}]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := heldBeside(live, tc.own, "v9", tc.drop, tc.held)
			if s := fmt.Sprint(got); s != tc.want && !(tc.want == "[]" && len(got) == 0) {
				t.Errorf("heldBeside = %s, want %s", s, tc.want)
			}
		})
	}
}

// refusal() is what Observe states and what applyCompositionCRD returns: the wait when there is one,
// the too-large refusal otherwise, nil when the write may go ahead.
func TestServedVersionAddPlanRefusal(t *testing.T) {
	w := &servedVersionAddWaits{}
	r := &servedVersionAddTooLarge{}
	if got := (servedVersionAddPlan{wait: w}).refusal(); got != error(w) {
		t.Errorf("wait plan refusal = %v", got)
	}
	if got := (servedVersionAddPlan{tooLarge: r}).refusal(); got != error(r) {
		t.Errorf("too-large plan refusal = %v", got)
	}
	if got := (servedVersionAddPlan{drop: map[string]bool{"v1": true}}).refusal(); got != nil {
		t.Errorf("a plan that writes refused: %v", got)
	}
}

package compositiondefinitions

import (
	"testing"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// #313: where a definition's controller lives. The management namespace is deliberately not the
// definition's namespace in these tests, because with equal values a placement bug is invisible.
func TestControllerNamespace(t *testing.T) {
	prev := ManagementNamespace
	ManagementNamespace = "krateo-mgmt"
	t.Cleanup(func() { ManagementNamespace = prev })

	local := &compositiondefinitionsv1alpha1.CompositionDefinition{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "apps"}}
	remote := &compositiondefinitionsv1alpha1.CompositionDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "apps"},
		Spec: compositiondefinitionsv1alpha1.CompositionDefinitionSpec{
			Deploy: &compositiondefinitionsv1alpha1.DeploymentTarget{
				TargetRef: &compositiondefinitionsv1alpha1.TargetReference{Name: "spoke"},
			},
		},
	}
	follower := &compositiondefinitionsv1alpha1.CompositionDefinition{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "other"}}

	for name, tc := range map[string]struct{ got, want string }{
		"a local definition's controller lives in the management namespace": {controllerNamespace(local), "krateo-mgmt"},
		"a remote definition's controller is unchanged until step 3":        {controllerNamespace(remote), "apps"},
		"a follower's controller is its owner's":                            {currentControllerNamespace(follower, local), "krateo-mgmt"},
		"an owner's controller is its own":                                  {currentControllerNamespace(local, nil), "krateo-mgmt"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s: got %q, want %q", name, tc.got, tc.want)
		}
	}
}

// #313: where retirement and relocation look. With the management namespace different from the
// definition's, a local definition has two placements; a remote one, or one already in the
// management namespace, has one, so relocation is a no-op for it.
func TestControllerPlacements(t *testing.T) {
	prev := ManagementNamespace
	ManagementNamespace = "krateo-mgmt"
	t.Cleanup(func() { ManagementNamespace = prev })

	inApps := &compositiondefinitionsv1alpha1.CompositionDefinition{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "apps"}}
	inMgmt := &compositiondefinitionsv1alpha1.CompositionDefinition{ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "krateo-mgmt"}}
	remote := &compositiondefinitionsv1alpha1.CompositionDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "apps"},
		Spec: compositiondefinitionsv1alpha1.CompositionDefinitionSpec{Deploy: &compositiondefinitionsv1alpha1.DeploymentTarget{
			TargetRef: &compositiondefinitionsv1alpha1.TargetReference{Name: "spoke"},
		}},
	}
	for name, tc := range map[string]struct {
		cr     *compositiondefinitionsv1alpha1.CompositionDefinition
		want   []string
		legacy string
	}{
		"local, outside the management namespace":    {inApps, []string{"krateo-mgmt", "apps"}, "apps"},
		"local, already in the management namespace": {inMgmt, []string{"krateo-mgmt"}, ""},
		"remote (not moved until step 3)":            {remote, []string{"apps"}, ""},
	} {
		got := controllerPlacements(tc.cr)
		if len(got) != len(tc.want) || got[0] != tc.want[0] || (len(got) == 2 && got[1] != tc.want[1]) {
			t.Errorf("%s: placements %v, want %v", name, got, tc.want)
		}
		if l := legacyControllerNamespace(tc.cr); l != tc.legacy {
			t.Errorf("%s: legacy namespace %q, want %q", name, l, tc.legacy)
		}
	}
}

// #313: the hand-over decisions. #404's first integration run showed what comparing definition
// NAMESPACES does after the move: a follower in another namespace "handed over" by undeploying the
// shared management-namespace controller, the owner's live one. They must compare PLACEMENTS.
func TestHandoverDecisionsComparePlacements(t *testing.T) {
	prev := ManagementNamespace
	ManagementNamespace = "krateo-mgmt"
	t.Cleanup(func() { ManagementNamespace = prev })

	cd := func(name, ns string) *compositiondefinitionsv1alpha1.CompositionDefinition {
		return &compositiondefinitionsv1alpha1.CompositionDefinition{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	}
	ownerApps, followerTeam, siblingApps, inMgmt := cd("a", "apps"), cd("b", "team"), cd("c", "apps"), cd("d", "krateo-mgmt")

	if !sharePlacement(followerTeam, ownerApps) {
		t.Error("two local definitions in different namespaces must share the management-namespace placement")
	}
	if sharePlacement(followerTeam, nil) {
		t.Error("nothing shares a placement with no definition")
	}
	for name, tc := range map[string]struct {
		cr, other *compositiondefinitionsv1alpha1.CompositionDefinition
		want      string
	}{
		"legacy copy, successor elsewhere: cr's own":          {ownerApps, followerTeam, "apps"},
		"legacy copy, sibling in the SAME namespace: shared":  {ownerApps, siblingApps, ""},
		"no other definition: cr's own":                       {ownerApps, nil, "apps"},
		"cr in the management namespace: no legacy placement": {inMgmt, ownerApps, ""},
	} {
		if got := ownLegacyPlacement(tc.cr, tc.other); got != tc.want {
			t.Errorf("%s: ownLegacyPlacement = %q, want %q", name, got, tc.want)
		}
	}
}

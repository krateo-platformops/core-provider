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

package compositiondefinitions

import (
	"testing"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
)

func TestScaleToZeroOptedIn(t *testing.T) {
	yes, no := true, false
	remote := &compositiondefinitionsv1alpha1.DeploymentTarget{TargetRef: &compositiondefinitionsv1alpha1.TargetReference{Name: "spoke"}}
	cd := func(stz *bool, deploy *compositiondefinitionsv1alpha1.DeploymentTarget) *compositiondefinitionsv1alpha1.CompositionDefinition {
		c := &compositiondefinitionsv1alpha1.CompositionDefinition{}
		c.Spec.Deploy = deploy
		if stz != nil {
			c.Spec.Controller = &compositiondefinitionsv1alpha1.ControllerConfig{ScaleToZero: stz}
		}
		return c
	}
	for _, c := range []struct {
		name string
		cd   *compositiondefinitionsv1alpha1.CompositionDefinition
		want bool
	}{
		{"local, true", cd(&yes, nil), true},
		{"local, false", cd(&no, nil), false},
		{"local, unset", cd(nil, nil), false},
		{"no controller block", &compositiondefinitionsv1alpha1.CompositionDefinition{}, false},
		{"remote, true (stored before the admission rule)", cd(&yes, remote), false},
		{"empty targetRef name is local", cd(&yes, &compositiondefinitionsv1alpha1.DeploymentTarget{TargetRef: &compositiondefinitionsv1alpha1.TargetReference{}}), true},
		{"nil definition", nil, false},
	} {
		if got := scaleToZeroOptedIn(c.cd); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

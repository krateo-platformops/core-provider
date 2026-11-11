package compositiondefinitions

import (
	"errors"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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

func TestControllerReadiness(t *testing.T) {
	yes, no := true, false
	r := func(n int32) *appsv1.Deployment { return &appsv1.Deployment{Spec: appsv1.DeploymentSpec{Replicas: &n}} }
	seen := metav1.NewTime(time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC))
	cd := func(stz *bool, st *compositiondefinitionsv1alpha1.ScaleToZeroStatus) *compositiondefinitionsv1alpha1.CompositionDefinition {
		c := &compositiondefinitionsv1alpha1.CompositionDefinition{}
		if stz != nil {
			c.Spec.Controller = &compositiondefinitionsv1alpha1.ControllerConfig{ScaleToZero: stz}
		}
		c.Status.ScaleToZero = st
		return c
	}
	for _, c := range []struct {
		name                       string
		cd                         *compositiondefinitionsv1alpha1.CompositionDefinition
		dep                        *appsv1.Deployment
		err                        error
		ready, readyReason         string
		active, activeReason, msgs string
	}{
		{"running, not opted in", cd(nil, nil), r(1), nil, "True", "Available", "True", "Running", "controller running"},
		{"running, opted in", cd(&yes, nil), r(1), nil, "True", "Available", "True", "Running", "scales to zero after 10m0s"},
		{"hold-down", cd(&yes, &compositiondefinitionsv1alpha1.ScaleToZeroStatus{Reason: "IdleHoldDown", LastSeen: &seen}), r(1), nil,
			"True", "Available", "True", "Running", "scales to zero at 2026-11-11T09:10:00Z"},
		{"held by a sibling", cd(&yes, &compositiondefinitionsv1alpha1.ScaleToZeroStatus{HeldBy: []string{"default/b"}}), r(1), nil,
			"True", "Available", "True", "Running", "kept running: default/b"},
		{"declared zero", cd(&yes, &compositiondefinitionsv1alpha1.ScaleToZeroStatus{Reason: "Idle", LastSeen: &seen}), r(0), nil,
			"True", "ScaledToZero", "False", "ScaledToZero", "since 2026-11-11T09:00:00Z; wakes on the first"},
		{"undeclared zero (not opted in)", cd(&no, nil), r(0), nil, "False", "Unavailable", "False", "Unavailable", "has not opted into scale-to-zero"},
		{"Deployment unreadable", cd(&yes, nil), nil, errors.New("boom"), "True", "Available", "Unknown", "NotObserved", "could not be read: boom"},
		{"Deployment absent", cd(&yes, nil), nil, nil, "True", "Available", "Unknown", "NotObserved", "not found"},
		{"nil replicas defaults to 1", cd(&yes, nil), &appsv1.Deployment{}, nil, "True", "Available", "True", "Running", "controller running"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ready, active := controllerReadiness(c.cd, "FireworksApp", c.dep, c.err)
			if string(ready.Status) != c.ready || string(ready.Reason) != c.readyReason {
				t.Errorf("Ready = %s/%s, want %s/%s", ready.Status, ready.Reason, c.ready, c.readyReason)
			}
			if string(active.Type) != "ControllerActive" || string(active.Status) != c.active || string(active.Reason) != c.activeReason {
				t.Errorf("ControllerActive = %s %s/%s, want ControllerActive %s/%s", active.Type, active.Status, active.Reason, c.active, c.activeReason)
			}
			if !strings.Contains(active.Message, c.msgs) {
				t.Errorf("ControllerActive message %q lacks %q", active.Message, c.msgs)
			}
			// Stable while the state is: a second evaluation must be Equal, or SetConditions rewrites it.
			r2, a2 := controllerReadiness(c.cd, "FireworksApp", c.dep, c.err)
			if !ready.Equal(r2) || !active.Equal(a2) {
				t.Errorf("not stable across two passes: %+v vs %+v", active, a2)
			}
		})
	}
}

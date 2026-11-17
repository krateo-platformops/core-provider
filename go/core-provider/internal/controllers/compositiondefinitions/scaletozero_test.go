package compositiondefinitions

import (
	"errors"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
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
	// r is a Deployment of n replicas that has rolled out: the state Available requires (Diego,
	// 2026-11-16). TestControllerRollout covers the states before and after it.
	r := func(n int32) *appsv1.Deployment { return rolledOut(n) }
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
		// Unreadable: the caller returns the error from Observe (NotObserved); the Ready here is unused.
		{"Deployment unreadable", cd(&yes, nil), nil, errors.New("boom"), "False", "Creating", "Unknown", "NotObserved", "could not be read: boom"},
		{"Deployment absent", cd(&yes, nil), nil, nil, "False", "Creating", "Unknown", "NotObserved", "not found"},
		{"nil replicas defaults to 1", cd(&yes, nil), func() *appsv1.Deployment { d := rolledOut(1); d.Spec.Replicas = nil; return d }(), nil, "True", "Available", "True", "Running", "controller running"},
		{"nil replicas, nothing rolled out: not Available", cd(&yes, nil), &appsv1.Deployment{}, nil, "False", "Creating", "True", "Running", "controller running"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ready, active := controllerReadiness(c.cd, "FireworksApp", c.dep, c.err, nil)
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
			r2, a2 := controllerReadiness(c.cd, "FireworksApp", c.dep, c.err, nil)
			if !ready.Equal(r2) || !active.Equal(a2) {
				t.Errorf("not stable across two passes: %+v vs %+v", active, a2)
			}
		})
	}
}

// rolledOut is a Deployment of n replicas whose rollout completed: generation observed, every replica
// updated and available, Progressing=True/NewReplicaSetAvailable and Available=True.
func rolledOut(n int32) *appsv1.Deployment {
	d := &appsv1.Deployment{Spec: appsv1.DeploymentSpec{Replicas: &n}}
	d.Name, d.Namespace, d.Generation = "fireworksapps-v1-0-0-controller", "krateo-system", 2
	d.Status = appsv1.DeploymentStatus{
		ObservedGeneration: 2, Replicas: n, UpdatedReplicas: n, AvailableReplicas: n, ReadyReplicas: n,
		Conditions: []appsv1.DeploymentCondition{
			{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionTrue, Reason: "NewReplicaSetAvailable"},
			{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue, Reason: "MinimumReplicasAvailable"},
		},
	}
	return d
}

func setCond(d *appsv1.Deployment, t appsv1.DeploymentConditionType, s corev1.ConditionStatus, reason, msg string) {
	for i := range d.Status.Conditions {
		if d.Status.Conditions[i].Type == t {
			d.Status.Conditions[i] = appsv1.DeploymentCondition{Type: t, Status: s, Reason: reason, Message: msg}
			return
		}
	}
	d.Status.Conditions = append(d.Status.Conditions, appsv1.DeploymentCondition{Type: t, Status: s, Reason: reason, Message: msg})
}

// Available means the controller is up (Diego, 2026-11-16), and not-available splits on the
// Deployment's own verdict (frontend's catch): a rollout in progress converges, a rollout given up on
// or availability lost after one completed fails. Each case names a message fragment only its arm
// writes, and every case is evaluated twice for message stability.
func TestControllerRollout(t *testing.T) {
	cd := &compositiondefinitionsv1alpha1.CompositionDefinition{}
	for _, c := range []struct {
		name       string
		dep        func() *appsv1.Deployment
		wantReason string
		wantStatus metav1.ConditionStatus
		wantInMsg  string
	}{
		{"rolled out: Available", func() *appsv1.Deployment { return rolledOut(1) }, "Available", metav1.ConditionTrue, ""},
		{"new spec not yet observed: rolling out", func() *appsv1.Deployment { d := rolledOut(1); d.Generation = 3; return d },
			"Creating", metav1.ConditionFalse, "is rolling out"},
		{"just created, no status: rolling out", func() *appsv1.Deployment {
			n := int32(1)
			d := &appsv1.Deployment{Spec: appsv1.DeploymentSpec{Replicas: &n}}
			d.Name, d.Namespace, d.Generation = "x", "krateo-system", 1
			return d
		}, "Creating", metav1.ConditionFalse, "is rolling out"},
		{"old pod still present: rolling out", func() *appsv1.Deployment {
			d := rolledOut(1)
			d.Status.Replicas = 2
			setCond(d, appsv1.DeploymentProgressing, corev1.ConditionTrue, "ReplicaSetUpdated", "")
			return d
		}, "Creating", metav1.ConditionFalse, "is rolling out"},
		{"new pod not yet available, rollout running: rolling out", func() *appsv1.Deployment {
			d := rolledOut(1)
			d.Status.AvailableReplicas = 0
			setCond(d, appsv1.DeploymentProgressing, corev1.ConditionTrue, "ReplicaSetUpdated", "")
			setCond(d, appsv1.DeploymentAvailable, corev1.ConditionFalse, "MinimumReplicasUnavailable", "Deployment does not have minimum availability.")
			return d
		}, "Creating", metav1.ConditionFalse, "is rolling out"},
		{"rollout gave up: Unavailable, naming ProgressDeadlineExceeded", func() *appsv1.Deployment {
			d := rolledOut(1)
			d.Status.AvailableReplicas = 0
			setCond(d, appsv1.DeploymentProgressing, corev1.ConditionFalse, "ProgressDeadlineExceeded", `ReplicaSet "x-1" has timed out progressing.`)
			return d
		}, "Unavailable", metav1.ConditionFalse, `gave up its rollout: ProgressDeadlineExceeded: ReplicaSet "x-1" has timed out progressing.`},
		{"availability lost after a completed rollout: Unavailable", func() *appsv1.Deployment {
			d := rolledOut(1)
			d.Status.AvailableReplicas = 0
			setCond(d, appsv1.DeploymentAvailable, corev1.ConditionFalse, "MinimumReplicasUnavailable", "Deployment does not have minimum availability.")
			return d
		}, "Unavailable", metav1.ConditionFalse, "lost availability after a completed rollout: MinimumReplicasUnavailable: Deployment does not have minimum availability."},
		{"counts complete but Available=False: Unavailable", func() *appsv1.Deployment {
			d := rolledOut(1)
			setCond(d, appsv1.DeploymentAvailable, corev1.ConditionFalse, "MinimumReplicasUnavailable", "m")
			return d
		}, "Unavailable", metav1.ConditionFalse, "lost availability after a completed rollout: MinimumReplicasUnavailable: m"},
		{"counts complete, no Available condition yet: rolling out", func() *appsv1.Deployment {
			d := rolledOut(1)
			d.Status.Conditions = d.Status.Conditions[:1]
			return d
		}, "Creating", metav1.ConditionFalse, "is rolling out"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ready, _ := controllerReadiness(cd, "FireworksApp", c.dep(), nil, nil)
			if string(ready.Reason) != c.wantReason || ready.Status != c.wantStatus {
				t.Fatalf("Ready = %s/%s (%q), want %s/%s", ready.Status, ready.Reason, ready.Message, c.wantStatus, c.wantReason)
			}
			if !strings.Contains(ready.Message, c.wantInMsg) {
				t.Errorf("Ready message %q lacks %q", ready.Message, c.wantInMsg)
			}
			again, _ := controllerReadiness(cd, "FireworksApp", c.dep(), nil, nil)
			if !ready.Equal(again) {
				t.Errorf("not stable across two passes: %q vs %q", ready.Message, again.Message)
			}
		})
	}
}

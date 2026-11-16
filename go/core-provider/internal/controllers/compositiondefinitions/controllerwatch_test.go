package compositiondefinitions

import (
	"strings"
	"testing"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/cdreason"
	rtv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrlevent "sigs.k8s.io/controller-runtime/pkg/event"
)

// openstack's catch (2026-11-16): Update writes status.apiVersion = X and the runtime does not
// re-observe, so a Ready kept from the old version would sit beside a status naming X. The switch says
// Creating; the same version, or a first status, leaves Ready alone.
func TestReadyOnVersionSwitch(t *testing.T) {
	for _, c := range []struct {
		name, before, after string
		wantReason          rtv1.ConditionReason
	}{
		{"switch to a new version: Creating", "composition.krateo.io/v1-2-3", "composition.krateo.io/v1-2-4", rtv1.ReasonCreating},
		{"same version: Ready kept", "composition.krateo.io/v1-2-4", "composition.krateo.io/v1-2-4", rtv1.ReasonAvailable},
		{"no earlier status: Ready kept", "", "composition.krateo.io/v1-2-4", rtv1.ReasonAvailable},
	} {
		t.Run(c.name, func(t *testing.T) {
			cr := &compositiondefinitionsv1alpha1.CompositionDefinition{}
			cr.SetConditions(cdreason.Available.Condition())
			cr.Status.ApiVersion = c.after
			readyOnVersionSwitch(cr, c.before)
			got := cr.GetCondition(rtv1.TypeReady)
			if got.Reason != c.wantReason {
				t.Fatalf("Ready = %s/%s (%q), want %s", got.Status, got.Reason, got.Message, c.wantReason)
			}
			if c.wantReason == rtv1.ReasonCreating && !strings.Contains(got.Message, "now serving "+c.after) {
				t.Errorf("Ready message %q does not name the version now served", got.Message)
			}
		})
	}
}

// A follower is Available only when the shared controller is, and otherwise carries its Creating or
// Unavailable with the role appended: isFollowerCondition must still recognise every form (#387).
func TestFollowerReadyFollowsTheSharedController(t *testing.T) {
	owner := &compositiondefinitionsv1alpha1.CompositionDefinition{}
	owner.Namespace, owner.Name = "krateo-system", "owner"
	gvr := schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1-0-0", Resource: "fireworksapps"}
	for _, c := range []struct {
		name       string
		controller rtv1.Condition
		want       cdreason.Reason
	}{
		{"controller rolled out: Available", cdreason.Available.Condition(), cdreason.Available},
		{"controller rolling out: Creating", cdreason.Creating.WithMessage("the controller Deployment ns/x is rolling out"), cdreason.Creating},
		{"controller given up: Unavailable", cdreason.Unavailable.WithMessage("the controller Deployment ns/x is not available: P: m"), cdreason.Unavailable},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := followerReady(c.controller, owner, gvr)
			if !c.want.Is(got) {
				t.Fatalf("Ready = %s/%s, want %s/%s", got.Status, got.Reason, c.want.Status(), c.want)
			}
			if !strings.Contains(got.Message, "krateo-system/owner") || !isFollowerCondition(got) {
				t.Errorf("Ready %q does not carry the follower role (#387 would not recognise it)", got.Message)
			}
			if c.controller.Message != "" && !strings.Contains(got.Message, c.controller.Message) {
				t.Errorf("Ready %q dropped the controller's own message %q", got.Message, c.controller.Message)
			}
		})
	}
	if isFollowerCondition(cdreason.Creating.WithMessage("the controller Deployment ns/x is rolling out")) {
		t.Error("control: an owner's Creating is read as a follower's")
	}
}

func controllerDeployment(ns, resource, version string) *appsv1.Deployment {
	d := rolledOut(1)
	d.Namespace, d.Name = ns, resource+"-"+version+"-controller"
	d.Labels = map[string]string{labelComponent: "controller", labelManagedBy: "krateo", labelInstance: resource, labelVersion: version}
	return d
}

func definitionServing(ns, name, resource, version string) compositiondefinitionsv1alpha1.CompositionDefinition {
	cd := compositiondefinitionsv1alpha1.CompositionDefinition{}
	cd.Namespace, cd.Name = ns, name
	cd.Status.ApiVersion, cd.Status.Kind, cd.Status.Resource = "composition.krateo.io/"+version, "FireworksApp", resource
	return cd
}

// The watch maps a controller Deployment to exactly the definitions it runs: same resource, same
// version, controller in its namespace. Each excluded definition differs in one field, so a mapper that
// ignored that field would enqueue it.
func TestDefinitionsOfController(t *testing.T) {
	ns := controllerNamespace(&compositiondefinitionsv1alpha1.CompositionDefinition{})
	dep := controllerDeployment(ns, "fireworksapps", "v1-2-4")
	cds := []compositiondefinitionsv1alpha1.CompositionDefinition{
		definitionServing("team-a", "owner", "fireworksapps", "v1-2-4"),
		definitionServing("team-b", "follower", "fireworksapps", "v1-2-4"),
		definitionServing("team-a", "old-version", "fireworksapps", "v1-2-3"),
		definitionServing("team-a", "other-resource", "rockets", "v1-2-4"),
		{}, // never reconciled: no status
	}
	got := definitionsOfController(dep, cds)
	var names []string
	for _, r := range got {
		names = append(names, r.Namespace+"/"+r.Name)
	}
	if strings.Join(names, ",") != "team-a/owner,team-b/follower" {
		t.Fatalf("enqueued %v, want team-a/owner and team-b/follower only", names)
	}
	other := controllerDeployment("elsewhere", "fireworksapps", "v1-2-4")
	if got := definitionsOfController(other, cds); len(got) != 0 {
		t.Errorf("a Deployment outside the controller namespace enqueued %v", got)
	}
	unlabelled := controllerDeployment(ns, "fireworksapps", "v1-2-4")
	delete(unlabelled.Labels, labelManagedBy)
	if got := definitionsOfController(unlabelled, cds); len(got) != 0 {
		t.Errorf("a Deployment that is not a composition controller enqueued %v", got)
	}
}

// The predicate wakes a definition on a change that can move controllerReadiness's verdict, and not
// on one that cannot.
func TestControllerDeploymentPredicate(t *testing.T) {
	p := controllerDeploymentPredicate()
	base := controllerDeployment("krateo-system", "fireworksapps", "v1-2-4")
	update := func(mut func(*appsv1.Deployment)) bool {
		n := base.DeepCopy()
		mut(n)
		return p.Update(ctrlevent.UpdateEvent{ObjectOld: base, ObjectNew: n})
	}
	for _, c := range []struct {
		name string
		mut  func(*appsv1.Deployment)
		want bool
	}{
		{"annotation only: no", func(d *appsv1.Deployment) { d.Annotations = map[string]string{"a": "b"} }, false},
		{"availability lost: yes", func(d *appsv1.Deployment) {
			d.Status.AvailableReplicas = 0
			setCond(d, appsv1.DeploymentAvailable, corev1.ConditionFalse, "MinimumReplicasUnavailable", "")
		}, true},
		{"progress deadline exceeded: yes", func(d *appsv1.Deployment) {
			setCond(d, appsv1.DeploymentProgressing, corev1.ConditionFalse, "ProgressDeadlineExceeded", "")
		}, true},
		{"new generation: yes", func(d *appsv1.Deployment) { d.Generation++ }, true},
		{"updated replicas: yes", func(d *appsv1.Deployment) { d.Status.UpdatedReplicas = 0 }, true},
		{"condition message only: no", func(d *appsv1.Deployment) {
			setCond(d, appsv1.DeploymentAvailable, corev1.ConditionTrue, "MinimumReplicasAvailable", "new text")
		}, false},
	} {
		if got := update(c.mut); got != c.want {
			t.Errorf("%s: predicate = %v, want %v", c.name, got, c.want)
		}
	}
	if !p.Create(ctrlevent.CreateEvent{Object: base}) || !p.Delete(ctrlevent.DeleteEvent{Object: base}) {
		t.Error("a controller Deployment's create or delete did not pass")
	}
	plain := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "nginx", Namespace: "krateo-system"}}
	if p.Create(ctrlevent.CreateEvent{Object: plain}) {
		t.Error("control: an unrelated Deployment's create passed")
	}
}

// An unreadable controller Deployment cannot vouch for Available: the error is returned (so Observe
// returns it and the runtime writes NotObserved), only ControllerActive is written, and the Ready the
// definition had is not replaced. Control: a readable one writes Ready.
func TestApplyControllerConditions(t *testing.T) {
	id := func(c rtv1.Condition) rtv1.Condition { return c }
	start := cdreason.Available.WithMessage("from an earlier pass")
	active := rtv1.Condition{Type: "ControllerActive", Status: metav1.ConditionUnknown, Reason: rtv1.ReasonNotObserved}

	cr := &compositiondefinitionsv1alpha1.CompositionDefinition{}
	cr.SetConditions(start)
	readErr := errorString("reading the controller Deployment: boom")
	err := applyControllerConditions(cr, cdreason.Creating.WithMessage("unused"), active, true, readErr, id, cdreason.Available.Condition())
	if err == nil || err.Error() != readErr.Error() {
		t.Fatalf("err = %v, want the read error returned", err)
	}
	if got := cr.GetCondition(rtv1.TypeReady); got.Message != "from an earlier pass" {
		t.Errorf("Ready rewritten to %s/%s (%q) on an unreadable Deployment", got.Status, got.Reason, got.Message)
	}
	if got := cr.GetCondition("ControllerActive"); got.Reason != rtv1.ReasonNotObserved {
		t.Errorf("ControllerActive = %s, want NotObserved", got.Reason)
	}

	ok := &compositiondefinitionsv1alpha1.CompositionDefinition{}
	if err := applyControllerConditions(ok, cdreason.Creating.WithMessage("rolling out"), active, true, nil, id, cdreason.Available.Condition()); err != nil {
		t.Fatal(err)
	}
	if got := ok.GetCondition(rtv1.TypeReady); !cdreason.Creating.Is(got) {
		t.Errorf("control: a readable Deployment's Ready not written (%s/%s)", got.Status, got.Reason)
	}
	remote := &compositiondefinitionsv1alpha1.CompositionDefinition{}
	if err := applyControllerConditions(remote, rtv1.Condition{}, rtv1.Condition{}, false, nil, id, cdreason.Available.Condition()); err != nil {
		t.Fatal(err)
	}
	if got := remote.GetCondition(rtv1.TypeReady); !cdreason.Available.Is(got) {
		t.Errorf("a remote definition's fallback not written (%s/%s)", got.Status, got.Reason)
	}
}

type errorString string

func (e errorString) Error() string { return string(e) }

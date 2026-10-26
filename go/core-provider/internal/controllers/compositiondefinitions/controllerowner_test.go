package compositiondefinitions

import (
	"context"
	"testing"
	"time"

	"github.com/krateo-platformops/core-provider/apis"
	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlevent "sigs.k8s.io/controller-runtime/pkg/event"
)

var ownerEpoch = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

func ownerCD(namespace, name string, ageRank int) compositiondefinitionsv1alpha1.CompositionDefinition {
	cd := compositiondefinitionsv1alpha1.CompositionDefinition{}
	cd.SetNamespace(namespace)
	cd.SetName(name)
	cd.SetCreationTimestamp(metav1.NewTime(ownerEpoch.Add(time.Duration(ageRank) * time.Hour)))
	return cd
}

func deleting(cd compositiondefinitionsv1alpha1.CompositionDefinition) compositiondefinitionsv1alpha1.CompositionDefinition {
	now := metav1.NewTime(ownerEpoch.Add(100 * time.Hour))
	cd.SetDeletionTimestamp(&now)
	return cd
}

func remoteCD(cd compositiondefinitionsv1alpha1.CompositionDefinition) compositiondefinitionsv1alpha1.CompositionDefinition {
	cd.Spec.Deploy = &compositiondefinitionsv1alpha1.DeploymentTarget{
		TargetRef: &compositiondefinitionsv1alpha1.TargetReference{Name: "spoke"},
	}
	return cd
}

func ownerName(cd *compositiondefinitionsv1alpha1.CompositionDefinition) string {
	if cd == nil {
		return "<self>"
	}
	return cd.Namespace + "/" + cd.Name
}

// The re-home scenario: a live definition in krateo-system, a new one for the same Kind+version
// created in its own namespace. The old one keeps the controller until its deletion starts; from
// that moment the new one owns it.
func TestElectControllerOwner_RehomeHandsOverOnDeletion(t *testing.T) {
	old := ownerCD("krateo-system", "selfservice-krateo", 0)
	moved := ownerCD("krateo-bp-selfservice-krateo", "selfservice-krateo", 1)

	if got := electControllerOwner(&moved, []compositiondefinitionsv1alpha1.CompositionDefinition{old, moved}); ownerName(got) != "krateo-system/selfservice-krateo" {
		t.Fatalf("new definition while the old is live: owner = %s, want krateo-system/selfservice-krateo", ownerName(got))
	}
	if got := electControllerOwner(&old, []compositiondefinitionsv1alpha1.CompositionDefinition{old, moved}); got != nil {
		t.Fatalf("old definition while live must own the controller, got follower of %s", ownerName(got))
	}

	oldDeleting := deleting(old)
	if got := electControllerOwner(&moved, []compositiondefinitionsv1alpha1.CompositionDefinition{oldDeleting, moved}); got != nil {
		t.Fatalf("once the old definition is deleting, the new one must own the controller; got follower of %s", ownerName(got))
	}
	if got := oldestLiveLocalSibling(&oldDeleting, []compositiondefinitionsv1alpha1.CompositionDefinition{oldDeleting, moved}); ownerName(got) != "krateo-bp-selfservice-krateo/selfservice-krateo" {
		t.Fatalf("successor of the deleting definition = %s, want the moved one", ownerName(got))
	}
}

// Every definition must elect the same owner, including when creation timestamps tie (they have
// one-second resolution, so definitions applied together routinely do).
func TestElectControllerOwner_TieBreakIsTotal(t *testing.T) {
	a := ownerCD("alpha", "x", 0)
	b := ownerCD("beta", "x", 0)
	c := ownerCD("alpha", "y", 0)
	all := []compositiondefinitionsv1alpha1.CompositionDefinition{b, c, a}

	for _, self := range []compositiondefinitionsv1alpha1.CompositionDefinition{a, b, c} {
		self := self
		got := electControllerOwner(&self, all)
		if self.Namespace == "alpha" && self.Name == "x" {
			if got != nil {
				t.Errorf("alpha/x must own (namespace then name order), got follower of %s", ownerName(got))
			}
			continue
		}
		if ownerName(got) != "alpha/x" {
			t.Errorf("%s/%s elected %s, want alpha/x", self.Namespace, self.Name, ownerName(got))
		}
	}
}

// Remote definitions get a controller dedicated to them on their spoke: they neither own a local
// Kind+version's controller nor follow one.
func TestElectControllerOwner_RemoteIsOutOfTheElection(t *testing.T) {
	remoteOld := remoteCD(ownerCD("team-a", "app", 0))
	local := ownerCD("team-b", "app", 1)

	if got := electControllerOwner(&local, []compositiondefinitionsv1alpha1.CompositionDefinition{remoteOld, local}); got != nil {
		t.Fatalf("an older REMOTE definition must not make a local one a follower, got %s", ownerName(got))
	}
	if got := electControllerOwner(&remoteOld, []compositiondefinitionsv1alpha1.CompositionDefinition{remoteOld, local}); got != nil {
		t.Fatalf("a remote definition never follows, got %s", ownerName(got))
	}
}

// A lone definition owns its controller, and has no successor.
func TestElectControllerOwner_Alone(t *testing.T) {
	only := ownerCD("krateo-system", "portal", 0)
	if got := electControllerOwner(&only, []compositiondefinitionsv1alpha1.CompositionDefinition{only}); got != nil {
		t.Fatalf("lone definition must own, got %s", ownerName(got))
	}
	if got := oldestLiveLocalSibling(&only, []compositiondefinitionsv1alpha1.CompositionDefinition{only}); got != nil {
		t.Fatalf("lone definition has no successor, got %s", ownerName(got))
	}
}

// Sibling wake-ups fire on deletion and on a version move only: firing on every status write would
// let two definitions on one Kind re-enqueue each other forever.
func TestSiblingHandoverPredicate(t *testing.T) {
	p := siblingHandoverPredicate()
	base := ownerCD("ns", "cd", 0)
	base.Status.ApiVersion = "composition.krateo.io/v1-0-0"

	if p.Create(ctrlevent.CreateEvent{Object: &base}) {
		t.Error("create must not wake siblings")
	}
	if !p.Delete(ctrlevent.DeleteEvent{Object: &base}) {
		t.Error("delete must wake siblings")
	}

	statusOnly := base.DeepCopy()
	statusOnly.Status.Digest = "changed"
	if p.Update(ctrlevent.UpdateEvent{ObjectOld: &base, ObjectNew: statusOnly}) {
		t.Error("a status write that keeps the version must not wake siblings")
	}

	startsDeleting := deleting(base)
	if !p.Update(ctrlevent.UpdateEvent{ObjectOld: &base, ObjectNew: &startsDeleting}) {
		t.Error("deletion starting must wake siblings")
	}

	bumped := base.DeepCopy()
	bumped.Status.ApiVersion = "composition.krateo.io/v1-1-0"
	if !p.Update(ctrlevent.UpdateEvent{ObjectOld: &base, ObjectNew: bumped}) {
		t.Error("a version move must wake siblings")
	}
}

// The election is per Kind+VERSION. Two definitions on the same Kind at different versions are not
// siblings: each owns its own version's controller and applies its own CRD version, so neither
// version can be lost to the other's CRD apply (#192's invariant).
func TestControllerOwner_IsPerVersion(t *testing.T) {
	sc := runtime.NewScheme()
	if err := apis.AddToScheme(sc); err != nil {
		t.Fatal(err)
	}
	onVersion := func(cd compositiondefinitionsv1alpha1.CompositionDefinition, version string) *compositiondefinitionsv1alpha1.CompositionDefinition {
		cd.Status.ApiVersion = "composition.krateo.io/" + version
		cd.Status.Kind = "FireworksApp"
		return &cd
	}
	older := onVersion(ownerCD("team-a", "fw", 0), "v1-0-0")
	newerOtherVersion := onVersion(ownerCD("team-b", "fw", 1), "v2-0-0")
	newerSameVersion := onVersion(ownerCD("team-c", "fw", 2), "v1-0-0")

	e := &external{mgmt: clientfake.NewClientBuilder().WithScheme(sc).
		WithObjects(older, newerOtherVersion, newerSameVersion).WithStatusSubresource(older, newerOtherVersion, newerSameVersion).Build()}

	gvk := func(version string) schema.GroupVersionKind {
		return schema.GroupVersionKind{Group: "composition.krateo.io", Version: version, Kind: "FireworksApp"}
	}
	if got, err := e.controllerOwner(context.Background(), newerOtherVersion, gvk("v2-0-0")); err != nil || got != nil {
		t.Fatalf("the only definition on v2-0-0 must own it, got follower of %s (err %v)", ownerName(got), err)
	}
	if got, err := e.controllerOwner(context.Background(), older, gvk("v1-0-0")); err != nil || got != nil {
		t.Fatalf("the oldest definition on v1-0-0 must own it, got follower of %s (err %v)", ownerName(got), err)
	}
	got, err := e.controllerOwner(context.Background(), newerSameVersion, gvk("v1-0-0"))
	if err != nil || ownerName(got) != "team-a/fw" {
		t.Fatalf("a newer definition on v1-0-0 must follow team-a/fw, got %s (err %v)", ownerName(got), err)
	}
}

// A deleting definition is finished once a live successor holds its Kind+version and no
// controller of its own is left; until then (or with no successor at all) the delete is not done.
// Reporting "gone" is what lets the reconciler remove the finalizer, since the CRD survives a
// hand-over by design.
func TestHandedOver(t *testing.T) {
	sc := runtime.NewScheme()
	if err := apis.AddToScheme(sc); err != nil {
		t.Fatal(err)
	}
	if err := appsv1.AddToScheme(sc); err != nil {
		t.Fatal(err)
	}
	v1 := func(cd compositiondefinitionsv1alpha1.CompositionDefinition) *compositiondefinitionsv1alpha1.CompositionDefinition {
		cd.Status.ApiVersion = "composition.krateo.io/v1-0-0"
		cd.Status.Kind = "FireworksApp"
		return &cd
	}
	gvk := schema.GroupVersionKind{Group: "composition.krateo.io", Version: "v1-0-0", Kind: "FireworksApp"}
	gvr := schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1-0-0", Resource: "fireworksapps"}
	leaving := v1(ownerCD("krateo-system", "fw", 0))
	successor := v1(ownerCD("krateo-bp-fw", "fw", 1))
	ownController := &appsv1.Deployment{}
	ownController.SetNamespace("krateo-system")
	ownController.SetName("fireworksapps-v1-0-0-controller")

	check := func(objs ...client.Object) bool {
		t.Helper()
		cli := clientfake.NewClientBuilder().WithScheme(sc).WithObjects(objs...).WithStatusSubresource(leaving, successor).Build()
		got, err := (&external{mgmt: cli, kube: cli}).handedOver(context.Background(), leaving, gvk, gvr)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	if check(leaving) {
		t.Error("no successor: the last definition's delete is not a hand-over")
	}
	if check(leaving, successor, ownController) {
		t.Error("own controller still running: hand-over not finished")
	}
	if !check(leaving, successor) {
		t.Error("successor present and own controller gone: the definition must be reported gone")
	}

	// Same namespace: the controller in it IS the successor's (shared {resource}-{apiVersion}
	// objects), so the leaving definition is done at once, and must not wait for, or remove, it.
	sameNS := v1(ownerCD("krateo-system", "fw-moved", 1))
	if !check(leaving, sameNS, ownController) {
		t.Error("same-namespace successor: the shared controller is the successor's; the leaving definition is gone")
	}
}

package compositiondefinitions

import (
	"testing"
	"time"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

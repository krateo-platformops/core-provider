package compositiondefinitions

import (
	"strings"
	"testing"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	rtv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// #387. A definition that took a controller over must stop saying another definition runs it, at
// once, rather than after the poll interval. And nothing but that one stale message is replaced.
func TestReadyOnBecomingOwner(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v2-0-4", Resource: "fireworksapps"}
	owner := &compositiondefinitionsv1alpha1.CompositionDefinition{ObjectMeta: metav1.ObjectMeta{Name: "fireworks-app-2", Namespace: "test-system"}}

	t.Run("the follower message is replaced by Creating", func(t *testing.T) {
		got, ok := readyOnBecomingOwner(followerCondition(owner, gvr), gvr)
		if !ok {
			t.Fatal("the follower condition was left in place")
		}
		if got.Reason != rtv1.ReasonCreating || got.Status != "False" || strings.Contains(got.Message, followerMarker) {
			t.Errorf("got %s/%s %q, want False/Creating without the follower message", got.Status, got.Reason, got.Message)
		}
		if !strings.Contains(got.Message, gvr.String()) {
			t.Errorf("message %q does not name the Kind+version", got.Message)
		}
	})

	// The control arms: an owner's ordinary conditions are not this function's business. Replacing
	// Available would flip Ready to False on every drift-correcting Update.
	for _, c := range []rtv1.Condition{
		rtv1.Available(),
		rtv1.Creating(),
		rtv1.Unavailable().WithMessage("chart could not be read"),
		rtv1.Available().WithMessage("composition controller for x is deployed"),
	} {
		if _, ok := readyOnBecomingOwner(c, gvr); ok {
			t.Errorf("replaced %s/%s %q, which is not the follower condition", c.Status, c.Reason, c.Message)
		}
	}

	// The writer and the reader share followerMarker; a reconstruction of the text would pass this too,
	// so the round trip is what is asserted.
	if !isFollowerCondition(followerCondition(owner, gvr)) {
		t.Error("isFollowerCondition does not recognise followerCondition's own output")
	}
}

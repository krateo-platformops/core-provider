package composition

import (
	"errors"
	"testing"

	"github.com/krateo-platformops/unstructured-runtime/pkg/controller"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// #381. A graceful pause that is requested and not yet in effect must say so, on every observation
// that does not already speak for the pause, and nowhere else.
func TestWithGracefulPausePending(t *testing.T) {
	obj := func(annotations map[string]string, deleting bool) *unstructured.Unstructured {
		u := &unstructured.Unstructured{}
		u.SetAPIVersion("composition.krateo.io/v1-0-0")
		u.SetKind("Fireworksapp")
		u.SetName("c")
		u.SetAnnotations(annotations)
		if deleting {
			now := metav1.Now()
			u.SetDeletionTimestamp(&now)
		}
		return u
	}
	pending := func(obs controller.ExternalObservation) bool {
		for _, c := range obs.Conditions {
			if c.Type == "Synced" && c.Reason == "GracefulPausePending" && c.Status == metav1.ConditionFalse {
				return true
			}
		}
		return false
	}
	requested := map[string]string{"krateo.io/gracefully-paused": "true"}
	engaged := map[string]string{"krateo.io/gracefully-paused": "true", "krateo.io/gracefully-paused-time": "2026-11-07T10:00:00Z"}

	// The subject: requested, not engaged, on both the converged and the change-pending observation.
	for _, upToDate := range []bool{true, false} {
		got, err := withGracefulPausePending(obj(requested, false), controller.ExternalObservation{ResourceExists: true, ResourceUpToDate: upToDate}, nil)
		if err != nil || !pending(got) {
			t.Errorf("upToDate=%v: pending condition missing (err %v, conditions %+v)", upToDate, err, got.Conditions)
		}
		if got.ResourceUpToDate != upToDate || !got.ResourceExists {
			t.Errorf("upToDate=%v: the observation itself changed: %+v", upToDate, got)
		}
	}

	// It adds; it does not replace what observe returned.
	prior := metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Available"}
	got, _ := withGracefulPausePending(obj(requested, false), controller.ExternalObservation{ResourceExists: true, ResourceUpToDate: true, Conditions: []metav1.Condition{prior}}, nil)
	if len(got.Conditions) != 2 || got.Conditions[0] != prior {
		t.Errorf("conditions = %+v, want observe's Ready kept and the pending Synced added", got.Conditions)
	}

	// The controls: each is a state where the pending condition would be false or not ours to say.
	controls := []struct {
		name string
		mg   *unstructured.Unstructured
		err  error
	}{
		{"not requested", obj(nil, false), nil},
		{"requested with another value", obj(map[string]string{"krateo.io/gracefully-paused": "false"}, false), nil},
		{"engaged: observe reports the pause itself", obj(engaged, false), nil},
		{"deleting: Delete owns it", obj(requested, true), nil},
		{"observe failed: the runtime reports the error", obj(requested, false), errors.New("boom")},
	}
	for _, c := range controls {
		got, err := withGracefulPausePending(c.mg, controller.ExternalObservation{ResourceExists: true, ResourceUpToDate: true}, c.err)
		if pending(got) {
			t.Errorf("%s: pending condition added, want none", c.name)
		}
		if !errors.Is(err, c.err) {
			t.Errorf("%s: error = %v, want %v passed through", c.name, err, c.err)
		}
	}

	// A malformed time is not "engaged" (GetGracefullyPausedTime refuses it), so the pause is still
	// pending, and saying so is correct.
	bad := map[string]string{"krateo.io/gracefully-paused": "true", "krateo.io/gracefully-paused-time": "yesterday"}
	if got, _ := withGracefulPausePending(obj(bad, false), controller.ExternalObservation{ResourceExists: true, ResourceUpToDate: true}, nil); !pending(got) {
		t.Error("a malformed paused-time is not an engaged pause, so it must still read as pending")
	}
}

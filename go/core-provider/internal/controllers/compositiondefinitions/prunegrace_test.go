package compositiondefinitions

import (
	"reflect"
	"testing"
	"time"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// #454. A served version is pruned only after it has stayed prune-eligible for the grace window,
// each version on its own clock, so a render that listed the CRDs before the prune never looks the
// version up after it.

func pp(v string, since time.Time) compositiondefinitionsv1alpha1.PendingPrune {
	return compositiondefinitionsv1alpha1.PendingPrune{Version: v, EligibleSince: metav1.NewTime(since)}
}

func TestPrunesDue(t *testing.T) {
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	w := servedVersionPruneGrace
	for _, tc := range []struct {
		name        string
		prunable    []string
		pending     []compositiondefinitionsv1alpha1.PendingPrune
		wantDue     []string
		wantPending []compositiondefinitionsv1alpha1.PendingPrune
	}{
		{
			name:        "first sight starts the clock and prunes nothing (the race #454 is)",
			prunable:    []string{"v0-1-42"},
			wantPending: []compositiondefinitionsv1alpha1.PendingPrune{pp("v0-1-42", now)},
		},
		{
			name:        "inside the window it waits",
			prunable:    []string{"v0-1-42"},
			pending:     []compositiondefinitionsv1alpha1.PendingPrune{pp("v0-1-42", now.Add(-w+time.Second))},
			wantPending: []compositiondefinitionsv1alpha1.PendingPrune{pp("v0-1-42", now.Add(-w+time.Second))},
		},
		{
			name:        "at the window it is due, and keeps its original clock until pruned",
			prunable:    []string{"v0-1-42"},
			pending:     []compositiondefinitionsv1alpha1.PendingPrune{pp("v0-1-42", now.Add(-w))},
			wantDue:     []string{"v0-1-42"},
			wantPending: []compositiondefinitionsv1alpha1.PendingPrune{pp("v0-1-42", now.Add(-w))},
		},
		{
			// demo, 2026-11-11: a history rotation released several versions at once. Each keeps its
			// own clock; a newly eligible one never resets an older one's.
			name:     "a batch is independent per-version clocks",
			prunable: []string{"v0-2-3", "v1-8-107"},
			pending:  []compositiondefinitionsv1alpha1.PendingPrune{pp("v0-2-3", now.Add(-6*time.Minute))},
			wantDue:  []string{"v0-2-3"},
			wantPending: []compositiondefinitionsv1alpha1.PendingPrune{
				pp("v0-2-3", now.Add(-6*time.Minute)), pp("v1-8-107", now),
			},
		},
		{
			name:    "a version that is no longer eligible is dropped",
			pending: []compositiondefinitionsv1alpha1.PendingPrune{pp("v0-1-42", now.Add(-10*time.Minute))},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			due, pending := prunesDue(tc.prunable, tc.pending, now, w)
			if !reflect.DeepEqual(due, tc.wantDue) {
				t.Errorf("due = %v, want %v", due, tc.wantDue)
			}
			if !reflect.DeepEqual(pending, tc.wantPending) {
				t.Errorf("pending = %v, want %v", pending, tc.wantPending)
			}
		})
	}
}

// A version that stops being eligible and becomes eligible again starts a FRESH clock: an old
// eligibility must not let a version that was just referenced again be pruned at once.
func TestPrunesDueResetsWhenEligibilityLapses(t *testing.T) {
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	_, pending := prunesDue(nil, []compositiondefinitionsv1alpha1.PendingPrune{pp("v1", now.Add(-time.Hour))}, now, servedVersionPruneGrace)
	due, pending := prunesDue([]string{"v1"}, pending, now.Add(time.Second), servedVersionPruneGrace)
	if len(due) != 0 {
		t.Fatalf("a version eligible again must wait the full window, got due=%v", due)
	}
	if len(pending) != 1 || !pending[0].EligibleSince.Time.Equal(now.Add(time.Second)) {
		t.Errorf("the clock must restart at the new sighting, got %v", pending)
	}
}

// Diego, 2026-11-11: 5 minutes, sized as 4x the worst Installer reconcile pass measured on
// krateo-057 (74.5s). A change to the window is a decision, so it must fail here first.
func TestServedVersionPruneGraceIsTheDecidedWindow(t *testing.T) {
	if servedVersionPruneGrace != 5*time.Minute {
		t.Errorf("servedVersionPruneGrace = %s, want the decided 5m", servedVersionPruneGrace)
	}
}

// applyPruneGrace is what both Observe and Update call: it must write the clock to the status the
// runtime persists, and return only what is due.
func TestApplyPruneGraceWritesTheRecordToStatus(t *testing.T) {
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	cr := &compositiondefinitionsv1alpha1.CompositionDefinition{}
	if due := applyPruneGrace(cr, []string{"v1"}, now); len(due) != 0 {
		t.Fatalf("first sight must not be due, got %v", due)
	}
	if got := cr.Status.ServedVersionsPendingPrune; len(got) != 1 || got[0].Version != "v1" || !got[0].EligibleSince.Time.Equal(now) {
		t.Fatalf("status record = %v, want [v1 since now]", got)
	}
	if due := applyPruneGrace(cr, []string{"v1"}, now.Add(servedVersionPruneGrace)); len(due) != 1 || due[0] != "v1" {
		t.Errorf("after the window v1 must be due, got %v", due)
	}
}

// Observe's decision: a version inside its window leaves the definition UP TO DATE (no Update
// re-run, no update-pending mark for a readiness gate to read); only a due version reports pending.
func TestObservePruneDecision(t *testing.T) {
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	cr := &compositiondefinitionsv1alpha1.CompositionDefinition{}
	if pending, _, _ := observePruneDecision(cr, []string{"v0-1-42"}, now); pending {
		t.Fatal("a version just seen eligible must leave the definition up to date")
	}
	if pending, _, _ := observePruneDecision(cr, []string{"v0-1-42"}, now.Add(servedVersionPruneGrace-time.Second)); pending {
		t.Fatal("inside the window the definition must stay up to date")
	}
	pending, due, msg := observePruneDecision(cr, []string{"v0-1-42"}, now.Add(servedVersionPruneGrace))
	if !pending || len(due) != 1 || due[0] != "v0-1-42" || msg != "stale served versions pending prune: [v0-1-42]" {
		t.Errorf("a due version must report pending with its message, got pending=%v due=%v msg=%q", pending, due, msg)
	}
	// Control: nothing eligible, nothing pending.
	if pending, _, _ := observePruneDecision(&compositiondefinitionsv1alpha1.CompositionDefinition{}, nil, now); pending {
		t.Error("with no eligible version Observe must report up to date")
	}
}

// #471: the keep reasons reach the status as {version, reason}, "ALL" as "*", sorted; nothing held
// is nil (no field written), so servedVersionsHeld is empty exactly when nothing is held.
func TestHeldVersions(t *testing.T) {
	got := heldVersions([]string{
		"v0-3-49:release-revisions=1:[openstack/umbrella(rev 4, secret openstack/sh.helm.release.v1.umbrella.v4, kinds Glance)]",
		"v0-3-47:instances=2",
		"v0-3-48:referenced",
	})
	want := []compositiondefinitionsv1alpha1.HeldVersion{
		{Version: "v0-3-47", Reason: "instances=2"},
		{Version: "v0-3-48", Reason: "referenced"},
		{Version: "v0-3-49", Reason: "release-revisions=1:[openstack/umbrella(rev 4, secret openstack/sh.helm.release.v1.umbrella.v4, kinds Glance)]"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("heldVersions = %+v\nwant %+v", got, want)
	}
	if all := heldVersions([]string{"ALL:unaccounted-instances=1:[ns/x]"}); len(all) != 1 || all[0].Version != "*" || all[0].Reason != "unaccounted-instances=1:[ns/x]" {
		t.Errorf("ALL = %+v, want version * with the reason", all)
	}
	if heldVersions(nil) != nil {
		t.Error("nothing held must be nil")
	}
}

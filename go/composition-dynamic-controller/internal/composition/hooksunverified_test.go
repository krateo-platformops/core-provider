package composition

import (
	"context"
	"strings"
	"testing"

	"github.com/krateo-platformops/composition-dynamic-controller/internal/conditionreason"
	compositionMeta "github.com/krateo-platformops/composition-dynamic-controller/pkg/meta"
	helmconfig "github.com/krateo-platformops/plumbing/helm"
	"github.com/krateo-platformops/plumbing/kubeutil/event"
	"github.com/krateo-platformops/unstructured-runtime/pkg/logging"
	unstructuredtools "github.com/krateo-platformops/unstructured-runtime/pkg/tools/unstructured"
	"github.com/krateo-platformops/unstructured-runtime/pkg/tools/unstructured/condition"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/events"
)

const hooksMark = "2026-11-17T15:00:00Z"

func markedPortal(t *testing.T, mark bool) *unstructured.Unstructured {
	t.Helper()
	u := srPortal("1", "x")
	ann := map[string]string{"keep.example/other": "kept"}
	if mark {
		ann[compositionMeta.AnnotationKeyHooksUnverified] = hooksMark
	}
	u.SetAnnotations(ann)
	return u
}

// #563: while krateo.io/hooks-unverified is set, Ready reads HooksMayNotHaveRun whatever the rollup
// says, a healthy rollup included, and keeps the rollup's reason and message. Control: without the
// mark the rollup's outcome passes through unchanged.
func TestWithHooksUnverified(t *testing.T) {
	healthy := readyOutcome{reason: "Available", message: "Composition is up-to-date"}
	failing := readyOutcome{reason: "Unavailable", message: "unhealthy: Deployment/db"}

	if got := withHooksUnverified(healthy, markedPortal(t, false)); got != healthy {
		t.Fatalf("control: no mark changed the outcome to %+v", got)
	}
	for _, in := range []readyOutcome{healthy, failing} {
		got := withHooksUnverified(in, markedPortal(t, true))
		if got.reason != conditionreason.ReasonHooksMayNotHaveRun {
			t.Errorf("%s with the mark: reason %q, want HooksMayNotHaveRun", in.reason, got.reason)
		}
		for _, want := range []string{compositionMeta.AnnotationKeyHooksUnverified + "=" + hooksMark, "Children: " + in.reason + ": " + in.message, "remove the annotation"} {
			if !strings.Contains(got.message, want) {
				t.Errorf("%s with the mark: message %q lacks %q", in.reason, got.message, want)
			}
		}
	}
}

// The outcome reaches the condition: stampReady has a case for it, so it neither errors with
// "unknown ready reason" nor stamps Available (the guard's whole point).
func TestStampReadyHooksMayNotHaveRun(t *testing.T) {
	mg := markedPortal(t, true)
	out := withHooksUnverified(readyOutcome{reason: "Available", message: "ok"}, mg)
	if err := stampReady(mg, out, true); err != nil {
		t.Fatalf("stampReady: %v", err)
	}
	var ready *metav1.Condition
	for _, c := range unstructuredtools.GetConditions(mg) {
		if c.Type == condition.TypeReady {
			c := c
			ready = &c
		}
	}
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != conditionreason.ReasonHooksMayNotHaveRun {
		t.Fatalf("Ready = %+v, want False/HooksMayNotHaveRun", ready)
	}
}

// upgradeRanHooks over every return plumbing's Reconcile makes (v1.93.0 helm/v3/reconcile.go): only
// a successful install or upgrade counts, whichever path ran it.
func TestUpgradeRanHooks(t *testing.T) {
	rel := &helmconfig.Release{Revision: 3, Status: helmconfig.StatusDeployed}
	for _, tc := range []struct {
		name string
		res  *helmconfig.ReconcileResult
		err  error
		want bool
	}{
		{"steady state: no revision, no hooks (:573)", &helmconfig.ReconcileResult{Release: rel}, nil, false},
		{"held desired-state change: nothing written (:343)", &helmconfig.ReconcileResult{DesiredStateChange: "chart version", DesiredStateHeld: true, Release: rel}, nil, false},
		{"upgrade to a changed desired state (:372)", &helmconfig.ReconcileResult{Changed: true, DesiredStateChange: "manifest", Release: rel}, nil, true},
		{"upgrade after a drift heal (:600)", &helmconfig.ReconcileResult{Changed: true, Release: rel}, nil, true},
		{"retried failed release, nothing else differed (:600)", &helmconfig.ReconcileResult{RetriedFailedRelease: true, Release: rel}, nil, true},
		{"an upgrade that failed", &helmconfig.ReconcileResult{Changed: true}, context.DeadlineExceeded, false},
		{"no result", nil, nil, false},
	} {
		if got := upgradeRanHooks(tc.res, tc.err); got != tc.want {
			t.Errorf("%s: upgradeRanHooks = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The mark is written and removed with a merge patch of that one key: another annotation survives,
// and the write carries no resourceVersion precondition (the object in the fake is at a newer
// resourceVersion than the copy patched from, which a tools.Update would 409 on).
func TestPatchHooksUnverifiedTouchesOnlyItsKey(t *testing.T) {
	ctx := context.Background()
	stored := markedPortal(t, false)
	stored.SetResourceVersion("7")
	dyn := srFakeDyn(stored)
	stale := markedPortal(t, false) // resourceVersion "1"

	v := hooksMark
	got, err := patchHooksUnverified(ctx, stale, &v, srOpts(dyn))
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if got.GetAnnotations()[compositionMeta.AnnotationKeyHooksUnverified] != hooksMark || got.GetAnnotations()["keep.example/other"] != "kept" {
		t.Fatalf("after set, annotations = %v", got.GetAnnotations())
	}
	got, err = patchHooksUnverified(ctx, got, nil, srOpts(dyn))
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, still := got.GetAnnotations()[compositionMeta.AnnotationKeyHooksUnverified]; still || got.GetAnnotations()["keep.example/other"] != "kept" {
		t.Fatalf("after remove, annotations = %v", got.GetAnnotations())
	}
}

// The clear: removed after a hooks-running upgrade with a Normal Event; kept after a pass that ran
// no hook; kept, with a Warning Event, when the patch fails (the object is gone from the fake), and
// the pass carries on with the object it had.
func TestClearHooksUnverifiedAfter(t *testing.T) {
	ctx := context.Background()
	ran := &helmconfig.ReconcileResult{Changed: true, Release: &helmconfig.Release{Revision: 3, Status: helmconfig.StatusDeployed}}
	steady := &helmconfig.ReconcileResult{Release: &helmconfig.Release{Revision: 2, Status: helmconfig.StatusDeployed}}
	for _, tc := range []struct {
		name       string
		inFake     bool
		res        *helmconfig.ReconcileResult
		wantMarked bool
		wantEvent  string
	}{
		{"a hooks-running upgrade clears it", true, ran, false, string(reasonHooksUnverifiedCleared)},
		{"control: a pass that ran no hook keeps it", true, steady, true, ""},
		{"a failed patch keeps it and says so", false, ran, true, string(reasonHooksUnverifiedNotClear)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mg := markedPortal(t, true)
			dyn := srFakeDyn()
			if tc.inFake {
				dyn = srFakeDyn(markedPortal(t, true))
			}
			rec := events.NewFakeRecorder(10)
			h := &handler{eventRecorder: *event.NewAPIRecorder(rec)}
			got := h.clearHooksUnverifiedAfter(ctx, mg, tc.res, nil, srOpts(dyn), logging.NewNopLogger(), "Observe")
			if _, marked := hooksUnverifiedSince(got); marked != tc.wantMarked {
				t.Errorf("marked after = %v, want %v", marked, tc.wantMarked)
			}
			var evs []string
			for len(rec.Events) > 0 {
				evs = append(evs, <-rec.Events)
			}
			if tc.wantEvent == "" && len(evs) > 0 {
				t.Errorf("events %v, want none", evs)
			}
			if tc.wantEvent != "" && (len(evs) != 1 || !strings.Contains(evs[0], tc.wantEvent)) {
				t.Errorf("events %v, want one %s", evs, tc.wantEvent)
			}
		})
	}
}

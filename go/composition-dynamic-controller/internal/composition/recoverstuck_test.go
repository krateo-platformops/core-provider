package composition

import (
	"context"
	"errors"
	"strings"
	"testing"

	compositionMeta "github.com/krateo-platformops/composition-dynamic-controller/pkg/meta"
	helmconfig "github.com/krateo-platformops/plumbing/helm"
	"github.com/krateo-platformops/plumbing/kubeutil/event"
	"github.com/krateo-platformops/unstructured-runtime/pkg/logging"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/tools/events"
)

// recoveryClient is a helm client that serves a fixed history and records every rollback: the
// revision it was asked for, and whether krateo.io/hooks-unverified was ALREADY on the stored object
// at that moment (read from the API, not from the caller's copy), which is the ordering #563 needs.
type recoveryClient struct {
	helmconfig.Client // any other method panics: the recovery must call only these two
	history           []*helmconfig.Release
	historyErr        error
	rollbackErr       error
	dyn               *dynamicfake.FakeDynamicClient

	rollbacks        []int
	markedAtRollback []bool
}

func (c *recoveryClient) History(context.Context, string, *helmconfig.HistoryConfig) ([]*helmconfig.Release, error) {
	return c.history, c.historyErr
}

func (c *recoveryClient) Rollback(ctx context.Context, _ string, cfg *helmconfig.RollbackConfig) (*helmconfig.OperationResult, error) {
	c.rollbacks = append(c.rollbacks, cfg.ReleaseVersion)
	marked := false
	if u, err := c.dyn.Resource(srGVR).Namespace("krateo-system").Get(ctx, "portal", metav1.GetOptions{}); err == nil {
		_, marked = u.GetAnnotations()[compositionMeta.AnnotationKeyHooksUnverified]
	}
	c.markedAtRollback = append(c.markedAtRollback, marked)
	if c.rollbackErr != nil {
		return nil, c.rollbackErr
	}
	last := 0
	for _, r := range c.history {
		if r.Revision > last {
			last = r.Revision
		}
	}
	return &helmconfig.OperationResult{Release: &helmconfig.Release{Name: "r", Revision: last + 1, Status: helmconfig.StatusDeployed}}, nil
}

// #563, through the recovery itself rather than the target function alone: which revision Rollback
// is asked for, that the mark lands on the stored object BEFORE a rollback to itself, and that every
// read or write it cannot complete stops it before any rollback.
func TestRecoverStuckOperation(t *testing.T) {
	for _, tc := range []struct {
		name         string
		pending      *helmconfig.Release
		history      []*helmconfig.Release
		historyErr   error
		rollbackErr  error
		notInAPI     bool // the composition is absent from the API, so the mark's patch fails
		wantRollback []int
		wantMarked   bool // the mark was on the stored object when Rollback was called
		wantErr      string
		wantEvent    string
	}{
		{name: "interrupted upgrade: rolled back to the deployed predecessor, no mark",
			pending:      rev(3, helmconfig.StatusPendingUpgrade),
			history:      []*helmconfig.Release{rev(1, helmconfig.StatusSuperseded), rev(2, helmconfig.StatusDeployed), rev(3, helmconfig.StatusPendingUpgrade)},
			wantRollback: []int{2}},
		{name: "interrupted first install: marked FIRST, then rolled back to itself",
			pending: rev(1, helmconfig.StatusPendingInstall), history: []*helmconfig.Release{rev(1, helmconfig.StatusPendingInstall)},
			wantRollback: []int{1}, wantMarked: true, wantEvent: string(reasonHooksUnverified)},
		{name: "a rollback that fails after the mark keeps it (still true)",
			pending: rev(1, helmconfig.StatusPendingInstall), history: []*helmconfig.Release{rev(1, helmconfig.StatusPendingInstall)},
			rollbackErr: errors.New("apiserver unavailable"), wantRollback: []int{1}, wantMarked: true, wantErr: "rolling back release",
			wantEvent: string(reasonHooksUnverified)},
		{name: "history unreadable: no rollback",
			pending: rev(3, helmconfig.StatusPendingUpgrade), historyErr: errors.New("secrets is forbidden"), wantErr: "reading its history"},
		{name: "history moved on since the read: no rollback",
			pending: rev(3, helmconfig.StatusPendingUpgrade),
			history: []*helmconfig.Release{rev(2, helmconfig.StatusDeployed), rev(3, helmconfig.StatusPendingUpgrade), rev(4, helmconfig.StatusDeployed)},
			wantErr: "moved on"},
		{name: "the mark cannot be written: no rollback to itself",
			pending: rev(1, helmconfig.StatusPendingInstall), history: []*helmconfig.Release{rev(1, helmconfig.StatusPendingInstall)},
			notInAPI: true, wantErr: "marking its hooks unverified first"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dyn := srFakeDyn(srPortal("1", "x"))
			if tc.notInAPI {
				dyn = srFakeDyn()
			}
			hc := &recoveryClient{history: tc.history, historyErr: tc.historyErr, rollbackErr: tc.rollbackErr, dyn: dyn}
			rec := events.NewFakeRecorder(10)
			h := &handler{eventRecorder: *event.NewAPIRecorder(rec)}

			// A zero gate: these are #563's own arms, with #629's reinstall not allowed (freshreinstall_test.go covers it).
			mg, got, _, err := h.recoverStuckOperation(context.Background(), srPortal("1", "x"), hc, tc.pending, "r", reinstallGate{}, srOpts(dyn), logging.NewNopLogger())

			if tc.wantErr == "" && err != nil {
				t.Fatalf("err = %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
			}
			if !equalInts(hc.rollbacks, tc.wantRollback) {
				t.Fatalf("rollbacks = %v, want %v", hc.rollbacks, tc.wantRollback)
			}
			for i, m := range hc.markedAtRollback {
				if m != tc.wantMarked {
					t.Errorf("rollback %d: mark on the stored object = %v, want %v", i, m, tc.wantMarked)
				}
			}
			if tc.wantErr == "" && (got == nil || got.Revision != tc.history[len(tc.history)-1].Revision+1) {
				t.Errorf("returned release = %+v, want the rollback's new revision", got)
			}
			if _, marked := hooksUnverifiedSince(mg); tc.wantMarked && !marked {
				t.Errorf("the returned object lost the mark")
			}
			var evs []string
			for len(rec.Events) > 0 {
				evs = append(evs, <-rec.Events)
			}
			if tc.wantEvent != "" && (len(evs) == 0 || !strings.Contains(strings.Join(evs, "|"), tc.wantEvent)) {
				t.Errorf("events %v, want a %s", evs, tc.wantEvent)
			}
			if tc.wantEvent == "" && strings.Contains(strings.Join(evs, "|"), string(reasonHooksUnverified)) {
				t.Errorf("events %v: a HooksUnverified Event without a rollback to itself", evs)
			}
		})
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

package composition

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
	"time"

	compositionMeta "github.com/krateo-platformops/composition-dynamic-controller/pkg/meta"
	helmconfig "github.com/krateo-platformops/plumbing/helm"
	"github.com/krateo-platformops/plumbing/kubeutil/event"
	"github.com/krateo-platformops/unstructured-runtime/pkg/logging"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/events"
)

// interruptedClient is recoveryClient plus the read-back after a settle. Uninstall is recorded: the
// recovery must never call it. Any other method panics.
type interruptedClient struct {
	recoveryClient
	settled    *helmconfig.Release // GetRelease's answer
	uninstalls int
}

func (c *interruptedClient) Uninstall(context.Context, string, *helmconfig.UninstallConfig) (*helmconfig.OperationResult, error) {
	c.uninstalls++
	return &helmconfig.OperationResult{}, nil
}

func (c *interruptedClient) GetRelease(context.Context, string, *helmconfig.GetConfig) (*helmconfig.Release, error) {
	return c.settled, nil
}

// fakeRetrier stands in for plumbing's two calls (installRetrier).
type fakeRetrier struct {
	retry   func(ctx context.Context, hc helmconfig.Client, releaseName, chartURL string, ac *helmconfig.ActionConfig, maxHistory int) (*helmconfig.OperationResult, error)
	settle  func(revision int) error
	settles []int
}

func (f *fakeRetrier) RetryFailedInstall(ctx context.Context, hc helmconfig.Client, releaseName, chartURL string, ac *helmconfig.ActionConfig, maxHistory int) (*helmconfig.OperationResult, error) {
	return f.retry(ctx, hc, releaseName, chartURL, ac, maxHistory)
}

func (f *fakeRetrier) SettleInterruptedInstall(_ context.Context, _ helmconfig.Client, _ string, revision int) error {
	f.settles = append(f.settles, revision)
	if f.settle != nil {
		return f.settle(revision)
	}
	return nil
}

// setRetrier replaces plumbing's two calls for one test.
func setRetrier(t *testing.T, r installRetrier) {
	t.Helper()
	old := retrier
	retrier = r
	t.Cleanup(func() { retrier = old })
}

func frJob(name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("batch/v1")
	u.SetKind("Job")
	u.SetNamespace("krateo-system")
	u.SetName(name)
	return u
}

func frFakeDyn(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{srGVR: "PortalList", jobsGVR: "JobList"}, objs...)
}

// pendingInstallWithHooks is an interrupted first install whose release record names two hook Jobs.
func pendingInstallWithHooks() *helmconfig.Release {
	r := rev(1, helmconfig.StatusPendingInstall)
	r.Namespace = "krateo-system"
	r.Hooks = []helmconfig.Hook{
		{Name: "db-init", Kind: "Job", Manifest: "apiVersion: batch/v1\nkind: Job\nmetadata:\n  name: db-init\n"},
		{Name: "ks-endpoints", Kind: "Job", Manifest: "apiVersion: batch/v1\nkind: Job\nmetadata:\n  name: ks-endpoints\n"},
	}
	return r
}

// interruptedAt is pendingInstallWithHooks at revision n.
func interruptedAt(n int) *helmconfig.Release {
	r := pendingInstallWithHooks()
	r.Revision = n
	return r
}

// core-provider#629 and #636 through recoverStuckOperation: which interrupted installs are settled as
// failed to be retried, which are settled for a human, and which keep #563's recovery. NO row uninstalls:
// the client would record it.
func TestRecoverStuckOperation_InterruptedFirstInstall(t *testing.T) {
	const (
		neverKey = compositionMeta.AnnotationKeyReleaseNeverDeployed
		countKey = compositionMeta.AnnotationKeyInterruptedInstallReinstalls
		swKey    = compositionMeta.AnnotationKeyInterruptedInstallRecovery
	)
	allow := func() interruptedGate {
		return interruptedGate{heldByNobody: true, createAllowed: true, cdAnnotationsKnown: true, cdAnnotations: map[string]string{}}
	}
	with := func(f func(*interruptedGate)) func() interruptedGate {
		return func() interruptedGate { g := allow(); f(&g); return g }
	}
	noRecord := map[string]string{}
	type outcome int
	const (
		retried  outcome = iota // settled, to be retried: counted, recorded, hook Jobs gone
		settled                 // settled for a human: marked hooks-unverified, nothing deleted
		rollback                // #563: rolled back, to wantTarget
		stopped                 // an error, nothing settled and nothing rolled back
	)
	for _, tc := range []struct {
		name        string
		pending     *helmconfig.Release // default: revision 1, with hooks
		history     []*helmconfig.Release
		historyErr  error
		gate        func() interruptedGate // default: allow
		composition map[string]string
		record      map[string]string // status.helmOperation; nil = this controller's record of the install
		jobStuck    bool              // a deleted hook Job does not go away
		jobErr      error
		settleErr   error
		readBack    *helmconfig.Release // default: the pending revision, failed
		denyPatch   string
		denyStatus  bool
		want        outcome
		wantTarget  int
		wantErr     string
		unmarked    bool // rollback: to a deployed revision, so no mark
		wantSkipped bool // rollback: this pass's upgrade must not clear the mark
		wantEvent   event.Reason
		settleCalls int // default: 1 when settled or retried
	}{
		{name: "revision 1: settled, to be retried", want: retried},
		{name: "revision 2, above a failed install: settled, to be retried", pending: interruptedAt(2),
			history: []*helmconfig.Release{rev(1, helmconfig.StatusFailed), interruptedAt(2)}, want: retried},
		{name: "revision 6, the first five pruned, accounted for by the mark", pending: interruptedAt(6), history: append(failedRevs(4, 5), interruptedAt(6)),
			composition: map[string]string{neverKey: ndMark("", "r", 5)}, want: retried},

		// #563's recovery exactly: not proven never deployed, or not this controller's.
		{name: "control: a deployed revision exists: rolled back to it", pending: rev(3, helmconfig.StatusPendingUpgrade),
			history: []*helmconfig.Release{rev(2, helmconfig.StatusDeployed), rev(3, helmconfig.StatusPendingUpgrade)}, want: rollback, wantTarget: 2, unmarked: true},
		{name: "ONCE LIVE, pruned to failed revisions: rolled back to itself", pending: interruptedAt(6), history: append(failedRevs(4, 5), interruptedAt(6)),
			want: rollback, wantTarget: 6, wantEvent: reasonHooksUnverified},
		{name: "live once (uninstalled with kept history, then reinstalled)", pending: interruptedAt(2),
			history: []*helmconfig.Release{rev(1, helmconfig.StatusUninstalled), interruptedAt(2)}, want: rollback, wantTarget: 2, wantEvent: reasonHooksUnverified},
		{name: "a superseded revision exists", pending: interruptedAt(2),
			history: []*helmconfig.Release{rev(1, helmconfig.StatusSuperseded), interruptedAt(2)}, want: rollback, wantTarget: 2, wantEvent: reasonHooksUnverified},
		{name: "HAND-INSTALLED: no operation recorded", record: noRecord, want: rollback, wantTarget: 1, wantEvent: reasonHooksUnverified},
		{name: "HAND-INSTALLED at revision 2: no operation recorded", record: noRecord, pending: interruptedAt(2),
			history: []*helmconfig.Release{rev(1, helmconfig.StatusFailed), interruptedAt(2)}, want: rollback, wantTarget: 2, wantEvent: reasonHooksUnverified},
		{name: "another operation's record", record: map[string]string{"phase": "reconcile", "startedAt": "0001-01-01T00:00:00Z"}, want: rollback, wantTarget: 1, wantEvent: reasonHooksUnverified},
		{name: "an install record written AFTER the revision", record: map[string]string{"phase": "install", "startedAt": "0001-01-01T00:00:01Z"}, want: rollback, wantTarget: 1, wantEvent: reasonHooksUnverified},
		{name: "an install record too long before the revision", record: map[string]string{"phase": "install", "startedAt": "0001-01-01T00:00:00Z"},
			pending: func() *helmconfig.Release {
				r := interruptedAt(1)
				r.Updated = r.Updated.Add(2 * orphanWindow)
				return r
			}(),
			want: rollback, wantTarget: 1, wantEvent: reasonHooksUnverified},
		{name: "an unreadable record", record: map[string]string{"phase": "install", "startedAt": "yesterday"}, want: rollback, wantTarget: 1, wantEvent: reasonHooksUnverified},
		{name: "the never-deployed mark alone is not the record", record: noRecord, composition: map[string]string{neverKey: ndMark("", "r", 1)}, want: rollback, wantTarget: 1, wantEvent: reasonHooksUnverified},

		// Proven and this controller's, and not retried. Revision 1: rolled back to itself, and the upgrade
		// this pass may run must not clear the mark.
		{name: "kill switch on the CompositionDefinition, revision 1", gate: with(func(g *interruptedGate) { g.cdAnnotations = map[string]string{swKey: "manual"} }),
			want: rollback, wantTarget: 1, wantSkipped: true, wantEvent: reasonHooksUnverified},
		{name: "kill switch on the composition, an unrecognised value, revision 1", composition: map[string]string{swKey: "auto"},
			want: rollback, wantTarget: 1, wantSkipped: true, wantEvent: reasonHooksUnverified},
		{name: "the CompositionDefinition's annotations unread, revision 1", gate: with(func(g *interruptedGate) { g.cdAnnotationsKnown = false }),
			want: rollback, wantTarget: 1, wantSkipped: true, wantEvent: reasonHooksUnverified},
		{name: "the management policy withholds create, revision 1", gate: with(func(g *interruptedGate) { g.createAllowed = false }),
			want: rollback, wantTarget: 1, wantSkipped: true, wantEvent: reasonHooksUnverified},
		{name: "cap reached, revision 1", composition: map[string]string{countKey: "3"}, want: rollback, wantTarget: 1, wantSkipped: true, wantEvent: reasonInterruptedInstallRetryCapped},
		{name: "control: below the cap", composition: map[string]string{countKey: "2"}, want: retried},
		{name: "an unreadable counter, revision 1", composition: map[string]string{countKey: "many"}, want: rollback, wantTarget: 1, wantSkipped: true, wantEvent: reasonInterruptedInstallRetryCapped},
		// Revision 2 and above NEVER rolls back: its stored manifest is not a render.
		{name: "kill switch, revision 2: settled, marked, not rolled back", pending: interruptedAt(2), composition: map[string]string{swKey: "manual"},
			history: []*helmconfig.Release{rev(1, helmconfig.StatusFailed), interruptedAt(2)}, want: settled, wantEvent: reasonHooksUnverified},
		{name: "cap reached, revision 2: settled, marked, not rolled back", pending: interruptedAt(2), composition: map[string]string{countKey: "3"},
			history: []*helmconfig.Release{rev(1, helmconfig.StatusFailed), interruptedAt(2)}, want: settled, wantEvent: reasonInterruptedInstallRetryCapped},
		{name: "create withheld, revision 2: settled, marked, not rolled back", pending: interruptedAt(2), gate: with(func(g *interruptedGate) { g.createAllowed = false }),
			history: []*helmconfig.Release{rev(1, helmconfig.StatusFailed), interruptedAt(2)}, want: settled, wantEvent: reasonHooksUnverified},

		// Stopped, with the release still pending-install.
		{name: "SOMEBODY MAY BE RUNNING IT: not settled", gate: with(func(g *interruptedGate) { g.heldByNobody = false }), want: stopped, wantErr: "may still be running it"},
		{name: "history unreadable", historyErr: errors.New("secrets is forbidden"), want: stopped, wantErr: "reading its history"},
		{name: "the attempt cannot be counted", denyPatch: countKey, want: stopped, wantErr: "counting the attempt first"},
		{name: "the durable record cannot be written", denyStatus: true, want: stopped, wantErr: "recording it first"},
		{name: "the never-deployed mark cannot be written", denyPatch: neverKey, want: stopped, wantErr: "marking the release never deployed first"},
		{name: "the hooks-unverified mark cannot be written, revision 2 under the switch", denyPatch: compositionMeta.AnnotationKeyHooksUnverified, pending: interruptedAt(2),
			composition: map[string]string{swKey: "manual"}, history: []*helmconfig.Release{rev(1, helmconfig.StatusFailed), interruptedAt(2)}, want: stopped, wantErr: "marking its hooks unverified first"},
		{name: "a hook Job cannot be deleted", jobErr: errors.New("jobs is forbidden"), want: stopped, wantErr: "could not be deleted"},
		{name: "a hook Job is still there: its pod has not stopped", jobStuck: true, want: stopped, wantErr: "still exists"},
		{name: "refused: it moved on. No rollback instead", settleErr: errRetryRefused, want: stopped, wantErr: errRetryRefused.Error(), settleCalls: 1},
		{name: "the read-back is not the revision settled", readBack: interruptedAt(1), want: stopped, wantErr: "not revision 1 failed", settleCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldTimeout, oldPoll := hookJobsGoneTimeout, hookJobsGonePoll
			hookJobsGoneTimeout, hookJobsGonePoll = 50*time.Millisecond, 10*time.Millisecond
			defer func() { hookJobsGoneTimeout, hookJobsGonePoll = oldTimeout, oldPoll }()
			ctx := context.Background()
			pending := tc.pending
			if pending == nil {
				pending = interruptedAt(1)
			}
			history := tc.history
			if history == nil {
				history = []*helmconfig.Release{pending}
			}
			portal := srPortal("1", "x")
			portal.SetAnnotations(tc.composition)
			if tc.record == nil {
				withInstallRecord(portal, pending)
			} else if len(tc.record) > 0 {
				_ = unstructured.SetNestedStringMap(portal.Object, tc.record, "status", "helmOperation")
			}
			dyn := frFakeDyn(portal, frJob("db-init"), frJob("unrelated"))
			stored := func() *unstructured.Unstructured {
				u, err := dyn.Tracker().Get(srGVR, "krateo-system", "portal")
				if err != nil {
					t.Fatal(err)
				}
				return u.(*unstructured.Unstructured)
			}
			jobThere := func(n string) bool { _, err := dyn.Tracker().Get(jobsGVR, "krateo-system", n); return err == nil }
			if tc.jobErr != nil || tc.jobStuck {
				dyn.PrependReactor("delete", "jobs", func(clienttesting.Action) (bool, runtime.Object, error) { return true, nil, tc.jobErr })
			}
			if tc.denyPatch != "" {
				dyn.PrependReactor("patch", "portals", func(a clienttesting.Action) (bool, runtime.Object, error) {
					if strings.Contains(string(a.(clienttesting.PatchAction).GetPatch()), tc.denyPatch) {
						return true, nil, errors.New("denied")
					}
					return false, nil, nil
				})
			}
			if tc.denyStatus {
				dyn.PrependReactor("update", "portals", func(clienttesting.Action) (bool, runtime.Object, error) { return true, nil, errors.New("denied") })
			}
			readBack := tc.readBack
			if readBack == nil {
				readBack = rev(pending.Revision, helmconfig.StatusFailed)
			}
			hc := &interruptedClient{recoveryClient: recoveryClient{history: history, historyErr: tc.historyErr, dyn: dyn}, settled: readBack}
			fr := &fakeRetrier{settle: func(int) error {
				u := stored()
				if n, _ := readNeverDeployedMark(u, "r"); n != pending.Revision {
					t.Errorf("settled before the never-deployed mark: it names %d", n)
				}
				_, marked := hooksUnverifiedSince(u)
				kind, _, _ := unstructured.NestedString(u.Object, "status", "installRecovery", "kind")
				if tc.want == settled {
					if !marked || kind != "" || !jobThere("db-init") {
						t.Errorf("settled for a human: marked %v installRecovery %q hook Job there %v", marked, kind, jobThere("db-init"))
					}
				} else if kind != installRecoveryRetried || u.GetAnnotations()[countKey] == "" || jobThere("db-init") {
					t.Errorf("settled for a retry before it was recorded (%q), counted (%q) or its hook Job gone (there: %v)", kind, u.GetAnnotations()[countKey], jobThere("db-init"))
				}
				return tc.settleErr
			}}
			setRetrier(t, fr)
			rec := events.NewFakeRecorder(20)
			h := &handler{eventRecorder: *event.NewAPIRecorder(rec)}
			gate := allow()
			if tc.gate != nil {
				gate = tc.gate()
			}
			gate.dyn = dyn

			mg, got, err := h.recoverStuckOperation(ctx, portal.DeepCopy(), hc, pending, "r", gate, srOpts(dyn), logging.NewNopLogger())

			if (tc.want == stopped) != (err != nil) || (err != nil && !strings.Contains(err.Error(), tc.wantErr)) || (tc.want == stopped && tc.wantErr == "") {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
			if hc.uninstalls != 0 {
				t.Errorf("%d uninstalls: this controller never uninstalls a release by itself", hc.uninstalls)
			}
			wantSettles := tc.settleCalls
			if tc.want == retried || tc.want == settled {
				wantSettles = 1
			}
			if len(fr.settles) != wantSettles || (wantSettles == 1 && fr.settles[0] != pending.Revision) {
				t.Errorf("settled revisions %v, want %d call(s) for revision %d", fr.settles, wantSettles, pending.Revision)
			}
			wantRollbacks := []int(nil)
			if tc.want == rollback {
				wantRollbacks = []int{tc.wantTarget}
			}
			if !equalInts(hc.rollbacks, wantRollbacks) {
				t.Errorf("rollbacks = %v, want %v", hc.rollbacks, wantRollbacks)
			}
			_, marked := hooksUnverifiedSince(mg)
			recovery, _, _ := unstructured.NestedMap(stored().Object, "status", "installRecovery")
			switch tc.want {
			case retried, settled:
				if got.rel == nil || got.rel.Status != helmconfig.StatusFailed || !got.interrupted {
					t.Errorf("carried on with %s, interrupted %v; want the revision settled as failed", releaseState(got.rel), got.interrupted)
				}
				if marked != (tc.want == settled) {
					t.Errorf("hooks-unverified = %v", marked)
				}
				if (recovery != nil) != (tc.want == retried) {
					t.Errorf("status.installRecovery = %v", recovery)
				}
				if tc.want == retried && (recovery["count"] != int64(1) || recovery["reason"] != installRecoveryInterrupted || !strings.Contains(recovery["message"].(string), "was interrupted before")) {
					t.Errorf("status.installRecovery = %v", recovery)
				}
			case rollback:
				if got.interrupted || got.installHooksSkipped != tc.wantSkipped || marked == tc.unmarked || recovery != nil {
					t.Errorf("interrupted %v, hooks skipped %v (want %v), marked %v (want %v), installRecovery %v", got.interrupted, got.installHooksSkipped, tc.wantSkipped, marked, !tc.unmarked, recovery)
				}
			case stopped:
				// The record proves the install is this controller's: it must still be there for the next pass.
				if phase, _, _ := unstructured.NestedString(stored().Object, "status", "helmOperation", "phase"); phase != "install" {
					t.Errorf("stored operation record's phase = %q after a pass that stopped, want install", phase)
				}
			}
			if !jobThere("unrelated") || (tc.want != retried && tc.settleCalls == 0 && !jobThere("db-init")) {
				t.Errorf("unrelated Job there %v, the interrupted install's hook Job there %v", jobThere("unrelated"), jobThere("db-init"))
			}
			var evs []string
			for len(rec.Events) > 0 {
				evs = append(evs, <-rec.Events)
			}
			if tc.wantEvent != "" && !strings.Contains(strings.Join(evs, "|"), string(tc.wantEvent)) {
				t.Errorf("events %v, want a %s", evs, tc.wantEvent)
			}
		})
	}
}

// This controller uninstalls a release in ONE place, a composition's Delete. The count is the control: a
// walk that found none would prove nothing.
func TestOnlyDeleteUninstallsARelease(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var sites []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range parsed.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if c, ok := n.(*ast.CallExpr); ok {
					if sel, ok := c.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Uninstall" {
						sites = append(sites, f+":"+fn.Name.Name)
					}
				}
				return true
			})
		}
	}
	if len(sites) != 1 || sites[0] != "composition.go:Delete" {
		t.Errorf("helm uninstalls at %v, want composition.go:Delete alone", sites)
	}
}

func TestNeverDeployed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pending *helmconfig.Release
		history []*helmconfig.Release
		want    bool
	}{
		{"first install, alone", rev(1, helmconfig.StatusPendingInstall), []*helmconfig.Release{rev(1, helmconfig.StatusPendingInstall)}, true},
		{"after failed installs", rev(3, helmconfig.StatusPendingInstall), []*helmconfig.Release{rev(1, helmconfig.StatusFailed), rev(2, helmconfig.StatusFailed), rev(3, helmconfig.StatusPendingInstall)}, true},
		{"an upgrade", rev(2, helmconfig.StatusPendingUpgrade), []*helmconfig.Release{rev(1, helmconfig.StatusDeployed), rev(2, helmconfig.StatusPendingUpgrade)}, false},
		{"deployed before", rev(2, helmconfig.StatusPendingInstall), []*helmconfig.Release{rev(1, helmconfig.StatusDeployed), rev(2, helmconfig.StatusPendingInstall)}, false},
		{"superseded before", rev(2, helmconfig.StatusPendingInstall), []*helmconfig.Release{rev(1, helmconfig.StatusSuperseded), rev(2, helmconfig.StatusPendingInstall)}, false},
		{"uninstalled before", rev(2, helmconfig.StatusPendingInstall), []*helmconfig.Release{rev(1, helmconfig.StatusUninstalled), rev(2, helmconfig.StatusPendingInstall)}, false},
		{"unknown before", rev(2, helmconfig.StatusPendingInstall), []*helmconfig.Release{rev(1, helmconfig.StatusUnknown), rev(2, helmconfig.StatusPendingInstall)}, false},
		{"empty history", rev(1, helmconfig.StatusPendingInstall), nil, false},
		{"a nil entry", rev(1, helmconfig.StatusPendingInstall), []*helmconfig.Release{nil, rev(1, helmconfig.StatusPendingInstall)}, false},
		{"pending not in history", rev(2, helmconfig.StatusPendingInstall), []*helmconfig.Release{rev(1, helmconfig.StatusFailed)}, false},
		{"history disagrees about the pending revision", rev(1, helmconfig.StatusPendingInstall), []*helmconfig.Release{rev(1, helmconfig.StatusDeployed)}, false},
		{"no release", nil, []*helmconfig.Release{rev(1, helmconfig.StatusPendingInstall)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, why := neverDeployed(srPortal("1", "x"), "r", tc.pending, tc.history); got != tc.want {
				t.Errorf("neverDeployed = %v (%s), want %v", got, why, tc.want)
			}
		})
	}
}

// The counter is removed once the release is deployed, and only then.
func TestClearInterruptedInstallAttempts(t *testing.T) {
	for _, tc := range []struct {
		name string
		rel  *helmconfig.Release
		want string
	}{
		{"deployed clears", rev(1, helmconfig.StatusDeployed), ""},
		{"pending keeps", rev(1, helmconfig.StatusPendingInstall), "2"},
		{"failed keeps", rev(1, helmconfig.StatusFailed), "2"},
		{"no release keeps", nil, "2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			portal := srPortal("1", "x")
			portal.SetAnnotations(map[string]string{compositionMeta.AnnotationKeyInterruptedInstallReinstalls: "2"})
			dyn := frFakeDyn(portal)
			out := clearInterruptedInstallAttempts(context.Background(), portal.DeepCopy(), tc.rel, srOpts(dyn), logging.NewNopLogger())
			if got := out.GetAnnotations()[compositionMeta.AnnotationKeyInterruptedInstallReinstalls]; got != tc.want {
				t.Errorf("counter = %q, want %q", got, tc.want)
			}
		})
	}
}

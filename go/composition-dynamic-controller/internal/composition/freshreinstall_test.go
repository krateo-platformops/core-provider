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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/events"
)

// reinstallClient is recoveryClient plus the two calls #629's reinstall makes: Uninstall (recording
// whether hooks were disabled) and the GetRelease that confirms the release is gone.
type reinstallClient struct {
	recoveryClient
	uninstallErr   error
	uninstalls     []bool // DisableHooks of each Uninstall call
	stillThere     bool   // GetRelease after the uninstall still returns a release
	countAtUninst  []string
	jobsAtUninstal []int
}

func (c *reinstallClient) Uninstall(ctx context.Context, _ string, cfg *helmconfig.UninstallConfig) (*helmconfig.OperationResult, error) {
	c.uninstalls = append(c.uninstalls, cfg.DisableHooks)
	if u, err := c.dyn.Resource(srGVR).Namespace("krateo-system").Get(ctx, "portal", metav1.GetOptions{}); err == nil {
		c.countAtUninst = append(c.countAtUninst, u.GetAnnotations()[compositionMeta.AnnotationKeyInterruptedInstallReinstalls])
	}
	l, _ := c.dyn.Resource(jobsGVR).Namespace("krateo-system").List(ctx, metav1.ListOptions{})
	n := 0
	if l != nil {
		n = len(l.Items)
	}
	c.jobsAtUninstal = append(c.jobsAtUninstal, n)
	if c.uninstallErr != nil {
		return nil, c.uninstallErr
	}
	return &helmconfig.OperationResult{}, nil
}

func (c *reinstallClient) GetRelease(context.Context, string, *helmconfig.GetConfig) (*helmconfig.Release, error) {
	if c.stillThere {
		return rev(1, helmconfig.StatusPendingInstall), nil
	}
	return nil, nil
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

// core-provider#629 through recoverStuckOperation: which interrupted installs are reinstalled (and
// how), and which keep #563's human gate. Every control is a reason NOT to uninstall.
func TestRecoverStuckOperation_InterruptedFreshInstall(t *testing.T) {
	allow := func() reinstallGate {
		return reinstallGate{policyAllows: true, cdAnnotationsKnown: true, cdAnnotations: map[string]string{}}
	}
	for _, tc := range []struct {
		name          string
		pending       *helmconfig.Release
		history       []*helmconfig.Release
		historyErr    error
		gate          func() reinstallGate
		composition   map[string]string // extra annotations on the composition
		jobDeleteErr  error
		uninstallErr  error
		stillThere    bool
		wantReinstall bool // uninstalled without hooks and reported absent
		wantRollback  []int
		wantMarked    bool
		wantErr       string
		wantEvent     event.Reason
		wantCount     string // the counter on the stored object afterwards ("" = absent)
		wantJobsLeft  []string
	}{
		{name: "never deployed: uninstalled WITHOUT hooks, its hook Jobs deleted, reported absent",
			pending: pendingInstallWithHooks(), history: []*helmconfig.Release{pendingInstallWithHooks()},
			gate: allow, wantReinstall: true, wantEvent: reasonInterruptedInstallReinstalled, wantCount: "1",
			wantJobsLeft: []string{"unrelated"}},
		{name: "never deployed after an earlier FAILED install: still reinstalled",
			pending: func() *helmconfig.Release { r := pendingInstallWithHooks(); r.Revision = 2; return r }(),
			history: []*helmconfig.Release{rev(1, helmconfig.StatusFailed), func() *helmconfig.Release { r := pendingInstallWithHooks(); r.Revision = 2; return r }()},
			gate:    allow, wantReinstall: true, wantEvent: reasonInterruptedInstallReinstalled, wantCount: "1", wantJobsLeft: []string{"unrelated"}},
		{name: "control: a deployed revision exists: #563 rollback to it, no uninstall",
			pending: rev(3, helmconfig.StatusPendingUpgrade),
			history: []*helmconfig.Release{rev(2, helmconfig.StatusDeployed), rev(3, helmconfig.StatusPendingUpgrade)},
			gate:    allow, wantRollback: []int{2}, wantJobsLeft: []string{"db-init", "unrelated"}},
		{name: "control: live once (uninstalled with kept history, then reinstalled): gate, no uninstall",
			pending: func() *helmconfig.Release { r := pendingInstallWithHooks(); r.Revision = 2; return r }(),
			history: []*helmconfig.Release{rev(1, helmconfig.StatusUninstalled), func() *helmconfig.Release { r := pendingInstallWithHooks(); r.Revision = 2; return r }()},
			gate:    allow, wantRollback: []int{2}, wantMarked: true, wantEvent: reasonHooksUnverified, wantJobsLeft: []string{"db-init", "unrelated"}},
		{name: "control: a superseded revision exists: gate, no uninstall",
			pending: func() *helmconfig.Release { r := pendingInstallWithHooks(); r.Revision = 2; return r }(),
			history: []*helmconfig.Release{rev(1, helmconfig.StatusSuperseded), func() *helmconfig.Release { r := pendingInstallWithHooks(); r.Revision = 2; return r }()},
			gate:    allow, wantRollback: []int{2}, wantMarked: true, wantEvent: reasonHooksUnverified, wantJobsLeft: []string{"db-init", "unrelated"}},
		{name: "kill switch on the CompositionDefinition: gate",
			pending: pendingInstallWithHooks(), history: []*helmconfig.Release{pendingInstallWithHooks()},
			gate: func() reinstallGate {
				g := allow()
				g.cdAnnotations = map[string]string{compositionMeta.AnnotationKeyInterruptedInstallRecovery: compositionMeta.InterruptedInstallRecoveryManual}
				return g
			},
			wantRollback: []int{1}, wantMarked: true, wantEvent: reasonHooksUnverified, wantJobsLeft: []string{"db-init", "unrelated"}},
		{name: "kill switch on the composition, an unrecognised value: still the gate",
			pending: pendingInstallWithHooks(), history: []*helmconfig.Release{pendingInstallWithHooks()},
			gate: allow, composition: map[string]string{compositionMeta.AnnotationKeyInterruptedInstallRecovery: "auto"},
			wantRollback: []int{1}, wantMarked: true, wantEvent: reasonHooksUnverified, wantJobsLeft: []string{"db-init", "unrelated"}},
		{name: "the CompositionDefinition's annotations unread: gate",
			pending: pendingInstallWithHooks(), history: []*helmconfig.Release{pendingInstallWithHooks()},
			gate:         func() reinstallGate { g := allow(); g.cdAnnotationsKnown, g.cdAnnotations = false, nil; return g },
			wantRollback: []int{1}, wantMarked: true, wantEvent: reasonHooksUnverified, wantJobsLeft: []string{"db-init", "unrelated"}},
		{name: "the management policy withholds create or delete: gate",
			pending: pendingInstallWithHooks(), history: []*helmconfig.Release{pendingInstallWithHooks()},
			gate:         func() reinstallGate { g := allow(); g.policyAllows = false; return g },
			wantRollback: []int{1}, wantMarked: true, wantEvent: reasonHooksUnverified, wantJobsLeft: []string{"db-init", "unrelated"}},
		{name: "cap reached: gate, and a Warning says so",
			pending: pendingInstallWithHooks(), history: []*helmconfig.Release{pendingInstallWithHooks()},
			gate: allow, composition: map[string]string{compositionMeta.AnnotationKeyInterruptedInstallReinstalls: "3"},
			wantRollback: []int{1}, wantMarked: true, wantEvent: reasonInterruptedInstallReinstallCapped, wantCount: "3",
			wantJobsLeft: []string{"db-init", "unrelated"}},
		{name: "an unreadable counter: gate",
			pending: pendingInstallWithHooks(), history: []*helmconfig.Release{pendingInstallWithHooks()},
			gate: allow, composition: map[string]string{compositionMeta.AnnotationKeyInterruptedInstallReinstalls: "many"},
			wantRollback: []int{1}, wantMarked: true, wantEvent: reasonHooksUnverified, wantCount: "many",
			wantJobsLeft: []string{"db-init", "unrelated"}},
		{name: "history unreadable: no uninstall, no rollback",
			pending: pendingInstallWithHooks(), historyErr: errors.New("secrets is forbidden"),
			gate: allow, wantErr: "reading its history", wantJobsLeft: []string{"db-init", "unrelated"}},
		{name: "a hook Job cannot be deleted: counted, but no uninstall",
			pending: pendingInstallWithHooks(), history: []*helmconfig.Release{pendingInstallWithHooks()},
			gate: allow, jobDeleteErr: errors.New("jobs is forbidden"), wantErr: "could not be deleted", wantCount: "1",
			wantJobsLeft: []string{"db-init", "unrelated"}},
		{name: "the uninstall fails: an error, the attempt counted",
			pending: pendingInstallWithHooks(), history: []*helmconfig.Release{pendingInstallWithHooks()},
			gate: allow, uninstallErr: errors.New("apiserver unavailable"), wantErr: "uninstalling the interrupted install", wantCount: "1",
			wantJobsLeft: []string{"unrelated"}},
		{name: "the release survives the uninstall: an error, not reported absent",
			pending: pendingInstallWithHooks(), history: []*helmconfig.Release{pendingInstallWithHooks()},
			gate: allow, stillThere: true, wantErr: "still exists", wantCount: "1", wantJobsLeft: []string{"unrelated"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			portal := srPortal("1", "x")
			if len(tc.composition) > 0 {
				portal.SetAnnotations(tc.composition)
			}
			dyn := frFakeDyn(portal, frJob("db-init"), frJob("unrelated"))
			if tc.jobDeleteErr != nil {
				dyn.PrependReactor("delete", "jobs", func(clienttesting.Action) (bool, runtime.Object, error) { return true, nil, tc.jobDeleteErr })
			}
			hc := &reinstallClient{recoveryClient: recoveryClient{history: tc.history, historyErr: tc.historyErr, dyn: dyn},
				uninstallErr: tc.uninstallErr, stillThere: tc.stillThere}
			rec := events.NewFakeRecorder(10)
			h := &handler{eventRecorder: *event.NewAPIRecorder(rec)}
			gate := tc.gate()
			gate.dyn = dyn

			mg, _, uninstalled, err := h.recoverStuckOperation(context.Background(), portal.DeepCopy(), hc, tc.pending, "r", gate, srOpts(dyn), logging.NewNopLogger())

			if tc.wantErr == "" && err != nil {
				t.Fatalf("err = %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
			}
			if uninstalled != tc.wantReinstall {
				t.Fatalf("uninstalled = %v, want %v", uninstalled, tc.wantReinstall)
			}
			wantUninstallCalls := tc.wantReinstall || tc.uninstallErr != nil || tc.stillThere
			if wantUninstallCalls != (len(hc.uninstalls) > 0) {
				t.Fatalf("uninstall calls = %v, want calls: %v", hc.uninstalls, wantUninstallCalls)
			}
			for i, disabled := range hc.uninstalls {
				if !disabled {
					t.Errorf("uninstall %d ran WITH hooks: an interrupted install's pre-delete hooks (an openstack DB drop) must not run", i)
				}
				if hc.countAtUninst[i] == "" {
					t.Errorf("uninstall %d ran before the attempt was counted on the stored object", i)
				}
				if hc.jobsAtUninstal[i] != 1 {
					t.Errorf("uninstall %d ran with %d Jobs present, want only the unrelated one left (hook Jobs deleted FIRST)", i, hc.jobsAtUninstal[i])
				}
			}
			if !equalInts(hc.rollbacks, tc.wantRollback) {
				t.Fatalf("rollbacks = %v, want %v", hc.rollbacks, tc.wantRollback)
			}
			if _, marked := hooksUnverifiedSince(mg); marked != tc.wantMarked {
				t.Errorf("hooks-unverified on the returned object = %v, want %v", marked, tc.wantMarked)
			}
			stored, gerr := dyn.Resource(srGVR).Namespace("krateo-system").Get(context.Background(), "portal", metav1.GetOptions{})
			if gerr != nil {
				t.Fatal(gerr)
			}
			if got := stored.GetAnnotations()[compositionMeta.AnnotationKeyInterruptedInstallReinstalls]; got != tc.wantCount {
				t.Errorf("stored counter = %q, want %q", got, tc.wantCount)
			}
			var left []string
			for _, n := range []string{"db-init", "ks-endpoints", "unrelated"} {
				if _, err := dyn.Resource(jobsGVR).Namespace("krateo-system").Get(context.Background(), n, metav1.GetOptions{}); err == nil {
					left = append(left, n)
				} else if !apierrors.IsNotFound(err) {
					t.Fatal(err)
				}
			}
			if strings.Join(left, ",") != strings.Join(tc.wantJobsLeft, ",") {
				t.Errorf("Jobs left = %v, want %v", left, tc.wantJobsLeft)
			}
			var evs []string
			for len(rec.Events) > 0 {
				evs = append(evs, <-rec.Events)
			}
			if tc.wantEvent != "" && !strings.Contains(strings.Join(evs, "|"), string(tc.wantEvent)) {
				t.Errorf("events %v, want a %s", evs, tc.wantEvent)
			}
			if !tc.wantReinstall && strings.Contains(strings.Join(evs, "|"), string(reasonInterruptedInstallReinstalled)) {
				t.Errorf("events %v: a reinstall Event without a reinstall", evs)
			}
		})
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
			if got, why := neverDeployed(tc.pending, tc.history); got != tc.want {
				t.Errorf("neverDeployed = %v (%s), want %v", got, why, tc.want)
			}
		})
	}
}

// The counter is removed once the release is deployed, and only then.
func TestClearInterruptedInstallReinstalls(t *testing.T) {
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
			out := clearInterruptedInstallReinstalls(context.Background(), portal.DeepCopy(), tc.rel, srOpts(dyn), logging.NewNopLogger())
			if got := out.GetAnnotations()[compositionMeta.AnnotationKeyInterruptedInstallReinstalls]; got != tc.want {
				t.Errorf("counter = %q, want %q", got, tc.want)
			}
		})
	}
}

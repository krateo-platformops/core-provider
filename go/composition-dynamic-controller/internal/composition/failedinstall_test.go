package composition

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/krateo-platformops/composition-dynamic-controller/internal/conditionreason"
	"github.com/krateo-platformops/composition-dynamic-controller/internal/tools/archive"
	compositionMeta "github.com/krateo-platformops/composition-dynamic-controller/pkg/meta"
	helmconfig "github.com/krateo-platformops/plumbing/helm"
	"github.com/krateo-platformops/plumbing/kubeutil/event"
	"github.com/krateo-platformops/unstructured-runtime/pkg/logging"
	"github.com/krateo-platformops/unstructured-runtime/pkg/meta"
	"github.com/krateo-platformops/unstructured-runtime/pkg/tools"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/events"
)

func fiGate(dyn *dynamicfake.FakeDynamicClient) failedInstallGate {
	g := failedInstallGate{createAllowed: true, cdAnnotationsKnown: true, cdAnnotations: map[string]string{}}
	if dyn != nil {
		g.dyn = dyn
	}
	return g
}

// core-provider#636: what a pass does about a failed release. Each row that does not install again
// differs from the first one by what its name says.
func TestDecideFailedInstall(t *testing.T) {
	sw := compositionMeta.AnnotationKeyInterruptedInstallRecovery
	for _, tc := range []struct {
		name    string
		mark    string
		mgAnn   map[string]string
		record  map[string]string // status.helmOperation; nil = this controller's record of the install
		history []*helmconfig.Release
		gate    func(*failedInstallGate)
		want    failedInstallPlan
		wantWhy string
	}{
		{name: "never deployed, this controller's install recorded", want: planInstallAgain},
		{name: "HAND-INSTALLED: never deployed, no record and no mark: today's upgrade", record: map[string]string{}, want: planUpgrade, wantWhy: "not known to have started"},
		{name: "control: no record, this controller's mark", record: map[string]string{}, mark: ndMark(ndUID, "r", 1), want: planInstallAgain},
		{name: "another operation's record", record: map[string]string{"phase": "reconcile", "startedAt": "0001-01-01T00:00:00Z"}, want: planUpgrade, wantWhy: "not an install"},
		{name: "an install record of another moment", record: map[string]string{"phase": "install", "startedAt": "0001-01-01T00:00:01Z"}, want: planUpgrade, wantWhy: "not revision 1's"},
		{name: "an unreadable record", record: map[string]string{"phase": "install", "startedAt": "x"}, want: planUpgrade, wantWhy: "unreadable"},
		{name: "a mark for another release is no proof", record: map[string]string{}, mark: ndMark(ndUID, "other", 1), want: planUpgrade},
		{name: "once live and pruned: today's upgrade", history: failedRevs(4, 6), want: planUpgrade, wantWhy: "may have been deployed and pruned"},
		{name: "control: the same history with the mark", mark: ndMark(ndUID, "r", 5), history: failedRevs(4, 6), want: planInstallAgain},
		{name: "a revision was deployed: today's upgrade", want: planUpgrade, wantWhy: "live once",
			history: []*helmconfig.Release{rev(1, helmconfig.StatusDeployed), rev(2, helmconfig.StatusFailed)}},
		{name: "kill switch on the CompositionDefinition", gate: func(g *failedInstallGate) { g.cdAnnotations = map[string]string{sw: "manual"} },
			want: planUpgradeUnverified, wantWhy: "the CompositionDefinition sets"},
		{name: "kill switch on the composition, an unrecognised value", mgAnn: map[string]string{sw: "auto"}, want: planUpgradeUnverified, wantWhy: "the composition sets"},
		{name: "the CompositionDefinition's annotations unread", gate: func(g *failedInstallGate) { g.cdAnnotationsKnown = false },
			want: planUpgradeUnverified, wantWhy: "could not be read"},
		{name: "the policy withholds create", gate: func(g *failedInstallGate) { g.createAllowed = false }, want: planUpgradeUnverified, wantWhy: "withholds create"},
		{name: "the policy withholds update (Observe)", gate: func(g *failedInstallGate) { g.updateWithheld = true }, want: planReport, wantWhy: "withholds update"},
		// The cap is for INTERRUPTED installs. Once one reached it, it is a human's until they remove the mark,
		// across passes; an install that FAILED is never capped.
		{name: "an interrupted install at the cap, marked: upgraded", mgAnn: map[string]string{compositionMeta.AnnotationKeyInterruptedInstallReinstalls: "3", compositionMeta.AnnotationKeyHooksUnverified: hooksMark},
			want: planUpgradeUnverified, wantWhy: "the cap"},
		{name: "control: a failed install, the counter at the cap: installed again", mgAnn: map[string]string{compositionMeta.AnnotationKeyInterruptedInstallReinstalls: "3"}, want: planInstallAgain},
		{name: "control: marked, below the cap (the kill switch, since removed): installed again",
			mgAnn: map[string]string{compositionMeta.AnnotationKeyInterruptedInstallReinstalls: "2", compositionMeta.AnnotationKeyHooksUnverified: hooksMark}, want: planInstallAgain},
		{name: "retiring", gate: func(g *failedInstallGate) { g.retiring, g.retiringWhy = true, "chart 0.1.0" }, want: planLeave, wantWhy: "chart 0.1.0"},
		{name: "retiring outranks every other reason", gate: func(g *failedInstallGate) {
			g.retiring, g.updateWithheld, g.createAllowed = true, true, false
			g.cdAnnotations = map[string]string{sw: "manual"}
		}, want: planLeave},
		{name: "a withheld update outranks the kill switch: nothing is written", mgAnn: map[string]string{sw: "manual"},
			gate: func(g *failedInstallGate) { g.updateWithheld = true }, want: planReport},
		{name: "once live under the kill switch: today's upgrade, no mark", mgAnn: map[string]string{sw: "manual"}, history: failedRevs(4, 6), want: planUpgrade},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mg := ndPortal(tc.mark)
			if tc.mgAnn != nil {
				mg.SetAnnotations(tc.mgAnn)
			}
			g := fiGate(frFakeDyn())
			if tc.gate != nil {
				tc.gate(&g)
			}
			h := tc.history
			if h == nil {
				h = failedRevs(1, 1)
			}
			if tc.record == nil {
				withInstallRecord(mg, h[len(h)-1])
			} else if len(tc.record) > 0 {
				_ = unstructured.SetNestedStringMap(mg.Object, tc.record, "status", "helmOperation")
			}
			got := decideFailedInstall(mg, "r", g, h[len(h)-1], h)
			if got.plan != tc.want || !strings.Contains(got.why, tc.wantWhy) {
				t.Errorf("plan = %s (%s), want %s (%s)", got.plan, got.why, tc.want, tc.wantWhy)
			}
			if wantAttempts := tc.want != planUpgrade; (len(got.attempts) == len(h)) != wantAttempts {
				t.Errorf("attempts = %d of %d, want them recorded: %v", len(got.attempts), len(h), wantAttempts)
			}
		})
	}
}

// The gate is read from the composition, the definition and this controller's own version.
func TestNewFailedInstallGate(t *testing.T) {
	info := func(version string, ann map[string]string) *archive.Info {
		return &archive.Info{Version: version, DefinitionVersion: "0.2.0", CompositionDefinitionInfo: &archive.CompositionDefinitionInfo{Annotations: ann}}
	}
	g := newFailedInstallGate(ndPortal(""), info("0.2.0", map[string]string{}), frFakeDyn())
	if g.retiring || !g.createAllowed || !g.cdAnnotationsKnown || g.updateWithheld {
		t.Errorf("control: the default composition's gate = %+v", g)
	}
	if g := newFailedInstallGate(ndPortal(""), info("0.1.0", map[string]string{}), nil); !g.retiring {
		t.Error("a controller rendering another chart version than the definition's is retiring")
	}
	if g := newFailedInstallGate(ndPortal(""), info("0.2.0", nil), nil); g.cdAnnotationsKnown {
		t.Error("nil annotations mean the definition was not read, never \"no kill switch\"")
	}
	mg := ndPortal("")
	mg.SetAnnotations(map[string]string{meta.AnnotationKeyManagementPolicy: "observe,update,delete"})
	if g := newFailedInstallGate(mg, info("0.2.0", map[string]string{}), nil); g.createAllowed {
		t.Error("a policy without create must not allow installing again")
	}
}

// historyClient counts the history reads; any other helm call panics.
type historyClient struct {
	helmconfig.Client
	history []*helmconfig.Release
	err     error
	reads   int
}

func (c *historyClient) History(context.Context, string, *helmconfig.HistoryConfig) ([]*helmconfig.Release, error) {
	c.reads++
	return c.history, c.err
}

// What assessFailedInstall leaves on the STORED composition before helm runs, for every plan, and that a
// mark it cannot write stops the pass.
func TestAssessFailedInstall(t *testing.T) {
	sw := compositionMeta.AnnotationKeyInterruptedInstallRecovery
	for _, tc := range []struct {
		name           string
		rel            *helmconfig.Release
		history        []*helmconfig.Release
		historyErr     error
		ann            map[string]string
		gate           func(*failedInstallGate)
		denyPatch      string // an annotation key whose patch is refused
		wantPlan       failedInstallPlan
		wantNever      int
		wantUnverified bool
		wantErr        string
		wantEvent      event.Reason
		wantReads      int
	}{
		{name: "deployed: nothing read, nothing written", rel: rev(1, helmconfig.StatusDeployed)},
		{name: "no release: nothing read", rel: nil},
		{name: "failed, never deployed: marked, then installed again", rel: rev(1, helmconfig.StatusFailed), history: failedRevs(1, 1),
			wantPlan: planInstallAgain, wantNever: 1, wantReads: 1},
		{name: "failed, once live: nothing marked", rel: rev(6, helmconfig.StatusFailed), history: failedRevs(4, 6), wantPlan: planUpgrade, wantReads: 1},
		{name: "kill switch: BOTH marks before the upgrade", rel: rev(2, helmconfig.StatusFailed), history: failedRevs(1, 2), ann: map[string]string{sw: "manual"},
			wantPlan: planUpgradeUnverified, wantNever: 2, wantUnverified: true, wantEvent: reasonHooksUnverified, wantReads: 1},
		{name: "kill switch, already marked: no second event", rel: rev(2, helmconfig.StatusFailed), history: failedRevs(1, 2),
			ann:      map[string]string{sw: "manual", compositionMeta.AnnotationKeyHooksUnverified: hooksMark},
			wantPlan: planUpgradeUnverified, wantNever: 2, wantUnverified: true, wantReads: 1},
		{name: "create withheld: BOTH marks", rel: rev(1, helmconfig.StatusFailed), history: failedRevs(1, 1),
			gate:     func(g *failedInstallGate) { g.createAllowed = false },
			wantPlan: planUpgradeUnverified, wantNever: 1, wantUnverified: true, wantEvent: reasonHooksUnverified, wantReads: 1},
		{name: "update withheld: nothing written", rel: rev(1, helmconfig.StatusFailed), history: failedRevs(1, 1),
			gate: func(g *failedInstallGate) { g.updateWithheld = true }, wantPlan: planReport, wantReads: 1},
		{name: "retiring: nothing written, and said", rel: rev(1, helmconfig.StatusFailed), history: failedRevs(1, 1),
			gate: func(g *failedInstallGate) { g.retiring = true }, wantPlan: planLeave, wantEvent: reasonRetiringLeftInstall, wantReads: 1},
		{name: "the never-deployed mark cannot be written: stop", rel: rev(1, helmconfig.StatusFailed), history: failedRevs(1, 1),
			denyPatch: compositionMeta.AnnotationKeyReleaseNeverDeployed, wantPlan: planInstallAgain, wantErr: "marking it never deployed first", wantReads: 1},
		{name: "the never-deployed mark cannot be written under the kill switch: stop", rel: rev(1, helmconfig.StatusFailed), history: failedRevs(1, 1),
			ann: map[string]string{sw: "manual"}, denyPatch: compositionMeta.AnnotationKeyReleaseNeverDeployed, wantPlan: planUpgradeUnverified,
			wantErr: "marking it never deployed first", wantUnverified: true, wantEvent: reasonHooksUnverified, wantReads: 1},
		{name: "hooks-unverified cannot be written: stop", rel: rev(1, helmconfig.StatusFailed), history: failedRevs(1, 1),
			ann: map[string]string{sw: "manual"}, denyPatch: compositionMeta.AnnotationKeyHooksUnverified, wantPlan: planUpgradeUnverified,
			wantErr: "marking its hooks unverified first", wantReads: 1},
		{name: "the history cannot be read: stop", rel: rev(1, helmconfig.StatusFailed), historyErr: errors.New("secrets is forbidden"), wantErr: "reading its history", wantReads: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			portal := ndPortal("")
			if tc.ann != nil {
				portal.SetAnnotations(tc.ann)
			}
			if tc.rel != nil {
				withInstallRecord(portal, tc.rel)
			}
			dyn := frFakeDyn(portal)
			if tc.denyPatch != "" {
				dyn.PrependReactor("patch", "portals", func(a clienttesting.Action) (bool, runtime.Object, error) {
					if strings.Contains(string(a.(clienttesting.PatchAction).GetPatch()), tc.denyPatch) {
						return true, nil, errors.New("denied by a policy")
					}
					return false, nil, nil
				})
			}
			g := fiGate(dyn)
			if tc.gate != nil {
				tc.gate(&g)
			}
			hc := &historyClient{history: tc.history, err: tc.historyErr}
			rec := events.NewFakeRecorder(10)
			h := &handler{eventRecorder: *event.NewAPIRecorder(rec)}
			mg, fi, err := h.assessFailedInstall(context.Background(), portal.DeepCopy(), hc, tc.rel, "r", g, srOpts(dyn), logging.NewNopLogger(), "Observe")
			if (tc.wantErr == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
			if fi.plan != tc.wantPlan || hc.reads != tc.wantReads {
				t.Errorf("plan %s, %d history reads; want %s, %d", fi.plan, hc.reads, tc.wantPlan, tc.wantReads)
			}
			stored, gerr := dyn.Resource(srGVR).Namespace("krateo-system").Get(context.Background(), "portal", metav1.GetOptions{})
			if gerr != nil {
				t.Fatal(gerr)
			}
			if n, _ := readNeverDeployedMark(stored, "r"); n != tc.wantNever {
				t.Errorf("stored never-deployed mark = %d, want %d", n, tc.wantNever)
			}
			if _, marked := hooksUnverifiedSince(stored); marked != tc.wantUnverified {
				t.Errorf("stored hooks-unverified = %v, want %v", marked, tc.wantUnverified)
			}
			if tc.wantErr == "" {
				if _, marked := hooksUnverifiedSince(mg); marked != tc.wantUnverified {
					t.Errorf("returned object's hooks-unverified = %v, want %v: this pass's Ready reads it", marked, tc.wantUnverified)
				}
			}
			var evs []string
			for len(rec.Events) > 0 {
				evs = append(evs, <-rec.Events)
			}
			if (tc.wantEvent == "") != (len(evs) == 0) || !strings.Contains(strings.Join(evs, "|"), string(tc.wantEvent)) {
				t.Errorf("events %v, want %q", evs, tc.wantEvent)
			}
		})
	}
}

// attempt is a failed revision whose record names hook Jobs.
func attempt(n int, jobs ...string) *helmconfig.Release {
	r := rev(n, helmconfig.StatusFailed)
	r.Namespace = "krateo-system"
	for _, j := range jobs {
		r.Hooks = append(r.Hooks, helmconfig.Hook{Name: j, Kind: "Job", Manifest: "apiVersion: batch/v1\nkind: Job\nmetadata:\n  name: " + j + "\n"})
	}
	return r
}

// denyAnnotationPatch refuses every annotation patch of the composition.
func denyAnnotationPatch(dyn *dynamicfake.FakeDynamicClient) {
	dyn.PrependReactor("patch", "portals", func(clienttesting.Action) (bool, runtime.Object, error) { return true, nil, errors.New("denied") })
}

// failNthStatusWrite fails the nth status write of the composition: 1 is the durable recovery record, 2
// the operation record.
func failNthStatusWrite(n int, err error) func(*dynamicfake.FakeDynamicClient) {
	return func(d *dynamicfake.FakeDynamicClient) {
		seen := 0
		d.PrependReactor("update", "portals", func(a clienttesting.Action) (bool, runtime.Object, error) {
			if a.(clienttesting.UpdateAction).GetSubresource() != "status" {
				return false, nil, nil
			}
			seen++
			if seen == n {
				return true, nil, err
			}
			return false, nil, nil
		})
	}
}

// What must hold before helm writes the retry's revision: the attempts' hook Jobs are gone (those of
// EVERY stored attempt, and no other Job), the composition is not being deleted, and the operation is
// recorded as this pod's, so the controller that replaces a dead one finds it orphaned.
func TestBeforeInstallAgain(t *testing.T) {
	podName, podNamespace = "me", "ns"
	defer func() { podName, podNamespace = "", "" }()
	oldTimeout, oldPoll := hookJobsGoneTimeout, hookJobsGonePoll
	hookJobsGoneTimeout, hookJobsGonePoll = 300*time.Millisecond, 10*time.Millisecond
	defer func() { hookJobsGoneTimeout, hookJobsGonePoll = oldTimeout, oldPoll }()
	for _, tc := range []struct {
		name         string
		react        func(*dynamicfake.FakeDynamicClient)
		notInAPI     bool
		noRecovery   bool // the durable record could not be written
		interrupted  bool // settling an interrupted install already counted and recorded this retry
		wantErr      string
		wantErrIs    error
		wantJobsLeft string
	}{
		{name: "ready: the Jobs of both attempts gone, the unrelated one kept, recorded", wantJobsLeft: "unrelated"},
		{name: "an interrupted install, already recorded by its settling: not recorded twice", interrupted: true, wantJobsLeft: "unrelated"},
		{name: "a Job cannot be deleted: stop", wantErr: "could not be deleted", wantJobsLeft: "db-init,old-hook,unrelated",
			react: func(d *dynamicfake.FakeDynamicClient) {
				d.PrependReactor("delete", "jobs", func(clienttesting.Action) (bool, runtime.Object, error) {
					return true, nil, errors.New("jobs is forbidden")
				})
			}},
		{name: "a Job that was deleted is still there (its pod has not stopped): stop", wantErr: "still exists", wantJobsLeft: "db-init,old-hook,unrelated",
			react: func(d *dynamicfake.FakeDynamicClient) {
				d.PrependReactor("delete", "jobs", func(clienttesting.Action) (bool, runtime.Object, error) { return true, nil, nil })
			}},
		{name: "a Job that cannot be confirmed gone: stop", wantErr: "confirming hook Job", wantJobsLeft: "unrelated",
			react: func(d *dynamicfake.FakeDynamicClient) {
				d.PrependReactor("get", "jobs", func(a clienttesting.Action) (bool, runtime.Object, error) {
					if a.(clienttesting.GetAction).GetName() == "db-init" {
						return true, nil, errors.New("apiserver unavailable")
					}
					return false, nil, nil
				})
			}},
		{name: "being deleted: stop, nothing recorded", wantErrIs: errInstallWhileDeleting, wantJobsLeft: "unrelated",
			// Through the read alone: the fake's status update replaces the whole object, deletionTimestamp included.
			react: func(d *dynamicfake.FakeDynamicClient) {
				d.PrependReactor("get", "portals", func(clienttesting.Action) (bool, runtime.Object, error) {
					u := ndPortal("")
					now := metav1.Now()
					u.SetDeletionTimestamp(&now)
					return true, u, nil
				})
			}},
		{name: "the durable record cannot be written: stop, nothing deleted", noRecovery: true, wantErr: "recording the install recovery", wantJobsLeft: "db-init,old-hook,unrelated",
			react: failNthStatusWrite(1, errors.New("denied"))},
		{name: "the durable record loses a status race: stop, nothing deleted", noRecovery: true, wantErrIs: tools.ErrStatusConflict, wantJobsLeft: "db-init,old-hook,unrelated",
			react: failNthStatusWrite(1, apierrors.NewConflict(schema.GroupResource{Group: srGVR.Group, Resource: srGVR.Resource}, "portal", nil))},
		{name: "the composition is gone: stop, nothing deleted", notInAPI: true, wantErr: "recording the install recovery", wantJobsLeft: "db-init,old-hook,unrelated"},
		{name: "the deletion cannot be checked: stop", wantErr: "checking the composition is not being deleted", wantJobsLeft: "unrelated",
			react: func(d *dynamicfake.FakeDynamicClient) {
				d.PrependReactor("get", "portals", func(clienttesting.Action) (bool, runtime.Object, error) {
					return true, nil, errors.New("apiserver unavailable")
				})
			}},
		{name: "the operation record loses a status race: stop", wantErrIs: tools.ErrStatusConflict, wantJobsLeft: "unrelated",
			react: failNthStatusWrite(2, apierrors.NewConflict(schema.GroupResource{Group: srGVR.Group, Resource: srGVR.Resource}, "portal", nil))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			stored := ndPortal("")
			objs := []runtime.Object{frJob("db-init"), frJob("old-hook"), frJob("unrelated")}
			if !tc.notInAPI {
				objs = append(objs, stored)
			}
			dyn := frFakeDyn(objs...)
			rec := events.NewFakeRecorder(10)
			h := &handler{eventRecorder: *event.NewAPIRecorder(rec)}
			fi := failedInstall{plan: planInstallAgain, revision: 2, interrupted: tc.interrupted, hookJobsDeleted: map[bool]int{true: 1}[tc.interrupted],
				attempts: []*helmconfig.Release{attempt(1, "db-init", "old-hook"), attempt(2, "db-init", "ks-endpoints")}}
			if tc.react != nil {
				tc.react(dyn)
			}
			start := time.Now()
			_, _, _, err := h.beforeInstallAgain(ctx, ndPortal(""), fi, "r", fiGate(dyn), srOpts(dyn), logging.NewNopLogger(), "Observe")
			ok := tc.wantErr == "" && tc.wantErrIs == nil
			if ok != (err == nil) || (tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr)) || (tc.wantErrIs != nil && !errors.Is(err, tc.wantErrIs)) {
				t.Fatalf("err = %v, want %q / %v", err, tc.wantErr, tc.wantErrIs)
			}
			var left []string
			l, lerr := dyn.Tracker().List(jobsGVR, schema.GroupVersionKind{Group: "batch", Version: "v1", Kind: "Job"}, "krateo-system")
			if lerr != nil {
				t.Fatal(lerr)
			}
			for _, j := range l.(*unstructured.UnstructuredList).Items {
				left = append(left, j.GetName())
			}
			if strings.Join(left, ",") != tc.wantJobsLeft {
				t.Errorf("Jobs left = %v, want %s", left, tc.wantJobsLeft)
			}
			deletes := 0
			for _, a := range dyn.Actions() {
				if d, isDelete := a.(clienttesting.DeleteActionImpl); isDelete && a.GetResource().Resource == "jobs" {
					deletes++
					if p := d.GetDeleteOptions().PropagationPolicy; p == nil || *p != metav1.DeletePropagationForeground {
						t.Errorf("Job %s deleted with propagation %v, want Foreground: with any other, the Job is gone before its pods are", d.GetName(), p)
					}
				}
			}
			if want := map[bool]int{false: 3, true: 0}[tc.notInAPI || tc.noRecovery]; deletes != want {
				t.Errorf("%d Job deletes, want %d (each recorded Job once, none before the durable record)", deletes, want)
			}
			if !tc.notInAPI && !tc.noRecovery {
				got, gerr := dyn.Tracker().Get(srGVR, "krateo-system", "portal")
				if gerr != nil {
					t.Fatal(gerr)
				}
				r, _, _ := unstructured.NestedMap(got.(*unstructured.Unstructured).Object, "status", "installRecovery")
				if tc.interrupted {
					if r != nil {
						t.Errorf("recorded twice: %v", r)
					}
				} else if r["kind"] != installRecoveryRetried || r["reason"] != installRecoveryFailed || r["count"] != int64(1) || r["lastAt"] == "" || !strings.Contains(r["message"].(string), "revision 2") {
					t.Errorf("stored status.installRecovery = %v", r)
				}
			}
			// The STORED record: an in-memory one left by a lost race reaches nobody.
			var rec1 map[string]string
			recorded := false
			if !tc.notInAPI {
				got, gerr := dyn.Tracker().Get(srGVR, "krateo-system", "portal")
				if gerr != nil {
					t.Fatal(gerr)
				}
				rec1, recorded, _ = unstructured.NestedStringMap(got.(*unstructured.Unstructured).Object, "status", "helmOperation")
			}
			if recorded != ok {
				t.Fatalf("operation recorded = %v, want %v", recorded, ok)
			}
			if !ok {
				if len(rec.Events) != 0 {
					t.Errorf("an event for a retry that did not start: %s", <-rec.Events)
				}
				return
			}
			if rec1["phase"] != "install" || rec1["startedBy"] != "ns/me" {
				t.Errorf("record = %v, want an install started by ns/me", rec1)
			}
			wantEvent := reasonFailedInstallRetried
			// db-init and old-hook exist, ks-endpoints never did; settling had deleted 1 more.
			wantCount := "deleting 2 of the 3 hook Job(s)"
			if tc.interrupted {
				wantEvent, wantCount = reasonInterruptedInstallRetried, "deleting 3 of the 3 hook Job(s)"
			}
			if e := <-rec.Events; !strings.Contains(e, string(wantEvent)) || !strings.Contains(e, wantCount) {
				t.Errorf("event %q, want a %s saying %q", e, wantEvent, wantCount)
			}
			// The controller dies in it: the next one finds the record on the STORED object, and the pod gone.
			got, gerr := dyn.Resource(srGVR).Namespace("krateo-system").Get(ctx, "portal", metav1.GetOptions{})
			if gerr != nil {
				t.Fatal(gerr)
			}
			inFlight := rev(3, helmconfig.StatusPendingInstall)
			inFlight.Updated = start.Add(time.Second)
			podName = "another"
			orphan, why := orphanedOperation(ctx, got, inFlight, goneLookup("ns/me"))
			podName = "me"
			if !orphan {
				t.Errorf("a retry whose controller died is not found orphaned: %s", why)
			}
		})
	}
}

// deleteHookJobsAndWait returns once the Jobs are gone, and not before: the Job here disappears only
// after its (simulated) pods do.
func TestDeleteHookJobsAndWait(t *testing.T) {
	oldPoll := hookJobsGonePoll
	hookJobsGonePoll = 10 * time.Millisecond
	defer func() { hookJobsGonePoll = oldPoll }()
	dyn := frFakeDyn(frJob("db-init"))
	dyn.PrependReactor("delete", "jobs", func(clienttesting.Action) (bool, runtime.Object, error) { return true, nil, nil })
	refs := []hookJobRef{{Namespace: "krateo-system", Name: "db-init"}}
	if _, err := deleteHookJobsAndWait(context.Background(), dyn, refs, 50*time.Millisecond); err == nil {
		t.Fatal("control: a Job whose pods are still there reads as gone")
	}
	gone := make(chan time.Time, 1)
	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = dyn.Tracker().Delete(jobsGVR, "krateo-system", "db-init")
		gone <- time.Now()
	}()
	if _, err := deleteHookJobsAndWait(context.Background(), dyn, refs, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if returned := time.Now(); returned.Before(<-gone) {
		t.Error("returned before the Job was gone")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if n, err := deleteHookJobsAndWait(ctx, frFakeDyn(frJob("db-init"), frJob("x")), nil, time.Second); err != nil || n != 0 {
		t.Errorf("no Jobs: %d %v", n, err)
	}
}

// The retry's history is never bounded to 1: helm would prune the failed revision it merges against.
func TestRetryMaxHistory(t *testing.T) {
	for configured, want := range map[int]int{0: 0, 1: 2, 2: 2, 3: 3, 10: 10} {
		if got := retryMaxHistory(configured); got != want {
			t.Errorf("HELM_MAX_HISTORY=%d: the retry keeps %d revisions, want %d", configured, got, want)
		}
	}
	old := helmMaxHistory
	defer func() { helmMaxHistory = old }()
	helmMaxHistory = 1
	got := -1
	setRetry(t, func(_ context.Context, _ helmconfig.Client, _, _ string, _ *helmconfig.ActionConfig, maxHistory int) (*helmconfig.OperationResult, error) {
		got = maxHistory
		return &helmconfig.OperationResult{Release: rev(2, helmconfig.StatusDeployed)}, nil
	})
	dyn := frFakeDyn(ndPortal(""))
	h := &handler{eventRecorder: *event.NewAPIRecorder(events.NewFakeRecorder(10))}
	if _, _, err := h.installAgain(context.Background(), ndPortal(""), nil, failedInstall{revision: 1}, "r", "u", &helmconfig.ActionConfig{}, fiGate(dyn), srOpts(dyn), logging.NewNopLogger(), "Observe"); err != nil || got != 2 {
		t.Errorf("with HELM_MAX_HISTORY=1 the call got %d (err %v), want 2", got, err)
	}
}

// ownFailureClient serves the release a failed install call left.
type ownFailureClient struct {
	helmconfig.Client
	rel        *helmconfig.Release
	relErr     error
	history    []*helmconfig.Release
	historyErr error
}

func (c *ownFailureClient) GetRelease(context.Context, string, *helmconfig.GetConfig) (*helmconfig.Release, error) {
	return c.rel, c.relErr
}

func (c *ownFailureClient) History(context.Context, string, *helmconfig.HistoryConfig) ([]*helmconfig.Release, error) {
	return c.history, c.historyErr
}

// The mark after this controller's own install call failed: only for the revision that call wrote.
func TestMarkOwnFailedInstall(t *testing.T) {
	started := time.Date(2026, 11, 23, 21, 0, 0, 0, time.UTC)
	at := func(n int, s helmconfig.Status, updated time.Time) *helmconfig.Release {
		r := rev(n, s)
		r.Updated = updated
		return r
	}
	mine := at(1, helmconfig.StatusFailed, started.Add(time.Second))
	hist := []*helmconfig.Release{mine}
	failure := errors.New("install failed: failed post-install")
	for _, tc := range []struct {
		name    string
		hc      *ownFailureClient
		callErr error
		deny    bool
		want    int
	}{
		{name: "this call's failed revision", hc: &ownFailureClient{rel: mine, history: hist}, callErr: failure, want: 1},
		{name: "refused, the name is in use: the release is somebody else's", hc: &ownFailureClient{rel: mine, history: hist}, callErr: errors.New("install failed: cannot re-use a name that is still in use")},
		{name: "another operation holds the release", hc: &ownFailureClient{rel: mine, history: hist}, callErr: fmt.Errorf("install failed: %w", helmconfig.ErrOperationInProgress)},
		{name: "refused by the retry", hc: &ownFailureClient{rel: mine, history: hist}, callErr: errRetryRefused},
		{name: "no error", hc: &ownFailureClient{rel: mine, history: hist}},
		{name: "a revision written before this call", hc: &ownFailureClient{rel: at(1, helmconfig.StatusFailed, started.Add(-time.Second)), history: hist}, callErr: failure},
		{name: "not a failed revision", hc: &ownFailureClient{rel: at(1, helmconfig.StatusPendingInstall, started.Add(time.Second)), history: hist}, callErr: failure},
		{name: "a release that was live", hc: &ownFailureClient{rel: at(2, helmconfig.StatusFailed, started.Add(time.Second)),
			history: []*helmconfig.Release{rev(1, helmconfig.StatusDeployed), at(2, helmconfig.StatusFailed, started.Add(time.Second))}}, callErr: failure},
		{name: "it failed before helm stored anything", hc: &ownFailureClient{}, callErr: failure},
		{name: "the release cannot be read", hc: &ownFailureClient{relErr: errors.New("x")}, callErr: failure},
		{name: "the history cannot be read", hc: &ownFailureClient{rel: mine, historyErr: errors.New("x")}, callErr: failure},
		{name: "the mark cannot be written: the object is returned as it was", hc: &ownFailureClient{rel: mine, history: hist}, callErr: failure, deny: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dyn := frFakeDyn(ndPortal(""))
			if tc.deny {
				dyn.PrependReactor("patch", "portals", func(clienttesting.Action) (bool, runtime.Object, error) { return true, nil, errors.New("denied") })
			}
			out := markOwnFailedInstall(context.Background(), ndPortal(""), tc.hc, "r", started, tc.callErr, srOpts(dyn), logging.NewNopLogger())
			if out == nil {
				t.Fatal("no object to carry on with")
			}
			stored, err := dyn.Resource(srGVR).Namespace("krateo-system").Get(context.Background(), "portal", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if n, _ := readNeverDeployedMark(stored, "r"); n != tc.want {
				t.Errorf("stored mark names revision %d, want %d", n, tc.want)
			}
		})
	}
}

// What is said to be deleted is what was deleted, never a name on a list (measured by openstack on
// 2.13.128: "deleted 7 hook Job(s)" where two existed). Control: all of them exist.
func TestDeletedHookJobsAreCountedByTheDelete(t *testing.T) {
	refs := []hookJobRef{{"krateo-system", "a"}, {"krateo-system", "b"}, {"krateo-system", "c"}}
	for _, tc := range []struct {
		name   string
		exist  []runtime.Object
		refuse string
		want   int
		left   int
	}{
		{"control: all exist", []runtime.Object{frJob("a"), frJob("b"), frJob("c")}, "", 3, 0},
		{"two absent", []runtime.Object{frJob("b")}, "", 1, 0},
		{"none exist", nil, "", 0, 0},
		{"one refused, one absent", []runtime.Object{frJob("a"), frJob("b")}, "b", 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := func() *dynamicfake.FakeDynamicClient {
				dyn := frFakeDyn(tc.exist...)
				dyn.PrependReactor("delete", "jobs", func(a clienttesting.Action) (bool, runtime.Object, error) {
					if a.(clienttesting.DeleteAction).GetName() == tc.refuse {
						return true, nil, errors.New("forbidden")
					}
					return false, nil, nil
				})
				return dyn
			}
			if n, left, err := deleteHookJobs(context.Background(), client(), refs); n != tc.want || len(left) != tc.left || (err != nil) != (tc.left > 0) {
				t.Errorf("deleteHookJobs: %d deleted, %d left, %v; want %d, %d", n, len(left), err, tc.want, tc.left)
			}
			if n, err := deleteHookJobsAndWait(context.Background(), client(), refs, time.Second); n != tc.want || (err != nil) != (tc.left > 0) {
				t.Errorf("deleteHookJobsAndWait: %d deleted, %v; want %d", n, err, tc.want)
			}
			if deleted, recorded, err := deleteAttemptsHookJobs(context.Background(), client(), []*helmconfig.Release{attempt(1, "a", "b"), attempt(2, "b", "c")}); deleted != tc.want || (tc.left == 0 && (recorded != 3 || err != nil)) {
				t.Errorf("deleteAttemptsHookJobs: %d of %d, %v; want %d of 3", deleted, recorded, err, tc.want)
			}
		})
	}
}

// setRetry replaces plumbing's retry with f.
func setRetry(t *testing.T, f func(context.Context, helmconfig.Client, string, string, *helmconfig.ActionConfig, int) (*helmconfig.OperationResult, error)) {
	t.Helper()
	setRetrier(t, &fakeRetrier{retry: f})
}

// installAgain: bounded history, the marks removed only by an install that was deployed, the record of a
// refused call put back.
func TestInstallAgain(t *testing.T) {
	podName, podNamespace = "me", "ns"
	defer func() { podName, podNamespace = "", "" }()
	oldPoll := hookJobsGonePoll
	hookJobsGonePoll = 10 * time.Millisecond
	defer func() { hookJobsGonePoll = oldPoll }()
	ownFailure := rev(2, helmconfig.StatusFailed)
	ownFailure.Updated = time.Now().Add(time.Hour)
	for _, tc := range []struct {
		name         string
		wantMark     int // the never-deployed mark's revision
		op           *helmconfig.OperationResult
		err          error
		wantErr      string
		wantMarks    bool // both marks still there
		wantRecorder string
	}{
		{name: "installed: both marks removed", op: &helmconfig.OperationResult{Release: rev(2, helmconfig.StatusDeployed)}, wantRecorder: "ns/me"},
		{name: "the install fails: both marks kept, the never-deployed one naming the new attempt", err: errors.New("failed post-install"), wantErr: "failed post-install", wantMarks: true, wantMark: 2, wantRecorder: ""},
		{name: "a release that is not deployed is not an install", op: &helmconfig.OperationResult{Release: rev(2, helmconfig.StatusFailed)},
			wantErr: "without a deployed release", wantMarks: true, wantMark: 2, wantRecorder: ""},
		{name: "no release is not an install", op: &helmconfig.OperationResult{}, wantErr: "without a deployed release", wantMarks: true, wantMark: 2, wantRecorder: ""},
		{name: "refused: the record it replaced is put back", err: errRetryRefused, wantErr: errRetryRefused.Error(), wantMarks: true, wantRecorder: "ns/holder"},
		{name: "another operation holds the release: the record it replaced is put back", err: fmt.Errorf("install failed: %w", helmconfig.ErrOperationInProgress),
			wantErr: "install failed", wantMarks: true, wantRecorder: "ns/holder"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			portal := ndPortal("")
			portal.SetAnnotations(map[string]string{compositionMeta.AnnotationKeyReleaseNeverDeployed: ndMark(ndUID, "r", 1), compositionMeta.AnnotationKeyHooksUnverified: hooksMark,
				compositionMeta.AnnotationKeyInterruptedInstallReinstalls: "1"})
			_ = unstructured.SetNestedStringMap(portal.Object, map[string]string{"phase": "install", "startedBy": "ns/holder"}, "status", "helmOperation")
			dyn := frFakeDyn(portal, frJob("db-init"))
			jobThere := func() bool {
				_, err := dyn.Resource(jobsGVR).Namespace("krateo-system").Get(ctx, "db-init", metav1.GetOptions{})
				return err == nil
			}
			ac := &helmconfig.ActionConfig{TakeOwnership: true}
			calls := 0
			setRetry(t, func(_ context.Context, _ helmconfig.Client, name, url string, got *helmconfig.ActionConfig, maxHistory int) (*helmconfig.OperationResult, error) {
				calls++
				if name != "r" || url != "http://chart" || got != ac || maxHistory != helmMaxHistory || helmMaxHistory != 3 || jobThere() {
					t.Errorf("install of %s from %s, config %+v, max history %d, hook Job there %v", name, url, got, maxHistory, jobThere())
				}
				return tc.op, tc.err
			})
			h := &handler{eventRecorder: *event.NewAPIRecorder(events.NewFakeRecorder(10))}
			fi := failedInstall{plan: planInstallAgain, revision: 1, attempts: []*helmconfig.Release{attempt(1, "db-init")}}
			hc := &ownFailureClient{rel: ownFailure, history: []*helmconfig.Release{rev(1, helmconfig.StatusFailed), ownFailure}}
			_, rel, err := h.installAgain(ctx, portal.DeepCopy(), hc, fi, "r", "http://chart", ac, fiGate(dyn), srOpts(dyn), logging.NewNopLogger(), "Observe")
			if (tc.wantErr == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
			if err == nil && rel.Revision != 2 {
				t.Errorf("release = %+v", rel)
			}
			if calls != 1 {
				t.Errorf("%d installs, want one", calls)
			}
			if jobThere() {
				t.Error("the failed attempt's hook Job is still there")
			}
			// The counter goes with the other marks, in the pass that installs, not a pass after Ready says so.
			if _, counted := storedPortal(t, dyn).GetAnnotations()[compositionMeta.AnnotationKeyInterruptedInstallReinstalls]; counted != tc.wantMarks {
				t.Errorf("the attempt counter is there: %v, want %v", counted, tc.wantMarks)
			}
			stored, gerr := dyn.Resource(srGVR).Namespace("krateo-system").Get(ctx, "portal", metav1.GetOptions{})
			if gerr != nil {
				t.Fatal(gerr)
			}
			_, never := stored.GetAnnotations()[compositionMeta.AnnotationKeyReleaseNeverDeployed]
			_, unverified := hooksUnverifiedSince(stored)
			if never != tc.wantMarks || unverified != tc.wantMarks {
				t.Errorf("never-deployed kept=%v hooks-unverified kept=%v, want both %v", never, unverified, tc.wantMarks)
			}
			if want := max(tc.wantMark, 1); tc.wantMarks {
				if n, _ := readNeverDeployedMark(stored, "r"); n != want {
					t.Errorf("never-deployed mark names revision %d, want %d", n, want)
				}
			}
			if by, _, _ := unstructured.NestedString(stored.Object, "status", "helmOperation", "startedBy"); by != tc.wantRecorder {
				t.Errorf("stored record startedBy %q, want %q", by, tc.wantRecorder)
			}
		})
	}
}

// Under a withheld update nothing is written to the release, so Ready itself must say that the failed
// first install's hooks may not have run. Control: any other plan leaves the verdict alone.
func TestWithInstallHooksNotRun(t *testing.T) {
	healthy := readyOutcome{reason: "Available", message: "ok"}
	for p := planUpgrade; p <= planLeave; p++ {
		msg := installHooksNotRunMessage(failedInstall{plan: p, revision: 1, why: "the management policy withholds update"})
		got := withUpdateWithheld(withInstallHooksNotRun(healthy, msg), "updates are withheld")
		if p != planReport {
			if got.reason != conditionreason.ReasonUpdateWithheldByPolicy {
				t.Errorf("control: plan %s changed the verdict to %s", p, got.reason)
			}
			continue
		}
		for _, want := range []string{"install hooks may not have run", "withholds update", "Children: Available: ok", "updates are withheld"} {
			if got.reason != conditionreason.ReasonHooksMayNotHaveRun || !strings.Contains(got.message, want) {
				t.Errorf("plan %s: %s %q, want HooksMayNotHaveRun with %q", p, got.reason, got.message, want)
			}
		}
	}
	marked := withHooksUnverified(healthy, markedPortal(t, true))
	if got := withInstallHooksNotRun(marked, "x"); got != marked {
		t.Errorf("the mark's own message was replaced: %+v", got)
	}
}

// A composition being deleted is never reported absent: the runtime would queue an install for it, and
// any consumer of the observation would read the failed release as gone. The Delete handler uninstalls it.
func TestObserve_DeletingCompositionIsNotReportedAbsent(t *testing.T) {
	mg := ndPortal(ndMark(ndUID, "r", 1))
	now := metav1.Now()
	mg.SetDeletionTimestamp(&now)
	obs, err := (&handler{}).Observe(context.Background(), mg)
	if err != nil || !obs.ResourceExists || !obs.ResourceUpToDate {
		t.Fatalf("observation %+v err %v, want exists and up to date", obs, err)
	}
}

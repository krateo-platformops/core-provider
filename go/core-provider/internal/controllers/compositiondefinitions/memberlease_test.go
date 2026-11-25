package compositiondefinitions

import (
	"context"
	"strings"
	"testing"
	"time"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/cdreason"
	"github.com/krateo-platformops/core-provider/internal/tools/deploy"
	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlevent "sigs.k8s.io/controller-runtime/pkg/event"
)

var member653 = schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1-0-0", Resource: "pages"}

func lease653(holder string, renewed time.Time, secs int32) *coordinationv1.Lease {
	r := metav1.NewMicroTime(renewed)
	return &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: "v1-0-0.pages.composition.krateo.io", Namespace: "krateo-system", ResourceVersion: renewed.String()},
		Spec: coordinationv1.LeaseSpec{HolderIdentity: ptr.To(holder), LeaseDurationSeconds: ptr.To(secs), RenewTime: &r}}
}

// The verdict, by input state. Freshness is on this process's clock; the renewTime counts only for a
// record from the first list, for its first duration.
func TestLeaseVerdict(t *testing.T) {
	t0 := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	pod := "cdc-pool-web-7d9f-abcde"
	G := deploy.LeaseTimings.MemberGrace()
	for _, c := range []struct {
		name  string
		rec   leaseRecord
		now   time.Time
		kind  memberKind
		since time.Time
	}{
		{"renewed 1s ago by the pool", leaseRecord{lease653(pod, t0, 15), t0, false}, t0.Add(time.Second), memberRunning, time.Time{}},
		{"renewed 14s ago", leaseRecord{lease653(pod, t0, 15), t0, false}, t0.Add(14 * time.Second), memberRunning, time.Time{}},
		{"not renewed for its duration: stale, starting", leaseRecord{lease653(pod, t0, 15), t0, false}, t0.Add(16 * time.Second), memberStarting, t0.Add(15 * time.Second)},
		{"stale for G", leaseRecord{lease653(pod, t0, 15), t0, false}, t0.Add(15*time.Second + G), memberNotRunning, t0.Add(15 * time.Second)},
		{"its own duration decides, not the default", leaseRecord{lease653(pod, t0, 60), t0, false}, t0.Add(30 * time.Second), memberRunning, time.Time{}},
		{"released 5s ago", leaseRecord{lease653("", t0, 15), t0, false}, t0.Add(5 * time.Second), memberStarting, t0},
		{"released G ago", leaseRecord{lease653("", t0, 15), t0, false}, t0.Add(G), memberNotRunning, t0},
		{"held by the dedicated pod", leaseRecord{lease653("pages-v1-0-0-controller-x-y", t0, 15), t0, false}, t0.Add(time.Second), memberTakingOver, time.Time{}},
		{"held by another pool", leaseRecord{lease653("cdc-pool-webby-x-y", t0, 15), t0, false}, t0.Add(time.Second), memberTakingOver, time.Time{}},
		{"first list, renewed recently by its clock", leaseRecord{lease653(pod, t0.Add(-5*time.Second), 15), t0, true}, t0.Add(time.Second), memberRunning, time.Time{}},
		{"first list, renewed long ago by its clock", leaseRecord{lease653(pod, t0.Add(-time.Hour), 15), t0, true}, t0.Add(time.Second), memberStarting, t0},
		{"first list, no renewal seen in a duration since", leaseRecord{lease653(pod, t0.Add(-time.Second), 15), t0, true}, t0.Add(16 * time.Second), memberStarting, t0.Add(15 * time.Second)},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := leaseVerdict("krateo-system/l", "web", c.rec, c.now)
			if s.kind != c.kind || !s.since.Equal(c.since) {
				t.Errorf("kind %d since %s, want %d since %s", s.kind, s.since, c.kind, c.since)
			}
		})
	}
}

// What each state writes, and that a pool whose Deployment is not available keeps the pool's verdict.
func TestMemberConditions(t *testing.T) {
	ok, running := cdreason.Available.Condition(), cdreason.ControllerRunning.WithMessage("controller running")
	since := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		s             memberState
		ready, active string
		readyMsg, msg string
		known         bool
	}{
		{memberState{kind: memberRunning}, "Available", "Running", "", "controller running in pool web", true},
		{memberState{kind: memberStarting}, "Creating", "Running", "is starting in pool web", "controller running in pool web", true},
		{memberState{kind: memberTakingOver, holder: "pages-v1-0-0-controller-x"}, "Creating", "Running", "is taking over from pages-v1-0-0-controller-x", "", true},
		{memberState{kind: memberNotRunning, lease: "ns/l", since: since}, "Unavailable", "Unavailable",
			"is not running in pool web: its ownership Lease ns/l has not been held since 2026-11-25T12:00:00Z", "has not been held since", true},
		{memberState{kind: memberUnknown, lease: "ns/l"}, "Available", "NotObserved", "", "could not be read", false},
	} {
		r, a, known := memberConditions(ok, running, c.s, "web", member653)
		if string(r.Reason) != c.ready || string(a.Reason) != c.active || known != c.known ||
			!strings.Contains(r.Message, c.readyMsg) || !strings.Contains(a.Message, c.msg) {
			t.Errorf("%d: Ready %s %q, ControllerActive %s %q, known %v", c.s.kind, r.Reason, r.Message, a.Reason, a.Message, known)
		}
	}
	creating := cdreason.Creating.WithMessage("the controller Deployment ns/cdc-pool-web is rolling out")
	if r, _, _ := memberConditions(creating, running, memberState{kind: memberNotRunning}, "web", member653); r.Message != creating.Message {
		t.Errorf("a pool rolling out: Ready = %s %q, want the pool's own verdict", r.Reason, r.Message)
	}
}

// The clock: what wakes a definition, and when a Lease no one has seen yet counts from.
func TestLeaseClock(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	c := newLeaseClock(func() time.Time { return now })
	pod := "cdc-pool-web-a-b"
	l := lease653(pod, now, 15)
	if c.observe(l, true, false) {
		t.Error("the first list woke a definition")
	}
	now = now.Add(2 * time.Second)
	if c.observe(lease653(pod, now, 15), false, false) {
		t.Error("a renewal woke a definition")
	}
	now = now.Add(20 * time.Second)
	if !c.observe(lease653(pod, now, 15), false, false) {
		t.Error("a renewal after a silence of a duration did not wake: a stale member would stay Unavailable")
	}
	if !c.observe(lease653("", now, 15), false, false) {
		t.Error("a release did not wake")
	}
	if !c.observe(lease653("", now, 15), false, true) {
		t.Error("a delete did not wake")
	}
	// Absent: it counts from the first time it was looked for, and stays put.
	first := c.state("web", member653, "krateo-system")
	now = now.Add(deploy.LeaseTimings.MemberGrace())
	if s := c.state("web", member653, "krateo-system"); s.kind != memberNotRunning || !s.since.Equal(first.since) {
		t.Errorf("absent for G: %+v (first %+v)", s, first)
	}
	c.synced = func() bool { return false }
	if s := c.state("web", member653, "krateo-system"); s.kind != memberUnknown {
		t.Errorf("before the informer synced: %d", s.kind)
	}
}

// The wakes map to the right definitions.
func TestLeaseAndPoolDeploymentWake(t *testing.T) {
	ns := withManagementNamespace(t)
	member := poolCD("team-a", "page", 1, "v1-0-0", "web")
	member.Status.Controller = &compositiondefinitionsv1alpha1.ControllerStatus{Mode: compositiondefinitionsv1alpha1.ControllerModePool, Namespace: ns, Pool: "web"}
	follower := poolCD("team-b", "page-b", 2, "v1-0-0", "")
	follower.Status.Controller = member.Status.Controller.DeepCopy()
	other := poolCD("team-c", "other", 3, "v2-0-0", "web")
	other.Status.Controller = member.Status.Controller.DeepCopy()
	dedicated := poolCD("team-d", "dedicated", 4, "v1-0-0", "")
	dedicated.Status.Controller = &compositiondefinitionsv1alpha1.ControllerStatus{Mode: compositiondefinitionsv1alpha1.ControllerModeDedicated, Namespace: ns}
	all := []compositiondefinitionsv1alpha1.CompositionDefinition{member, follower, other, dedicated}
	names := func(reqs []types.NamespacedName) string {
		var out []string
		for _, r := range reqs {
			out = append(out, r.Name)
		}
		return strings.Join(out, ",")
	}
	var got []types.NamespacedName
	for _, r := range definitionsOfLease("v1-0-0.pages.composition.krateo.io", all) {
		got = append(got, r.NamespacedName)
	}
	if names(got) != "page,page-b" {
		t.Errorf("a Lease wakes %s, want page,page-b", names(got))
	}
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "cdc-pool-web", Namespace: ns, Labels: map[string]string{
		deploy.PoolLabel: "web", labelComponent: "controller", labelManagedBy: "krateo"}}}
	got = nil
	for _, r := range definitionsOfController(dep, all) {
		got = append(got, r.NamespacedName)
	}
	if names(got) != "page,page-b,other" {
		t.Errorf("the pool's Deployment wakes %s, want page,page-b,other", names(got))
	}
	if !controllerDeploymentPredicate().Create(ctrlevent.CreateEvent{Object: dep}) {
		t.Error("the pool's Deployment does not pass the predicate")
	}
}

// The poll hook brings a member back at its recheck time, never later than the poll, never in a loop.
func TestMemberPollHook(t *testing.T) {
	r := &memberRequeue{}
	cd := poolCD("team-a", "page", 1, "v1-0-0", "web")
	hook := memberPollHook(r)
	if d := hook(&cd, 3*time.Minute); d != 3*time.Minute {
		t.Errorf("no recheck: %s", d)
	}
	r.set(types.NamespacedName{Namespace: cd.Namespace, Name: cd.Name}, time.Now().Add(20*time.Second))
	if d := hook(&cd, 3*time.Minute); d > 20*time.Second || d < 19*time.Second {
		t.Errorf("recheck in 20s: %s", d)
	}
	r.set(types.NamespacedName{Namespace: cd.Namespace, Name: cd.Name}, time.Now().Add(-time.Minute))
	if d := hook(&cd, 3*time.Minute); d != time.Second {
		t.Errorf("a recheck in the past: %s, want the 1s floor", d)
	}
	r.set(types.NamespacedName{Namespace: cd.Namespace, Name: cd.Name}, time.Time{})
	if d := hook(&cd, 3*time.Minute); d != 3*time.Minute {
		t.Errorf("cleared: %s", d)
	}
}

// A definition's recheck belongs to its pool membership. One that leaves the pool, for a dedicated
// controller or for a remote target (pool and targetRef are exclusive only within one spec, so one
// update can do both), must not keep the recheck it had: a recheck in the past is floored to 1s, so the
// poll hook would bring it back every second for as long as it exists.
func TestMemberRecheck_IsClearedOffThePool(t *testing.T) {
	ns := withManagementNamespace(t)
	poolDep := rolledOut(1)
	poolDep.Name, poolDep.Namespace = deploy.PoolNames("pages").Workload, ns
	ctx := context.Background()
	const poll = 3 * time.Minute
	for _, c := range []struct {
		name  string
		leave func(*compositiondefinitionsv1alpha1.CompositionDefinition)
	}{
		{"for a dedicated controller", func(cd *compositiondefinitionsv1alpha1.CompositionDefinition) { cd.Spec.Controller = nil }},
		{"for a remote target", func(cd *compositiondefinitionsv1alpha1.CompositionDefinition) {
			cd.Spec.Controller = nil
			cd.Spec.Deploy = &compositiondefinitionsv1alpha1.DeploymentTarget{TargetRef: &compositiondefinitionsv1alpha1.TargetReference{Name: "spoke"}}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			now := time.Now()
			e := &external{kube: clientfake.NewClientBuilder().WithObjects(poolDep.DeepCopy()).Build(),
				leases: newLeaseClock(func() time.Time { return now }), requeue: &memberRequeue{}}
			hook := memberPollHook(e.requeue)
			member := poolCD("team-a", "page", 1, "v1-0-0", "pages")
			// Premise: as a member whose Lease nobody holds yet, it is starting, rechecked at G.
			ready, _, _, err := e.controllerConditions(ctx, &member, controllerPlacement(&member, pagesGVR), pagesGVR)
			if err != nil || !cdreason.Creating.Is(ready) {
				t.Fatalf("premise: a member with no Lease: Ready %s/%s %q err=%v, want Creating", ready.Status, ready.Reason, ready.Message, err)
			}
			if d := hook(&member, poll); d >= poll {
				t.Fatalf("premise: the member has no recheck (hook = %s)", d)
			}
			left := member.DeepCopy()
			c.leave(left)
			if _, _, _, err := e.controllerConditions(ctx, left, controllerPlacement(left, pagesGVR), pagesGVR); err != nil {
				t.Fatal(err)
			}
			if d := hook(left, poll); d != poll {
				t.Errorf("after it left the pool the hook brings it back in %s, want the poll (%s)", d, poll)
			}
		})
	}
}

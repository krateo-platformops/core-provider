package compositiondefinitions

import (
	"context"
	"strings"
	"sync"
	"time"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/cdreason"
	"github.com/krateo-platformops/core-provider/internal/tools/deploy"
	"github.com/krateo-platformops/core-provider/internal/tools/objects/templates"
	rtv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
	"github.com/krateo-platformops/provider-runtime/pkg/reconciler"
	"github.com/krateo-platformops/provider-runtime/pkg/resource"
	coordinationv1 "k8s.io/api/coordination/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// A POOL MEMBER'S STATE IS ITS OWNERSHIP LEASE (#653, Diego 2026-11-24; docs/design/cdc-pooling.md §5c
// "Per-member readiness, by its Lease"). A pool's pod is one process for every member, so its
// Deployment says whether the process runs, not whether a member does: a member that failed in a
// healthy process, and one that never started, both sit behind a Deployment that reads Available. Each
// member's controller holds the Lease templates.ControllerLeaseName names, renewed every 2s while it
// reconciles, and released when it stops. That is read here, on top of the Deployment's verdict.

// memberGrace is G (deploy.OwnershipLeaseTimings.MemberGrace, 20s at the default timings). A same-key
// replacement holds the Lease through its drain, and the successor takes it within Quiet, so a bump
// flips nobody.
func memberGrace() time.Duration { return deploy.LeaseTimings.MemberGrace() }

// leaseRecord is what this process has seen of one Lease: the Lease as last seen, and when, on this
// process's clock. initial: seen only in the informer's first list, so no renewal has been seen yet.
type leaseRecord struct {
	lease   *coordinationv1.Lease
	at      time.Time
	initial bool
}

// leaseClock records, per Lease in the management namespace, when this process last saw it change.
// Freshness is judged on this clock, never against the Lease's renewTime, which is the pool's clock:
// client-go's own rule (a record unchanged for leaseDurationSeconds belongs to a holder that is gone),
// so no two clocks are compared.
type leaseClock struct {
	mu      sync.Mutex
	now     func() time.Time
	records map[string]leaseRecord
	// missing: when this process first looked for a Lease that did not exist.
	missing map[string]time.Time
	synced  func() bool
}

func newLeaseClock(now func() time.Time) *leaseClock {
	if now == nil {
		now = time.Now
	}
	return &leaseClock{now: now, records: map[string]leaseRecord{}, missing: map[string]time.Time{}, synced: func() bool { return true }}
}

// observe records a Lease event and says whether it can change a member's verdict: a holder that
// changed, a Lease created or deleted, or a renewal after a silence of a lease duration, which brings a
// member that read stale back. A renewal otherwise changes nothing anyone reads, and wakes nobody.
func (c *leaseClock) observe(l *coordinationv1.Lease, initial, deleted bool) (wake bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	prev, had := c.records[l.Name]
	if deleted {
		delete(c.records, l.Name)
		return true
	}
	c.records[l.Name] = leaseRecord{lease: l.DeepCopy(), at: now, initial: initial}
	delete(c.missing, l.Name)
	if initial {
		return false // the controller's own start reconciles every definition
	}
	if !had {
		return true
	}
	return holderOf(prev.lease) != holderOf(l) || now.Sub(prev.at) >= leaseDuration(prev.lease)
}

func holderOf(l *coordinationv1.Lease) string {
	if l == nil || l.Spec.HolderIdentity == nil {
		return ""
	}
	return *l.Spec.HolderIdentity
}

func leaseDuration(l *coordinationv1.Lease) time.Duration {
	if l != nil && l.Spec.LeaseDurationSeconds != nil && *l.Spec.LeaseDurationSeconds > 0 {
		return time.Duration(*l.Spec.LeaseDurationSeconds) * time.Second
	}
	return deploy.LeaseTimings.Duration // a Lease that does not say: the timings the controllers run with
}

// memberKind is what a member's Lease says, beside a pool Deployment that has rolled out.
type memberKind int

const (
	memberRunning    memberKind = iota // held by a pod of the pool, fresh
	memberStarting                     // not held by the pool, for less than G
	memberTakingOver                   // held, fresh, by someone else: a dedicated pod handing over, or another pool
	memberNotRunning                   // not held by the pool for G or more
	memberUnknown                      // nothing can be said yet: the Lease informer has not synced
)

type memberState struct {
	kind   memberKind
	lease  string
	holder string
	// since is when the Lease was first known not to be held by the pool (memberStarting, memberNotRunning).
	since time.Time
	// recheck is when this verdict can change without any event (zero: only an event changes it).
	recheck time.Time
}

// state is pool's member gvr, as its Lease shows it now.
func (c *leaseClock) state(pool string, gvr schema.GroupVersionResource, namespace string) memberState {
	name := templates.ControllerLeaseName(gvr.Group, gvr.Version, gvr.Resource)
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.synced() {
		return memberState{kind: memberUnknown, lease: namespace + "/" + name}
	}
	now := c.now()
	rec, ok := c.records[name]
	if !ok {
		first, seen := c.missing[name]
		if !seen {
			first = now
			c.missing[name] = first
		}
		return notHeld(namespace+"/"+name, "", first, now)
	}
	return leaseVerdict(namespace+"/"+name, pool, rec, now)
}

// leaseVerdict is the decision, a pure function of what was seen and when.
//
//   - A Lease is fresh while a holder is named and this process saw it change less than its duration ago.
//   - A record from the informer's first list carries no renewal yet: for its first duration it is judged
//     by its renewTime, fresh while within twice the duration (wrong only with more skew than that, and
//     only then); after that it is stale, because a held Lease would have been renewed in between.
//   - Held by a pod of cdc-pool-<pool> (the Deployment's pods are named <Deployment>-<hash>-<suffix>) is
//     the pool running the member; held by anyone else is a hand-over in progress.
func leaseVerdict(name, pool string, rec leaseRecord, now time.Time) memberState {
	l, dur := rec.lease, leaseDuration(rec.lease)
	holder := holderOf(l)
	fresh, freshUntil := false, rec.at.Add(dur)
	switch {
	case holder == "":
	case rec.initial && now.Sub(rec.at) < dur:
		if l.Spec.RenewTime != nil {
			freshUntil = l.Spec.RenewTime.Add(2 * dur)
			fresh = now.Before(freshUntil)
			if freshUntil.After(rec.at.Add(dur)) {
				freshUntil = rec.at.Add(dur) // judged on this clock from then on
			}
		}
	default:
		fresh = now.Before(freshUntil)
	}
	if fresh && strings.HasPrefix(holder, deploy.PoolNames(pool).Workload+"-") {
		return memberState{kind: memberRunning, lease: name, holder: holder, recheck: freshUntil.Add(memberGrace())}
	}
	if fresh {
		return memberState{kind: memberTakingOver, lease: name, holder: holder}
	}
	since := rec.at // released, at the event that released it
	if holder != "" {
		since = freshUntil // stale: from when it stopped being renewed
	}
	if since.Before(rec.at) {
		// Judged by the pool's clock, from the first list: not before this process saw it, so skew at a
		// restart costs G, never a false Unavailable.
		since = rec.at
	}
	return notHeld(name, holder, since, now)
}

func notHeld(name, holder string, since, now time.Time) memberState {
	if now.Sub(since) < memberGrace() {
		return memberState{kind: memberStarting, lease: name, holder: holder, since: since, recheck: since.Add(memberGrace())}
	}
	return memberState{kind: memberNotRunning, lease: name, holder: holder, since: since}
}

// memberConditions refines the pool Deployment's verdict with the member's own state. It applies only
// when the Deployment says the process runs (Ready Available): a pool rolling out, given up or
// unavailable is the whole process's, and every member keeps saying so.
func memberConditions(ready, active rtv1.Condition, s memberState, pool string, gvr schema.GroupVersionResource) (rtv1.Condition, rtv1.Condition, bool) {
	if !cdreason.Available.Is(ready) {
		return ready, active, true
	}
	running := cdreason.ControllerRunning.WithMessage("controller running in pool " + pool)
	switch s.kind {
	case memberRunning:
		return ready, running, true
	case memberStarting:
		return cdreason.Creating.WithMessage("the controller for " + gvr.String() + " is starting in pool " + pool), running, true
	case memberTakingOver:
		return cdreason.Creating.WithMessage("the controller for " + gvr.String() + " is taking over from " + s.holder), running, true
	case memberNotRunning:
		msg := "the controller for " + gvr.String() + " is not running in pool " + pool + ": its ownership Lease " + s.lease +
			" has not been held since " + s.since.UTC().Format(time.RFC3339)
		return cdreason.Unavailable.WithMessage(msg), cdreason.ControllerUnavailable.WithMessage(msg), true
	}
	return ready, cdreason.ControllerNotObserved.WithMessage("the ownership Lease " + s.lease + " could not be read: core-provider has not listed the Leases yet"), false
}

// memberRequeue is when each pool member's definition must be looked at again although nothing changes:
// a member whose Lease stops being renewed by a process that is still alive produces no event. Observe
// writes it, and the reconciler's poll hook reads it.
type memberRequeue struct{ m sync.Map }

func (r *memberRequeue) set(key types.NamespacedName, at time.Time) {
	if r == nil {
		return
	}
	if at.IsZero() {
		r.m.Delete(key)
		return
	}
	r.m.Store(key, at)
}

// requeueMembers is whether the poll hook brings a pool member's definition back at its recheck time
// (#653: measured before it was kept, see the design). A variable so a test can measure without it.
var requeueMembers = true

// memberPollHook is the reconciler's poll interval for mg: its member's recheck time when that is sooner.
func memberPollHook(r *memberRequeue) reconciler.PollIntervalHook {
	return func(mg resource.Managed, poll time.Duration) time.Duration {
		if !requeueMembers {
			return poll
		}
		v, ok := r.m.Load(client.ObjectKeyFromObject(mg))
		if !ok {
			return poll
		}
		d := time.Until(v.(time.Time))
		if d < time.Second {
			d = time.Second
		}
		return min(d, poll)
	}
}

// leaseHandler feeds the clock from the Lease informer and wakes the definitions a Lease event can
// change: those whose controller runs in a pool and holds that Lease (an owner, or a follower, both of
// which record the owner's placement, #580).
func leaseHandler(c *leaseClock, cli client.Client) handler.TypedEventHandler[client.Object, reconcile.Request] {
	wake := func(ctx context.Context, name string, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
		var list compositiondefinitionsv1alpha1.CompositionDefinitionList
		if err := cli.List(ctx, &list); err != nil {
			return // the definitions' own poll still observes them
		}
		for _, r := range definitionsOfLease(name, list.Items) {
			q.Add(r)
		}
	}
	lease := func(o client.Object) (*coordinationv1.Lease, bool) {
		l, ok := o.(*coordinationv1.Lease)
		return l, ok && l.Namespace == ManagementNamespace
	}
	return handler.Funcs{
		CreateFunc: func(ctx context.Context, ev event.CreateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			if l, ok := lease(ev.Object); ok && c.observe(l, ev.IsInInitialList, false) {
				wake(ctx, l.Name, q)
			}
		},
		UpdateFunc: func(ctx context.Context, ev event.UpdateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			if l, ok := lease(ev.ObjectNew); ok && c.observe(l, false, false) {
				wake(ctx, l.Name, q)
			}
		},
		DeleteFunc: func(ctx context.Context, ev event.DeleteEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			if l, ok := lease(ev.Object); ok && c.observe(l, false, true) {
				wake(ctx, l.Name, q)
			}
		},
	}
}

// definitionsOfLease are the definitions whose controller runs in a pool and holds Lease name.
func definitionsOfLease(name string, cds []compositiondefinitionsv1alpha1.CompositionDefinition) []reconcile.Request {
	var out []reconcile.Request
	for i := range cds {
		cd := &cds[i]
		gvr, ok := statusGVR(cd)
		if !ok || cd.Status.Controller == nil || cd.Status.Controller.Pool == "" ||
			templates.ControllerLeaseName(gvr.Group, gvr.Version, gvr.Resource) != name {
			continue
		}
		out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(cd)})
	}
	return out
}

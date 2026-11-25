//go:build envtest

package compositiondefinitions

import (
	"context"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/tools/deploy"
	"github.com/krateo-platformops/core-provider/internal/tools/objects/templates"
	rtv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
	"github.com/krateo-platformops/provider-runtime/pkg/controller"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	"github.com/krateo-platformops/provider-runtime/pkg/ratelimiter"
	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// #653 through the production wiring: a controller-runtime manager running Setup, so the Lease watch,
// the pool Deployment's wake-up, the poll hook and the Lease clock are the ones that ship. The pool's
// process is leases652 (a real cdc pool cannot run here: it is another Go module); it takes, renews and
// releases each member's Lease as the pool would.

// manager653 runs core-provider's definitions controller on e's apiserver, polling every poll. It
// returns how many Observe passes the controller has made of each definition so far: the dry-run apply
// of the definition's env ConfigMap that every pass that reaches the drift checks makes.
//
// limiter is the controller's global rate limiter. Production's (main.go) is provider-runtime's
// ExponentialTimedFailure at 1s..1m, which delays EVERY reconcile of a definition reconciled within the
// last two minutes, events included, by up to a minute (it forgets an item only after 2 x max); the
// tests of a mechanism use one that limits nothing, and the measurement uses production's.
func manager653(t *testing.T, e *env652, poll time.Duration, limiter workqueue.TypedRateLimiter[any]) func(key string) int {
	t.Helper()
	cfg := rest.CopyConfig(e.c.cfg)
	var mu sync.Mutex
	passes := map[string]int{}
	cfg.WrapTransport = func(rt http.RoundTripper) http.RoundTripper {
		return roundTrip(func(r *http.Request) (*http.Response, error) {
			if r.URL.Query().Get("dryRun") != "" && strings.Contains(r.URL.Path, "/configmaps") {
				name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
				mu.Lock()
				passes[name]++
				mu.Unlock()
			}
			return rt.RoundTrip(r)
		})
	}
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 e.c.cl.Scheme(),
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Cache: cache.Options{ByObject: map[client.Object]cache.ByObject{
			&coordinationv1.Lease{}: {Namespaces: map[string]cache.Config{ns652: {}}},
		}},
		Controller: config.Controller{SkipNameValidation: ptr.To(true)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := Setup(mgr, Options{
		ControllerOptions: controller.Options{
			Logger: logging.NewNopLogger(), PollInterval: poll, MaxConcurrentReconciles: 5,
			GlobalRateLimiter: limiter,
		},
		Pluralizer: pluralizer644{e.c.cs.Discovery()},
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = mgr.Start(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	return func(key string) int {
		mu.Lock()
		defer mu.Unlock()
		return passes[key+"-configmap"]
	}
}

// unlimited is a global rate limiter that delays nothing.
func unlimited() workqueue.TypedRateLimiter[any] { return ratelimiter.NewGlobal(100000) }

// production is main.go's, at its defaults.
func production() workqueue.TypedRateLimiter[any] {
	return ratelimiter.NewGlobalExponential(time.Second, time.Minute)
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// readyLog records every persisted Ready and ControllerActive of every definition, from a watch.
type readyLog struct {
	mu  sync.Mutex
	evs []readyEv
}

type readyEv struct {
	at                  time.Time
	name, ready, active string
	msg                 string
}

func watchReady(t *testing.T, e *env652) *readyLog {
	t.Helper()
	w, err := e.c.raw.Resource(cdGVR644).Namespace(self642CDNS).Watch(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.Stop)
	r := &readyLog{}
	go func() {
		for ev := range w.ResultChan() {
			u, ok := ev.Object.(*unstructured.Unstructured)
			if !ok || ev.Type == watch.Deleted {
				continue
			}
			conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
			var rd, ac, msg string
			for _, c := range conds {
				m, _ := c.(map[string]any)
				switch m["type"] {
				case "Ready":
					rd, msg = m["status"].(string)+"/"+m["reason"].(string), stringOf(m["message"])
				case "ControllerActive":
					ac = m["status"].(string) + "/" + m["reason"].(string)
				}
			}
			r.mu.Lock()
			r.evs = append(r.evs, readyEv{time.Now(), u.GetName(), rd, ac, msg})
			r.mu.Unlock()
		}
	}()
	return r
}

func stringOf(v any) string { s, _ := v.(string); return s }

// since are the persisted states of name from t0 on.
func (r *readyLog) since(name string, t0 time.Time) []readyEv {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []readyEv
	for _, ev := range r.evs {
		if ev.name == name && !ev.at.Before(t0) {
			out = append(out, ev)
		}
	}
	return out
}

func (e *env652) waitReady(t *testing.T, d *cd644, ready, activePrefix string, within time.Duration) time.Duration {
	t.Helper()
	start := time.Now()
	for time.Since(start) < within {
		cd := &compositiondefinitionsv1alpha1.CompositionDefinition{}
		if err := e.c.cl.Get(context.Background(), d.key, cd); err == nil {
			r, a := cd.GetCondition(rtv1.TypeReady), cd.GetCondition(rtv1.ConditionType("ControllerActive"))
			if string(r.Status)+"/"+string(r.Reason) == ready && strings.HasPrefix(string(a.Status)+"/"+string(a.Reason), activePrefix) {
				return time.Since(start)
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s did not read Ready %s / ControllerActive %s within %s: Ready %s %q", d.key.Name, ready, activePrefix, within, d.cond("Ready"), d.message("Ready"))
	return 0
}

func leaseName653(e *env652, d *cd644) string {
	gvr := e.gvr(d, v1)
	return templates.ControllerLeaseName(gvr.Group, gvr.Version, gvr.Resource)
}

// One member that stops running reads Unavailable, on its own definition; the others never change.
// A member that comes back reads Available; one the pool never starts reads Creating, then Unavailable.
func TestEnvtest_653_OneMemberDoesNotDecideAnother(t *testing.T) {
	e := start652(t)
	a, b, c := e.member("alpha", "web"), e.member("beta", "web"), e.member("gamma", "web")
	log := watchReady(t, e)
	passes := manager653(t, e, 3*time.Minute, unlimited())
	for _, d := range []*cd644{a, b, c} {
		e.waitReady(t, d, "True/Available", "True/Running", 2*time.Minute)
	}
	if m := c.message("ControllerActive"); m != "controller running in pool web" {
		t.Errorf("ControllerActive %q", m)
	}
	t0 := time.Now()
	G := deploy.LeaseTimings.MemberGrace()
	before := map[string]int{e.key(b, v1): passes(e.key(b, v1)), e.key(c, v1): passes(e.key(c, v1))}
	// A second witness beside the watch: the live Ready of b and c, sampled. An up-to-date pass writes
	// no status, so the watch alone can be silent.
	var sampled []string
	stopSampling := make(chan struct{})
	sampling := make(chan struct{})
	go func() {
		defer close(sampling)
		for {
			select {
			case <-stopSampling:
				return
			case <-time.After(250 * time.Millisecond):
			}
			for _, d := range []*cd644{b, c} {
				cd := &compositiondefinitionsv1alpha1.CompositionDefinition{}
				if err := e.c.cl.Get(context.Background(), d.key, cd); err == nil {
					r := cd.GetCondition(rtv1.TypeReady)
					sampled = append(sampled, d.key.Name+" "+string(r.Status)+"/"+string(r.Reason))
				}
			}
		}
	}()

	// a's process stops renewing without releasing it: no event at all.
	e.leases.mu.Lock()
	e.leases.dead[leaseName653(e, a)] = true
	e.leases.mu.Unlock()
	took := e.waitReady(t, a, "False/Unavailable", "False/Unavailable", 2*time.Minute)
	t.Logf("a dead member read Unavailable after %s (lease duration %s + G %s)", took.Round(time.Second), deploy.LeaseTimings.Duration, G)
	if want := deploy.LeaseTimings.Duration + G; took < want-2*time.Second {
		t.Errorf("Unavailable after %s, before the Lease could be stale for G (%s)", took, want)
	}
	if !strings.Contains(a.message("Ready"), "is not running in pool web: its ownership Lease "+ns652+"/"+leaseName653(e, a)+" has not been held since ") {
		t.Errorf("Ready %q", a.message("Ready"))
	}

	// It comes back: the first renewal after the silence wakes it.
	e.leases.mu.Lock()
	delete(e.leases.dead, leaseName653(e, a))
	e.leases.mu.Unlock()
	if d := e.waitReady(t, a, "True/Available", "True/Running", time.Minute); d > 15*time.Second {
		t.Errorf("Available again after %s: the renewal after a silence should have woken it", d)
	}

	// The pool never runs it (released): Creating, then Unavailable after G.
	e.leases.mu.Lock()
	e.leases.refused[leaseName653(e, a)] = true
	e.leases.mu.Unlock()
	e.waitReady(t, a, "False/Creating", "True/Running", time.Minute)
	if !strings.Contains(a.message("Ready"), "is starting in pool web") {
		t.Errorf("Ready %q", a.message("Ready"))
	}
	e.waitReady(t, a, "False/Unavailable", "False/Unavailable", time.Minute)
	e.leases.mu.Lock()
	delete(e.leases.refused, leaseName653(e, a))
	e.leases.mu.Unlock()
	e.waitReady(t, a, "True/Available", "True/Running", time.Minute)

	close(stopSampling)
	<-sampling
	// INVARIANT, over every persisted state and every sample: b and c read Available and Running
	// throughout, and they were looked at in that time (or the invariant says nothing).
	bad := 0
	for _, s := range sampled {
		if !strings.HasSuffix(s, " True/Available") {
			bad++
			t.Errorf("sampled while a did not run: %s", s)
		}
	}
	t.Logf("%d samples of b and c, %d not Available", len(sampled), bad)
	for k, n := range before {
		if got := passes(k) - n; got == 0 {
			t.Errorf("%s was not observed once while a did not run: its unchanged Ready proves nothing", k)
		} else {
			t.Logf("%s: %d Observe passes while a did not run", k, got)
		}
	}
	for _, d := range []*cd644{b, c} {
		seen := log.since(d.key.Name, t0)
		for _, ev := range seen {
			if ev.ready != "True/Available" || ev.active != "True/Running" {
				t.Errorf("%s while a did not run: Ready %s %q, ControllerActive %s", d.key.Name, ev.ready, ev.msg, ev.active)
			}
		}
		t.Logf("%s: %d persisted states while a did not run, all Available", d.key.Name, len(seen))
	}
	// Control: the log sees a's states, so the silence above is not an empty watch.
	var as []string
	for _, ev := range log.since(a.key.Name, t0) {
		as = append(as, ev.ready)
	}
	if !slices.Contains(as, "False/Unavailable") || !slices.Contains(as, "False/Creating") {
		t.Errorf("control: a's persisted states %v", as)
	}
}

// The pool's Deployment is every member's: a member is woken by it, which before #653 it was not (the
// watch's predicate needed the per-version labels a pool's Deployment does not carry), so a member saw a
// change to its pool's Deployment only at its next poll.
func TestEnvtest_653_ThePoolDeploymentWakesItsMembers(t *testing.T) {
	e := start652(t)
	a, b := e.member("alpha", "web"), e.member("beta", "web")
	passes := manager653(t, e, 3*time.Minute, unlimited())
	for _, d := range []*cd644{a, b} {
		e.waitReady(t, d, "True/Available", "True/Running", 2*time.Minute)
	}
	// Quiet first: no pass of either for 8s, so a pass after the delete is the delete's.
	quiet := func() bool {
		x, y := passes(e.key(a, v1)), passes(e.key(b, v1))
		time.Sleep(8 * time.Second)
		return passes(e.key(a, v1)) == x && passes(e.key(b, v1)) == y
	}
	for end := time.Now().Add(2 * time.Minute); !quiet(); {
		if time.Now().After(end) {
			t.Fatal("premise: the members were never quiet for 8s")
		}
	}
	before := map[string]int{e.key(a, v1): passes(e.key(a, v1)), e.key(b, v1): passes(e.key(b, v1))}
	if err := e.c.cs.AppsV1().Deployments(ns652).Delete(context.Background(), "cdc-pool-web", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	for k, n := range before {
		woke := time.Duration(0)
		for end := time.Now().Add(3 * time.Second); woke == 0 && time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
			if passes(k) > n {
				woke = time.Since(t0)
			}
		}
		if woke == 0 {
			t.Errorf("%s was not observed within 3s of its pool's Deployment being deleted, after 8s quiet (poll 3m)", k)
			continue
		}
		t.Logf("%s observed %s after its pool's Deployment was deleted", k, woke.Round(100*time.Millisecond))
	}
	// The repair is the members' Update recreating the Deployment, so that is what is waited for. Their
	// Ready is no witness of it: it read True/Available before the delete and still did with the
	// Deployment gone, so a wait on it returned at once, before the Update ran (the gate run of 5b7ff1a).
	recreated := time.Duration(0)
	for end := time.Now().Add(30 * time.Second); recreated == 0 && time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		if e.exists(&appsv1.Deployment{}, "cdc-pool-web") {
			recreated = time.Since(t0)
		}
	}
	if recreated == 0 {
		t.Fatal("the pool's Deployment was not recreated within 30s of its delete")
	}
	t.Logf("the pool's Deployment was recreated %s after its delete", recreated.Round(100*time.Millisecond))
	for _, d := range []*cd644{a, b} {
		e.waitReady(t, d, "True/Available", "True/Running", time.Minute)
	}
}

// MEASUREMENT (#653), the decision on the requeue after G: how long a member whose process stopped
// renewing its Lease, without releasing it, reads Available. Nothing produces an event then, so without
// the requeue it is the poll interval (3m by default). Run with MEASURE_653=1; the default run asserts
// only the requeued arm, which is what ships.
func TestEnvtest_653_DeadMemberMeasured(t *testing.T) {
	type arm struct {
		name    string
		requeue bool
		limiter func() workqueue.TypedRateLimiter[any]
		bound   time.Duration // 0: measured only
	}
	L := deploy.LeaseTimings
	arms := []arm{{"with the requeue, production's rate limiter", true, production, L.Duration + L.MemberGrace() + time.Minute + 10*time.Second}}
	if os.Getenv("MEASURE_653") != "" {
		arms = append(arms,
			arm{"with the requeue, no rate limit", true, unlimited, 0},
			arm{"without the requeue, production's rate limiter", false, production, 0})
	}
	for _, c := range arms {
		t.Run(c.name, func(t *testing.T) {
			was := requeueMembers
			requeueMembers = c.requeue
			t.Cleanup(func() { requeueMembers = was })
			e := start652(t)
			a := e.member("alpha", "web")
			counter := manager653(t, e, 3*time.Minute, c.limiter())
			e.waitReady(t, a, "True/Available", "True/Running", 3*time.Minute)
			time.Sleep(5 * time.Second)
			key := e.key(a, v1)
			before, t0 := counter(key), time.Now()
			e.leases.mu.Lock()
			e.leases.dead[leaseName653(e, a)] = true
			e.leases.mu.Unlock()
			took := e.waitReady(t, a, "False/Unavailable", "False/Unavailable", 5*time.Minute)
			passes := counter(key) - before
			t.Logf("MEASURED %s: a dead member read Available for %s after it stopped renewing; %d Observe passes of it in that time (%.1f/min)",
				c.name, took.Round(time.Second), passes, float64(passes)/time.Since(t0).Minutes())
			if c.bound > 0 && took > c.bound {
				t.Errorf("%s > %s", took, c.bound)
			}
		})
	}
}

package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/krateo-platformops/unstructured-runtime/pkg/controller"
	metricsserver "github.com/krateo-platformops/unstructured-runtime/pkg/metrics/server"
	"github.com/krateo-platformops/unstructured-runtime/pkg/ratelimit"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// Pool runs the members a pool's membership index names (core-provider docs/design/cdc-pooling.md
// §5c). It is step 4's manager with membership read at runtime:
//
//   - core-provider writes the index, ConfigMap `cdc-pool-<p>-members`: one key per member, valued
//     with IndexEntry, the member's env ConfigMap and that ConfigMap's checksum. The pool WATCHES that
//     one ConfigMap by name, so a join, a leave, or a member's configuration changing is one event,
//     and nothing restarts the process (§3.2: the pool is not told through its own environment).
//   - A member's configuration is its env ConfigMap, read by name and parsed by MemberFromConfig,
//     exactly as a dedicated controller parses its environment.
//   - FAILURE ISOLATION (Diego, 2026-11-16, replacing step 4's provisional rule): a member that fails,
//     a lost Lease included, is rebuilt and restarted ALONE, with backoff; the others keep
//     reconciling. Only a process-level failure (the index watch, the metrics server) ends Run.
//   - NOTHING WAITS FOR A DRAIN BUT THE MEMBER DRAINING. A member that leaves, or whose configuration
//     changed, is cancelled at once; its drain (up to GracefulShutdownTimeout) is waited for by its own
//     goroutine, never under mu. The probes, the index handler and every other member go on meanwhile.
//   - ONE CONTROLLER PER KEY. Each key has one goroutine (run), which starts a controller only once the
//     previous one for that key has returned, so has drained and released its Lease. The Lease cannot
//     do this inside one process: both controllers would hold it as the same identity, the pod.
type Pool struct {
	Process Process
	Client  kubernetes.Interface
	// Namespace holds the index and the member ConfigMaps: the pool's own (the management namespace).
	Namespace string
	// Index is the membership index ConfigMap's name.
	Index    string
	Resolver ResolverFactory
	// Backoff is the wait before a failed member's next start; nil is 1s doubling to 1m.
	Backoff func(attempt int) time.Duration

	// mu guards running, stopping, and the poolMember fields that say so. It is held only to read or
	// change them, never across a drain, an API call or a controller's start.
	mu sync.Mutex
	// running holds every key that has a goroutine: running, backing off, or draining. A member that
	// left stays in it, with want nil, until its drain ends.
	running  map[string]*poolMember
	stopping bool

	// What /readyz has established (#653), guarded by mu. listed: the index's first list is done, at
	// listedAt; first: the members it named then, each true once its first attempt has synced or
	// failed; ready: latched, never cleared.
	listed   bool
	listedAt time.Time
	first    map[string]bool
	ready    bool
	// now is the clock; nil is time.Now. Tests replace it.
	now func() time.Time

	// attempt builds and runs one controller until it stops; nil is runOnce. Tests replace it.
	attempt func(ctx context.Context, key string, e IndexEntry, m *poolMember) error
}

// IndexEntry is one member's value in the index.
type IndexEntry struct {
	ConfigMap string `json:"configMap"`
	Checksum  string `json:"checksum"`
}

// ParseIndex reads the index's data into its entries. An entry that does not parse is reported, not
// skipped silently: a member core-provider meant to run would otherwise never start, with no trace.
func ParseIndex(data map[string]string) (map[string]IndexEntry, error) {
	out := make(map[string]IndexEntry, len(data))
	var bad []string
	for k, v := range data {
		var e IndexEntry
		if err := json.Unmarshal([]byte(v), &e); err != nil || e.ConfigMap == "" {
			bad = append(bad, k)
			continue
		}
		out[k] = e
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return out, fmt.Errorf("index entries that are not {configMap, checksum}: %s", strings.Join(bad, ", "))
	}
	return out, nil
}

// poolMember is one index key's place in the pool: the controller it runs, or is draining, and what it
// is to run next.
type poolMember struct {
	// done is closed when the key's goroutine has ended: its controller has drained.
	done chan struct{}

	// Guarded by Pool.mu.
	entry    IndexEntry         // what the current controller was started with
	want     *IndexEntry        // what the index asks for now; nil: the member has left
	cancel   context.CancelFunc // stops the current controller
	draining bool               // cancelled: the current controller is on its way out

	mu   sync.Mutex
	ctrl *controller.Controller // nil while building or backing off
	last error                  // the last failure, for readiness
}

// stopLocked cancels the current controller, once. It does not wait: run does.
func (m *poolMember) stopLocked() {
	if !m.draining {
		m.draining = true
		m.cancel()
	}
}

func (m *poolMember) set(c *controller.Controller, err error) {
	m.mu.Lock()
	m.ctrl, m.last = c, err
	m.mu.Unlock()
}

// Run watches the index and keeps the running members equal to it until ctx ends.
func (p *Pool) Run(ctx context.Context) error {
	if p.Index == "" || p.Namespace == "" || p.Client == nil {
		return errors.New("manager: a pool needs its index name, namespace and client")
	}
	// core-provider#561: the members share the process's governor, so it must be one built to be
	// shared (saturation.NewPoolGovernor). An unshared one, or none, lets an idle member's worker hold
	// a slot a busy member needs, runs a CPU sampler per member, and enforces no pool total.
	if sg, ok := p.Process.Governor.(ratelimit.SharedLocalGate); !ok || !sg.SharedLocal() {
		return fmt.Errorf("manager: a pool's members need a SHARED governor (saturation.NewPoolGovernor), got %T", p.Process.Governor)
	}
	p.mu.Lock()
	p.running = map[string]*poolMember{}
	p.mu.Unlock()
	if err := p.startMetrics(ctx); err != nil {
		return err
	}

	lw := cache.NewListWatchFromClient(p.Client.CoreV1().RESTClient(), "configmaps", p.Namespace,
		fields.OneTermEqualSelector("metadata.name", p.Index))
	_, informer := cache.NewInformerWithOptions(cache.InformerOptions{
		ListerWatcher: lw,
		ObjectType:    &corev1.ConfigMap{},
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc:    func(obj any) { p.sync(ctx, obj) },
			UpdateFunc: func(_, obj any) { p.sync(ctx, obj) },
			DeleteFunc: func(any) { p.sync(ctx, nil) }, // no index: no members (the pool is being torn down)
		},
	})
	go func() {
		if cache.WaitForCacheSync(ctx.Done(), informer.HasSynced) {
			p.indexListed()
		}
	}()
	informer.Run(ctx.Done())
	p.stopAll()
	return nil
}

// sync makes the pool match the index. It never waits: a member that leaves or changes is cancelled,
// and its own goroutine drains it and starts what the index asks for BY THEN. So a later index
// supersedes an earlier one that is still draining, and the configurations in between never run.
func (p *Pool) sync(ctx context.Context, obj any) {
	want := map[string]IndexEntry{}
	if cm, ok := obj.(*corev1.ConfigMap); ok {
		var err error
		if want, err = ParseIndex(cm.Data); err != nil {
			p.logError(err, "the membership index has entries that cannot be read; those members are not started")
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopping {
		return
	}
	for key, m := range p.running {
		e, ok := want[key]
		if !ok {
			m.want = nil
			m.stopLocked()
			continue
		}
		m.want = &e
		if e != m.entry {
			m.stopLocked()
		}
	}
	keys := make([]string, 0, len(want))
	for k := range want {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if _, ok := p.running[key]; !ok {
			p.startLocked(ctx, key, want[key])
		}
	}
}

func (p *Pool) startLocked(ctx context.Context, key string, e IndexEntry) {
	m := &poolMember{want: &e, done: make(chan struct{})}
	p.running[key] = m
	mctx := m.nextLocked(ctx)
	go p.run(ctx, mctx, key, m)
}

// nextLocked makes what the index asks for the member's current controller, and returns its context.
func (m *poolMember) nextLocked(ctx context.Context) context.Context {
	mctx, cancel := context.WithCancel(ctx)
	m.entry, m.cancel, m.draining = *m.want, cancel, false
	m.set(nil, nil)
	return mctx
}

// run is key's goroutine, the only one that runs its controllers: one after another, each until it is
// cancelled and has drained. It ends when the member has left, or the pool is stopping.
func (p *Pool) run(ctx, mctx context.Context, key string, m *poolMember) {
	defer close(m.done)
	for mctx != nil {
		p.runUntilStopped(mctx, key, m)
		mctx = p.drained(ctx, key, m)
	}
}

// drained is called once key's controller has stopped. It starts the next one, or forgets the key and
// returns nil. Both are decided under one hold of mu, so an index change lands either on this member
// (seen here) or after it is gone (started afresh by sync), never in between.
func (p *Pool) drained(ctx context.Context, key string, m *poolMember) context.Context {
	p.mu.Lock()
	defer p.mu.Unlock()
	m.cancel()
	if m.want == nil || p.stopping || ctx.Err() != nil {
		delete(p.running, key)
		return nil
	}
	return m.nextLocked(ctx)
}

// runUntilStopped runs the member's current configuration until ctx ends, restarting it alone, with
// backoff, each time it stops by itself.
func (p *Pool) runUntilStopped(ctx context.Context, key string, m *poolMember) {
	p.mu.Lock()
	e := m.entry
	p.mu.Unlock()
	run := p.attempt
	if run == nil {
		run = p.runOnce
	}
	for attempt := 0; ; attempt++ {
		// Each attempt has its own context, ended with it: a controller that stopped by itself (a lost
		// Lease) returns with its informer still running on the context it was given.
		actx, cancel := context.WithCancel(ctx)
		err := run(actx, key, e, m)
		cancel()
		if ctx.Err() != nil {
			return // left the pool, changed, or the process is stopping: not a failure to retry
		}
		m.set(nil, err)
		p.logError(err, "pool member stopped; restarting it alone", "member", key, "attempt", attempt+1)
		select {
		case <-ctx.Done():
			return
		case <-time.After(p.backoff(attempt)):
		}
	}
}

// runOnce reads the member's configuration, builds its controller and runs it until it stops.
func (p *Pool) runOnce(ctx context.Context, key string, e IndexEntry, m *poolMember) error {
	cm, err := p.Client.CoreV1().ConfigMaps(p.Namespace).Get(ctx, e.ConfigMap, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("reading member configuration %s/%s: %w", p.Namespace, e.ConfigMap, err)
	}
	member, err := MemberFromConfig(cm.Data, p.Resolver, func(msg string, err error) { p.logError(err, msg, "member", key) })
	if err != nil {
		return err
	}
	c, err := p.Process.build(ctx, member)
	if err != nil {
		return fmt.Errorf("building the controller for %s: %w", member.GVR, err)
	}
	m.set(c, nil)
	if err := c.Run(ctx, member.workers(p.Process)); err != nil {
		return fmt.Errorf("%s: %w", member.GVR, err)
	}
	return errors.New("the controller returned without an error")
}

// stopMargin is how much longer than the members' own drain (GracefulShutdownTimeout) stopAll waits:
// a member that has drained still releases its Lease, an API call bounded by the elector's 10s renew
// deadline. 30s + 10s stays under the chart's 45s terminationGracePeriodSeconds.
const stopMargin = 10 * time.Second

// stopAll stops every member at once and waits for all of them. The members drain side by side, so the
// wait is the longest drain, not their sum, and it is bounded: the process is exiting, and a member
// that does not stop (a build that ignores its context) must not hold the exit past the kubelet's
// SIGKILL. A negative GracefulShutdownTimeout means "wait for ever" here too.
func (p *Pool) stopAll() {
	p.mu.Lock()
	p.stopping = true
	members := make(map[string]*poolMember, len(p.running))
	for key, m := range p.running {
		m.want = nil
		m.stopLocked()
		members[key] = m
	}
	p.mu.Unlock()

	var deadline <-chan time.Time
	if grace := p.Process.GracefulShutdownTimeout; grace >= 0 {
		t := time.NewTimer(grace + stopMargin)
		defer t.Stop()
		deadline = t.C
	}
	for _, m := range members {
		select {
		case <-m.done:
		case <-deadline:
			p.logError(fmt.Errorf("still running: %s", strings.Join(notDone(members), ", ")), "exiting without waiting longer for pool members to stop")
			return
		}
	}
}

func notDone(members map[string]*poolMember) []string {
	var keys []string
	for key, m := range members {
		select {
		case <-m.done:
		default:
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

// wantedLocked lists the members the index names, sorted. A member that has left and is still draining
// is not one.
func (p *Pool) wantedLocked() []string {
	keys := make([]string, 0, len(p.running))
	for k, m := range p.running {
		if m.want != nil {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

// memberSyncWait bounds how long /readyz waits for the members the index named at the first list. A
// member that can never list (its CRD version is not served, #333) would otherwise keep the pod
// NotReady, and with it every member's definition: what the per-member Lease reports instead (#653).
const memberSyncWait = time.Minute

// indexListed records that the index's first list is done, and which members it named.
func (p *Pool) indexListed() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.listed, p.listedAt, p.first = true, p.clock(), map[string]bool{}
	for _, k := range p.wantedLocked() {
		p.first[k] = false
	}
}

func (p *Pool) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// ReadyzCheck is the PROCESS's readiness (#653, docs/design/cdc-pooling.md §5c): ready once the index
// has been listed and each member it named then has synced once, or its first attempt has failed, or
// memberSyncWait has passed. It then stays ready: a member failing, draining, joining or leaving later
// is that member's, and core-provider reads it from the member's ownership Lease, on that member's
// definition alone. Before, one member that failed made the pod NotReady, and so every member's
// definition read Unavailable. It never waits.
func (p *Pool) ReadyzCheck(req *http.Request) error {
	type state struct {
		key  string
		ctrl *controller.Controller
		last error
	}
	p.mu.Lock()
	if p.ready {
		p.mu.Unlock()
		return nil
	}
	if !p.listed {
		p.mu.Unlock()
		return errors.New("the membership index has not been listed yet")
	}
	var pending []state
	for k, done := range p.first {
		m := p.running[k]
		if done || m == nil || m.want == nil {
			delete(p.first, k) // done, or it left before it ran: not this process's to wait for
			continue
		}
		m.mu.Lock()
		pending = append(pending, state{k, m.ctrl, m.last})
		m.mu.Unlock()
	}
	expired := p.clock().Sub(p.listedAt) >= memberSyncWait
	p.mu.Unlock()

	sort.Slice(pending, func(i, j int) bool { return pending[i].key < pending[j].key })
	var waiting []string
	var done []string
	for _, s := range pending {
		if s.last != nil || (s.ctrl != nil && s.ctrl.ReadyzCheck(req) == nil) {
			done = append(done, s.key)
			continue
		}
		waiting = append(waiting, s.key)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, k := range done {
		p.first[k] = true
	}
	if len(waiting) == 0 || expired {
		if len(waiting) > 0 {
			p.logError(fmt.Errorf("not synced after %s: %s", memberSyncWait, strings.Join(waiting, ", ")),
				"pool ready without these members; each is reported on its own definition")
		}
		p.ready = true
		return nil
	}
	return fmt.Errorf("the first members have not synced yet: %s", strings.Join(waiting, ", "))
}

// membersReady says whether every member the index names is running and ready, and which is not: a
// member being rebuilt after a failure, one whose previous controller is still draining, one starting.
// A member that has LEFT does not count while it drains. It reads state and never waits. It is no longer
// the probe (#653): one member must not decide another's readiness, so it is what the process logs and
// tests read.
func (p *Pool) membersReady(req *http.Request) error {
	type state struct {
		key      string
		draining bool
		ctrl     *controller.Controller
		last     error
	}
	var states []state
	p.mu.Lock()
	for _, k := range p.wantedLocked() {
		m := p.running[k]
		m.mu.Lock()
		states = append(states, state{k, m.draining, m.ctrl, m.last})
		m.mu.Unlock()
	}
	p.mu.Unlock()
	for _, s := range states {
		switch {
		case s.draining:
			return fmt.Errorf("%s: its previous controller is draining; it restarts after that", s.key)
		case s.ctrl == nil && s.last != nil:
			return fmt.Errorf("%s: restarting after: %w", s.key, s.last)
		case s.ctrl == nil:
			return fmt.Errorf("%s: starting", s.key)
		}
		if err := s.ctrl.ReadyzCheck(req); err != nil {
			return fmt.Errorf("%s: %w", s.key, err)
		}
	}
	return nil
}

// Members lists the keys of the members the index names, sorted.
func (p *Pool) Members() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.wantedLocked()
}

func (p *Pool) backoff(attempt int) time.Duration {
	if p.Backoff != nil {
		return p.Backoff(attempt)
	}
	d := time.Second << min(attempt, 6)
	return min(d, time.Minute)
}

func (p *Pool) startMetrics(ctx context.Context) error {
	addr := p.Process.MetricsBindAddress
	if addr == "" || addr == "0" {
		return nil
	}
	srv, err := metricsserver.NewServer(metricsserver.Options{BindAddress: addr}, p.Process.Config, http.DefaultClient)
	if err != nil {
		return fmt.Errorf("manager: creating the metrics server: %w", err)
	}
	if srv != nil {
		go func() {
			if err := srv.WithLogger(p.Process.Log).Start(ctx); err != nil {
				p.logError(err, "metrics server failed")
			}
		}()
	}
	return nil
}

func (p *Pool) logError(err error, msg string, kv ...any) {
	if p.Process.Log != nil && err != nil {
		p.Process.Log.Error(err, msg, kv...)
	}
}

// The pool's state machine (docs/design/cdc-pooling.md §5c) with the controllers replaced by fakes whose
// drain the test controls: nothing waits for a drain but the member draining, and one key never runs two
// controllers. pool_drain_envtest_test.go runs the same against real controllers and Leases.
package manager

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// fakeRun is Pool.attempt: one "controller" that runs until its context ends and then drains for as long
// as the test holds it.
type fakeRun struct {
	mu       sync.Mutex
	active   map[string]int           // controllers running or draining now, by key
	overlap  []string                 // keys that had two at once
	log      []string                 // "start k=sum", "cancel k=sum", "stop k=sum", in order
	hold     map[string]chan struct{} // a key's drain lasts until its channel is closed
	drain    time.Duration            // and then this long
	fail     map[string]error         // what a key's attempt returns when it stops
	failFast map[string]bool          // a key whose attempt fails without being cancelled
	ctxs     map[string][]context.Context
}

func newFakeRun() *fakeRun {
	return &fakeRun{active: map[string]int{}, hold: map[string]chan struct{}{}, fail: map[string]error{},
		failFast: map[string]bool{}, ctxs: map[string][]context.Context{}}
}

func (f *fakeRun) note(what, key string, e IndexEntry) {
	f.log = append(f.log, what+" "+key+"="+e.Checksum)
}

func (f *fakeRun) attempt(ctx context.Context, key string, e IndexEntry, _ *poolMember) error {
	f.mu.Lock()
	f.active[key]++
	if f.active[key] > 1 {
		f.overlap = append(f.overlap, key)
	}
	f.ctxs[key] = append(f.ctxs[key], ctx)
	f.note("start", key, e)
	failFast := f.failFast[key]
	f.mu.Unlock()
	if !failFast {
		<-ctx.Done()
		f.mu.Lock()
		f.note("cancel", key, e)
		hold := f.hold[key]
		drain := f.drain
		f.mu.Unlock()
		if hold != nil {
			<-hold
		}
		time.Sleep(drain)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.active[key]--
	f.note("stop", key, e)
	return f.fail[key]
}

// holdDrain makes key's drain last until the returned func is called (or the test ends).
func (f *fakeRun) holdDrain(t *testing.T, key string) (release func()) {
	t.Helper()
	c := make(chan struct{})
	f.mu.Lock()
	f.hold[key] = c
	f.mu.Unlock()
	var once sync.Once
	release = func() {
		once.Do(func() {
			f.mu.Lock()
			delete(f.hold, key)
			f.mu.Unlock()
			close(c)
		})
	}
	t.Cleanup(release)
	return release
}

func (f *fakeRun) saw(line string) bool { return f.count(line) > 0 }

func (f *fakeRun) count(line string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, l := range f.log {
		if l == line {
			n++
		}
	}
	return n
}

// lines are key's log lines, in order.
func (f *fakeRun) lines(key string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, l := range f.log {
		if strings.Contains(l, " "+key+"=") {
			out = append(out, l)
		}
	}
	return out
}

func (f *fakeRun) noOverlap(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.overlap) > 0 {
		t.Errorf("two controllers at once for %v; log: %v", f.overlap, f.log)
	}
}

func indexOf(members map[string]string) *corev1.ConfigMap {
	data := make(map[string]string, len(members))
	for k, sum := range members {
		data[k] = fmt.Sprintf(`{"configMap":%q,"checksum":%q}`, k+"-configmap", sum)
	}
	return &corev1.ConfigMap{Data: data}
}

// newDrainPool is an empty pool over f, and how to apply an index to it.
func newDrainPool(t *testing.T, f *fakeRun, backoff func(int) time.Duration) (*Pool, func(map[string]string)) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p := &Pool{Process: testProcess(), running: map[string]*poolMember{}, attempt: f.attempt, Backoff: backoff}
	return p, func(m map[string]string) {
		t.Helper()
		within(t, 2*time.Second, "applying an index change", func() { p.sync(ctx, indexOf(m)) })
	}
}

// drainPool is newDrainPool running members.
func drainPool(t *testing.T, f *fakeRun, members map[string]string) (*Pool, func(map[string]string)) {
	t.Helper()
	p, set := newDrainPool(t, f, func(int) time.Duration { return time.Millisecond })
	set(members)
	for k, sum := range members {
		soon(t, "start "+k, func() bool { return f.saw("start " + k + "=" + sum) })
	}
	return p, set
}

// within fails the test when fn has not returned after d: fn waited for something.
func within(t *testing.T, d time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not return within %s: it is waiting", what, d)
	}
}

func soon(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(2 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for: %s", what)
}

// gone waits until key has left the pool altogether: drained, its goroutine ended.
func gone(t *testing.T, p *Pool, key string) {
	t.Helper()
	soon(t, key+" gone", func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		_, ok := p.running[key]
		return !ok
	})
}

// One member's long drain holds up nobody: not the probe, not the member list, not another member's
// start, not another member's stop.
func TestPoolDrain_BlocksNobodyElse(t *testing.T) {
	f := newFakeRun()
	release := f.holdDrain(t, "a")
	p, set := drainPool(t, f, map[string]string{"a": "1", "b": "1"})

	set(map[string]string{"b": "1", "c": "1"}) // a leaves and drains; c joins
	soon(t, "a cancelled", func() bool { return f.saw("cancel a=1") })
	within(t, time.Second, "ReadyzCheck", func() { _ = p.membersReady(nil) })
	within(t, time.Second, "Members", func() {
		if got := p.Members(); !reflect.DeepEqual(got, []string{"b", "c"}) {
			t.Errorf("Members() = %v while a drains after leaving, want [b c]", got)
		}
	})
	soon(t, "c started while a drains", func() bool { return f.saw("start c=1") })

	set(map[string]string{"c": "1"}) // b leaves
	soon(t, "b stopped while a drains", func() bool { return f.saw("stop b=1") })
	if f.saw("stop a=1") {
		t.Fatal("control: a was to be still draining")
	}
	release()
	gone(t, p, "a")
	f.noOverlap(t)
}

// Readiness while a member drains: a member that LEFT is no longer the index's, so it does not count; a
// member being REPLACED is named, and stays not ready until its successor is.
func TestPoolDrain_Readiness(t *testing.T) {
	f := newFakeRun()
	release := f.holdDrain(t, "a")
	p, set := drainPool(t, f, map[string]string{"a": "1"})
	set(map[string]string{"a": "2"})
	soon(t, "a cancelled", func() bool { return f.saw("cancel a=1") })
	readyz := func() (err error) {
		t.Helper()
		within(t, time.Second, "ReadyzCheck", func() { err = p.membersReady(nil) })
		return err
	}
	if err := readyz(); err == nil || !strings.HasPrefix(err.Error(), "a: ") || !strings.Contains(err.Error(), "draining") {
		t.Errorf("a being replaced: ReadyzCheck = %v, want a named as draining", err)
	}
	set(map[string]string{}) // a leaves while it drains
	if err := readyz(); err != nil {
		t.Errorf("a has left and is draining: ReadyzCheck = %v, want nil", err)
	}
	release()
	gone(t, p, "a")
}

// A configuration change: the successor starts only once its predecessor has drained.
func TestPoolDrain_SuccessorWaitsForItsPredecessor(t *testing.T) {
	f := newFakeRun()
	release := f.holdDrain(t, "a")
	_, set := drainPool(t, f, map[string]string{"a": "1"})
	set(map[string]string{"a": "2"})
	soon(t, "a cancelled", func() bool { return f.saw("cancel a=1") })
	time.Sleep(100 * time.Millisecond)
	if f.saw("start a=2") {
		t.Fatal("the successor started while its predecessor was draining")
	}
	release()
	soon(t, "the successor started", func() bool { return f.saw("start a=2") })
	if got, want := f.lines("a"), []string{"start a=1", "cancel a=1", "stop a=1", "start a=2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("a: %v, want %v", got, want)
	}
	f.noOverlap(t)
}

// Every index change that lands during one drain supersedes the one before it: only the last is
// applied, once, after the drain.
func TestPoolDrain_LastIndexChangeWins(t *testing.T) {
	const left = ""
	for name, c := range map[string]struct {
		changes []string // a's checksum in each successive index; "" = not in it
		want    []string // what a runs after its drain
	}{
		"changed twice runs only the last":         {[]string{"2", "3"}, []string{"start a=3"}},
		"changed and changed back restarts":        {[]string{"2", "1"}, []string{"start a=1"}},
		"leave":                                    {[]string{left}, nil},
		"leave, rejoin unchanged":                  {[]string{left, "1"}, []string{"start a=1"}},
		"leave, rejoin changed":                    {[]string{left, "2"}, []string{"start a=2"}},
		"leave, rejoin, leave":                     {[]string{left, "2", left}, nil},
		"leave, rejoin, leave, rejoin":             {[]string{left, "2", left, "3"}, []string{"start a=3"}},
		"changed, leave":                           {[]string{"2", left}, nil},
		"changed, leave, rejoin with the old one":  {[]string{"2", left, "1"}, []string{"start a=1"}},
		"changed, unrelated index change, changed": {[]string{"2", "2", "3"}, []string{"start a=3"}},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeRun()
			release := f.holdDrain(t, "a")
			p, set := drainPool(t, f, map[string]string{"a": "1", "other": "1"})
			for _, sum := range c.changes {
				m := map[string]string{"other": "1"}
				if sum != left {
					m["a"] = sum
				}
				set(m)
			}
			soon(t, "a cancelled", func() bool { return f.saw("cancel a=1") })
			time.Sleep(20 * time.Millisecond)
			if got, want := f.lines("a"), []string{"start a=1", "cancel a=1"}; !reflect.DeepEqual(got, want) {
				t.Fatalf("while draining, a: %v, want %v", got, want)
			}
			release()
			if c.want == nil {
				gone(t, p, "a")
			} else {
				soon(t, c.want[0], func() bool { return f.saw(c.want[0]) })
			}
			time.Sleep(20 * time.Millisecond)
			if got, want := f.lines("a"), append([]string{"start a=1", "cancel a=1", "stop a=1"}, c.want...); !reflect.DeepEqual(got, want) {
				t.Errorf("a: %v, want %v", got, want)
			}
			if got := f.lines("other"); !reflect.DeepEqual(got, []string{"start other=1"}) {
				t.Errorf("the other member was disturbed: %v", got)
			}
			f.noOverlap(t)
			// Whatever happened, the key is not stuck: the next index applies.
			set(map[string]string{"a": "9", "other": "1"})
			soon(t, "a runs the next index", func() bool { return f.saw("start a=9") })
		})
	}
}

// The next portal release: every member's entry changes in one index write. All are cancelled at once
// and drain side by side, so the whole change takes one drain, not fifteen.
func TestPoolDrain_FifteenMembersChangedInOneWrite(t *testing.T) {
	f := newFakeRun()
	f.drain = 300 * time.Millisecond
	before, after := map[string]string{}, map[string]string{}
	for i := range 15 {
		k := fmt.Sprintf("page%02d", i)
		before[k], after[k] = "1", "2"
	}
	_, set := drainPool(t, f, before)
	start := time.Now()
	set(after)
	for k := range after {
		soon(t, k+" restarted", func() bool { return f.saw("start " + k + "=2") })
	}
	if d := time.Since(start); d > 3*f.drain {
		t.Errorf("15 members took %s for a %s drain each: they did not drain side by side", d, f.drain)
	}
	for k := range after {
		if n := f.count("start " + k + "=2"); n != 1 {
			t.Errorf("%s started %d times on its new configuration", k, n)
		}
	}
	f.noOverlap(t)
}

// Process exit: every member is cancelled at once, the wait is one drain, and nothing starts after it.
func TestPoolDrain_StopAllIsOneDrainLong(t *testing.T) {
	f := newFakeRun()
	f.drain = 300 * time.Millisecond
	members := map[string]string{}
	for i := range 15 {
		members[fmt.Sprintf("page%02d", i)] = "1"
	}
	p, set := drainPool(t, f, members)
	start := time.Now()
	within(t, 10*time.Second, "stopAll", p.stopAll)
	if d := time.Since(start); d > 3*f.drain {
		t.Errorf("stopAll of 15 members took %s for a %s drain each", d, f.drain)
	}
	for k := range members {
		if got, want := f.lines(k), []string{"start " + k + "=1", "cancel " + k + "=1", "stop " + k + "=1"}; !reflect.DeepEqual(got, want) {
			t.Errorf("after stopAll, %s: %v, want %v", k, got, want)
		}
	}
	set(map[string]string{"late": "1"})
	time.Sleep(50 * time.Millisecond)
	if f.saw("start late=1") {
		t.Error("a member started after the pool stopped")
	}
}

// A member already draining for its successor when the process exits: the successor never starts.
func TestPoolDrain_StopAllSupersedesASuccessor(t *testing.T) {
	f := newFakeRun()
	release := f.holdDrain(t, "a")
	p, set := drainPool(t, f, map[string]string{"a": "1"})
	set(map[string]string{"a": "2"})
	soon(t, "a cancelled", func() bool { return f.saw("cancel a=1") })
	done := make(chan struct{})
	go func() { defer close(done); p.stopAll() }()
	soon(t, "stopping", func() bool {
		if !p.mu.TryLock() {
			return false
		}
		defer p.mu.Unlock()
		return p.stopping
	})
	select {
	case <-done:
		t.Fatal("stopAll returned before the member had drained")
	case <-time.After(50 * time.Millisecond):
	}
	within(t, time.Second, "ReadyzCheck during stopAll", func() { _ = p.membersReady(nil) })
	release()
	within(t, 5*time.Second, "stopAll", func() { <-done })
	if f.saw("start a=2") {
		t.Error("the successor started during the process's exit")
	}
}

// Process exit is bounded: a member that never stops does not hold it for ever. The bound is the members'
// own drain plus stopMargin, inside the pod's terminationGracePeriodSeconds.
func TestPoolDrain_StopAllIsBounded(t *testing.T) {
	t.Parallel() // stopMargin long
	f := newFakeRun()
	f.holdDrain(t, "stuck") // never released before the test ends
	p, _ := drainPool(t, f, map[string]string{"stuck": "1", "ok": "1"})
	p.Process.GracefulShutdownTimeout = 100 * time.Millisecond
	start := time.Now()
	within(t, stopMargin+5*time.Second, "stopAll with a member that never stops", p.stopAll)
	if d := time.Since(start); d < p.Process.GracefulShutdownTimeout+stopMargin {
		t.Errorf("stopAll gave up after %s, before the members' own drain could end", d)
	}
	if !f.saw("stop ok=1") {
		t.Error("the other member did not drain")
	}
}

// A member that fails WHILE it drains: its error is the end of the drain, not a failure to retry, so its
// successor starts at once (no backoff) and readiness does not report it as failing.
func TestPoolDrain_FailureWhileDraining(t *testing.T) {
	f := newFakeRun()
	f.fail["a"] = errors.New("boom")
	release := f.holdDrain(t, "a")
	p, set := newDrainPool(t, f, func(int) time.Duration { return time.Hour })
	set(map[string]string{"a": "1"})
	soon(t, "a started", func() bool { return f.saw("start a=1") })
	set(map[string]string{"a": "2"})
	soon(t, "a cancelled", func() bool { return f.saw("cancel a=1") })
	release()
	soon(t, "the successor started despite the failure", func() bool { return f.saw("start a=2") })
	if err := p.membersReady(nil); err == nil || strings.Contains(err.Error(), "boom") {
		t.Errorf("ReadyzCheck = %v: the predecessor's error is not the successor's", err)
	}
	f.noOverlap(t)
}

// A member failing by itself, in backoff: it leaves, or takes its new configuration, at once. And each
// failed attempt's context has ended before the next begins, so the informer a controller leaves behind
// when it loses its Lease does not run beside its successor.
func TestPoolDrain_FailingMember(t *testing.T) {
	f := newFakeRun()
	f.fail["a"], f.failFast["a"] = errors.New("boom"), true
	var backoff atomic.Int64
	backoff.Store(int64(time.Millisecond))
	p, set := newDrainPool(t, f, func(int) time.Duration { return time.Duration(backoff.Load()) })
	set(map[string]string{"a": "1", "b": "1"})
	soon(t, "a retried", func() bool { return f.count("start a=1") >= 3 })
	f.mu.Lock()
	for i, ctx := range f.ctxs["a"][:2] {
		if ctx.Err() == nil {
			t.Errorf("attempt %d's context was still live when the next had started", i+1)
		}
	}
	f.mu.Unlock()
	if err := p.membersReady(nil); err == nil || !strings.HasPrefix(err.Error(), "a: ") {
		t.Errorf("ReadyzCheck = %v, want a named", err)
	}
	if got := f.lines("b"); !reflect.DeepEqual(got, []string{"start b=1"}) {
		t.Errorf("a failing disturbed b: %v", got)
	}

	backoff.Store(int64(time.Hour)) // a now waits it out: only an index change can end the wait
	time.Sleep(50 * time.Millisecond)
	f.mu.Lock()
	f.failFast["a"] = false
	f.mu.Unlock()
	set(map[string]string{"a": "2", "b": "1"})
	soon(t, "a runs its new configuration", func() bool { return f.saw("start a=2") })
	if err := p.membersReady(nil); err == nil || err.Error() != "a: starting" {
		t.Errorf("ReadyzCheck = %v: the old configuration's failure is not the new one's", err)
	}
	set(map[string]string{"b": "1"})
	gone(t, p, "a")
	f.noOverlap(t)
}

// Any sequence of index changes, over short drains and one that outlasts them all: never two controllers
// for a key, and the pool ends up running exactly the last index.
func TestPoolDrain_AnySequenceEndsOnTheLastIndex(t *testing.T) {
	rnd := rand.New(rand.NewSource(1))
	f := newFakeRun()
	f.drain = 3 * time.Millisecond
	release := f.holdDrain(t, "k0")
	p, set := drainPool(t, f, map[string]string{"k0": "0"})
	var last map[string]string
	for i := range 400 {
		last = map[string]string{}
		for k := range 5 {
			if rnd.Intn(3) > 0 {
				last[fmt.Sprintf("k%d", k)] = fmt.Sprint(rnd.Intn(3))
			}
		}
		if i == 399 {
			last["k0"] = "last"
		}
		set(last)
		within(t, time.Second, "ReadyzCheck", func() { _ = p.membersReady(nil) })
		time.Sleep(time.Duration(rnd.Intn(4)) * time.Millisecond)
	}
	if n := f.count("stop k0=0"); n != 0 {
		t.Fatal("control: k0's first controller was to drain throughout")
	}
	release()
	soon(t, "the pool runs exactly the last index", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		now := map[string]string{}
		for _, l := range f.log {
			var what, kv string
			if _, err := fmt.Sscan(l, &what, &kv); err != nil {
				return false
			}
			k, sum, _ := strings.Cut(kv, "=")
			switch what {
			case "start":
				now[k] = sum
			case "cancel":
				delete(now, k)
			}
		}
		return reflect.DeepEqual(now, last)
	})
	if got, want := f.lines("k0"), []string{"start k0=0", "cancel k0=0", "stop k0=0", "start k0=last"}; !reflect.DeepEqual(got, want) {
		t.Errorf("k0: %v, want %v", got, want)
	}
	f.noOverlap(t)
}

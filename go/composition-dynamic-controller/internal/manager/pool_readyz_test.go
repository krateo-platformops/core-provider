package manager

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// #653: /readyz is the PROCESS's. Not ready until the index is listed and the members it named then have
// each synced once, or failed once, or memberSyncWait has passed; then latched. A member failing,
// draining, joining or leaving after that does not touch it.
func TestPoolReadyz_ProcessLevelAndLatched(t *testing.T) {
	var clock atomic.Int64
	clock.Store(time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC).UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()) }
	advance := func(d time.Duration) { clock.Add(int64(d)) }

	t.Run("not ready before the index is listed, whatever runs", func(t *testing.T) {
		f := newFakeRun()
		p, _ := drainPool(t, f, map[string]string{"a": "1"})
		if err := p.ReadyzCheck(nil); err == nil || !strings.Contains(err.Error(), "not been listed") {
			t.Errorf("ReadyzCheck = %v", err)
		}
	})
	t.Run("an empty index is ready at once", func(t *testing.T) {
		p, _ := newDrainPool(t, newFakeRun(), nil)
		p.indexListed()
		if err := p.ReadyzCheck(nil); err != nil {
			t.Errorf("ReadyzCheck = %v", err)
		}
	})
	t.Run("a first member that fails counts as done: it is reported on its own definition", func(t *testing.T) {
		f := newFakeRun()
		f.fail["bad"], f.failFast["bad"] = errors.New("no configuration"), true
		p, set := newDrainPool(t, f, func(int) time.Duration { return time.Hour })
		set(map[string]string{"bad": "1", "good": "1"})
		p.now = now
		p.indexListed()
		soon(t, "bad failed", func() bool { return f.saw("stop bad=1") })
		// good runs in a fake with no controller to sync: it is what keeps the process waiting.
		if err := p.ReadyzCheck(nil); err == nil || !strings.Contains(err.Error(), "good") || strings.Contains(err.Error(), "bad") {
			t.Fatalf("ReadyzCheck = %v, want only good waited for", err)
		}
		advance(memberSyncWait - time.Second)
		if p.ReadyzCheck(nil) == nil {
			t.Fatal("ready before memberSyncWait with a first member not synced")
		}
		advance(time.Second)
		if err := p.ReadyzCheck(nil); err != nil {
			t.Fatalf("after memberSyncWait: %v", err)
		}
		// Latched: everything after is the members' own.
		set(map[string]string{"bad": "2"})
		set(map[string]string{"late": "1"})
		if err := p.ReadyzCheck(nil); err != nil {
			t.Errorf("after the latch, members changing made the process not ready: %v", err)
		}
		if p.membersReady(nil) == nil {
			t.Error("control: the members' own account was to say a member is not ready")
		}
	})
	t.Run("a first member that leaves before it runs is not waited for", func(t *testing.T) {
		f := newFakeRun()
		p, set := drainPool(t, f, map[string]string{"gone": "1"})
		p.indexListed()
		if p.ReadyzCheck(nil) == nil {
			t.Fatal("control: gone, running and not synced, was to be waited for")
		}
		set(map[string]string{})
		if err := p.ReadyzCheck(nil); err != nil {
			t.Errorf("ReadyzCheck = %v after the only first member left", err)
		}
	})
}

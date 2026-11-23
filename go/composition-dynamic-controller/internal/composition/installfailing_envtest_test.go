//go:build envtest

package composition

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/krateo-platformops/composition-dynamic-controller/internal/conditionreason"
	"github.com/krateo-platformops/unstructured-runtime/pkg/controller/builder"
	"github.com/krateo-platformops/unstructured-runtime/pkg/workqueue"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// observed636 is one state of the composition the apiserver served.
type observed636 struct {
	status, reason, message string
	failed, retries         int64
}

// watch636 records every state of the composition, in order: a poll would miss what flaps.
func (a *arm636) watch636(ctx context.Context) func() []observed636 {
	a.t.Helper()
	w, err := a.e.dyn.Resource(gvr636).Namespace(a.ns).Watch(ctx, metav1.ListOptions{})
	if err != nil {
		a.t.Fatal(err)
	}
	var mu sync.Mutex
	var seen []observed636
	go func() {
		for ev := range w.ResultChan() {
			u, ok := ev.Object.(*unstructured.Unstructured)
			if !ok {
				continue
			}
			r := readyOf636(u)
			retries, _ := installRecoveryCount(u)
			o := observed636{string(r.Status), r.Reason, r.Message, installFailureCount(u), retries}
			mu.Lock()
			if len(seen) == 0 || seen[len(seen)-1] != o {
				seen = append(seen, o)
			}
			mu.Unlock()
		}
	}()
	return func() []observed636 {
		mu.Lock()
		defer mu.Unlock()
		return append([]observed636(nil), seen...)
	}
}

// run636 runs the REAL runtime over the arm's handler, as main.go does, until stop is called.
func (a *arm636) run636() (stop func()) {
	a.t.Helper()
	c, err := builder.Build(a.e.ctx, builder.Configuration{Config: a.e.cfg, GVR: gvr636, ProviderName: "composition-dynamic-controller"},
		builder.WithNamespace(a.ns), builder.WithPluralizer(p597Pluralizer{}), builder.WithResyncInterval(time.Hour), builder.WithMaxRetries(1000),
		builder.WithGracefulShutdownTimeout(10*time.Second),
		builder.WithGlobalRateLimiter(workqueue.NewExponentialTimedFailureRateLimiter[any](200*time.Millisecond, 500*time.Millisecond)))
	if err != nil {
		a.t.Fatal(err)
	}
	c.SetExternalClient(a.h)
	ctx, cancel := context.WithCancel(a.e.ctx)
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx, 1) }()
	return func() { cancel(); <-done }
}

func (a *arm636) sample(what string) {
	a.t.Helper()
	st := runtime.DeepCopyJSONValue(a.get().Object["status"]).(map[string]any)
	b, _ := json.MarshalIndent(map[string]any{"conditions": st["conditions"], "installFailures": st["installFailures"], "installRecovery": st["installRecovery"], "helmOperation": st["helmOperation"]}, "", "  ")
	a.t.Logf("SAMPLE %s: %s", what, b)
}

// Ready=False/InstallFailing on the LIVE object, through the real unstructured-runtime: a first install whose
// hook never passes reads Creating for two failures and InstallFailing from the third; from then on EVERY
// state the apiserver served says InstallFailing, through the retries, a restart of the controller and an
// interrupted retry, and names both stored counts; the last error can be read while a retry runs; and once
// the spec is fixed it is Available, with the retries still recorded.
func TestEnvtest_636_InstallFailingThroughTheRuntime(t *testing.T) {
	e := start636(t)
	podName, podNamespace = "cdc-1", "no-such-namespace" // a starter that is gone is found gone
	t.Cleanup(func() { podName, podNamespace = "", "" })
	a := e.arm(t, "failing", getter636{ann: map[string]string{}})
	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	if _, err := e.dyn.Resource(gvr636).Namespace(a.ns).Create(e.ctx, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": gvr636.GroupVersion().String(), "kind": "Firework",
		"metadata": map[string]any{"name": "fw", "namespace": a.ns},
		"spec":     map[string]any{"k": "v1", "hookns": "no-such-namespace", "both": true},
	}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	seen := a.watch636(ctx)
	failedAtLeast := func(n int64) func() bool { return func() bool { return installFailureCount(a.get()) >= n } }

	stop := a.run636()
	waitUntil(t, "four failed attempts", 120*time.Second, failedAtLeast(4))
	stop() // between attempts, or in one: either way the next controller goes on from what is stored
	before := installFailureCount(a.get())
	a.sample("between attempts, the controller stopped")
	podName = "cdc-2"
	stop = a.run636()
	waitUntil(t, "another failed attempt after the restart", 120*time.Second, failedAtLeast(before+1))
	waitUntil(t, "a retry in flight", 60*time.Second, func() bool {
		r := readyOf636(a.get())
		if strings.Contains(r.Message, "a retry is running") {
			a.sample("while retrying")
			return true
		}
		return false
	})
	a.edit(a.fixSpec)
	waitUntil(t, "Available", 120*time.Second, func() bool { return a.ready() == "True/Available" })
	stop()
	a.sample("after recovery")

	states := seen()
	var firstFailing, last int
	counts := map[string]int{}
	for i, o := range states {
		counts[o.status+"/"+o.reason]++
		if o.reason == conditionreason.ReasonInstallFailing {
			if firstFailing == 0 {
				firstFailing = i
			}
			last = i
			want := fmt.Sprintf("the first install has failed %d times (%d automatic retries so far); last error: ", o.failed, o.retries)
			if !strings.Contains(o.message, want) || o.failed < installFailingAfter {
				t.Errorf("state %d: message %q does not name the stored counts (%d, %d)", i, o.message, o.failed, o.retries)
			}
		}
	}
	t.Logf("%d states: %v; first InstallFailing at %d (failed %d), last at %d", len(states), counts, firstFailing, states[firstFailing].failed, last)
	if firstFailing == 0 || states[firstFailing].failed != installFailingAfter {
		t.Fatalf("InstallFailing first seen with %d failed attempts", states[firstFailing].failed)
	}
	running, sawCreating := 0, false
	for i, o := range states {
		if o.status == "True" && o.failed > 0 && i < last {
			t.Errorf("state %d: Ready True while the install was failing", i)
		}
		if i < firstFailing {
			sawCreating = sawCreating || (o.reason == "Creating" && o.failed == 2)
			continue
		}
		if i <= last && o.reason != conditionreason.ReasonInstallFailing {
			t.Errorf("state %d: Ready flapped to %s/%s (%q)", i, o.status, o.reason, o.message)
		}
		if strings.Contains(o.message, "a retry is running") && strings.Contains(o.message, "last error: ") && strings.Contains(o.message, "hook-2") == false && !strings.Contains(o.message, "hook2.yaml") {
			t.Errorf("state %d: a retry runs and the last error is not readable: %q", i, o.message)
		} else if strings.Contains(o.message, "a retry is running") {
			running++
		}
	}
	if !sawCreating || running == 0 {
		t.Errorf("control: Creating seen at the second failure %v, states while a retry ran %d", sawCreating, running)
	}
	if u := a.get(); installFailureCount(u) != 0 || a.recovery() == "" {
		t.Errorf("after recovery: failed %d installRecovery %q", installFailureCount(u), a.recovery())
	}
}

// A failed CREATE ends as the Ready this handler RETURNS, not the Creating the runtime writes on the object it
// re-fetches after the call: here the install fails in helm before it stores a revision, so it is Create that
// fails each time. Control: one failure earlier, the runtime's Creating stands.
func TestEnvtest_636_AFailedCreateEndsInstallFailing(t *testing.T) {
	e := start636(t)
	for _, tc := range []struct {
		ns   string
		had  int64
		want string
	}{{"create-third", installFailingAfter - 1, "InstallFailing"}, {"create-second", installFailingAfter - 2, "Creating"}} {
		t.Run(tc.ns, func(t *testing.T) {
			a := e.arm(t, tc.ns, getter636{url: "http://127.0.0.1:9/nothing.tgz", ann: map[string]string{}})
			u, err := e.dyn.Resource(gvr636).Namespace(a.ns).Create(e.ctx, &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": gvr636.GroupVersion().String(), "kind": "Firework",
				"metadata": map[string]any{"name": "fw", "namespace": a.ns}, "spec": map[string]any{"k": "v1", "hookns": a.ns},
			}}, metav1.CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}
			_ = unstructured.SetNestedMap(u.Object, map[string]any{"count": tc.had, "lastAt": "2026-11-23T21:00:00Z", "lastError": "x"}, "status", "installFailures")
			if _, err := e.dyn.Resource(gvr636).Namespace(a.ns).UpdateStatus(e.ctx, u, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			stop := a.run636()
			// The runtime's own Synced=ReconcileError is the witness that its writes after the call are done.
			waitUntil(t, "the runtime to record the failed create", 60*time.Second, func() bool {
				synced := false
				conds, _, _ := unstructured.NestedSlice(a.get().Object, "status", "conditions")
				for _, c := range conds {
					if m, ok := c.(map[string]any); ok && m["type"] == "Synced" && m["reason"] == "ReconcileError" {
						synced = true
					}
				}
				return synced && installFailureCount(a.get()) == tc.had+1
			})
			stop()
			if r := readyOf636(a.get()); string(r.Status)+"/"+r.Reason != "False/"+tc.want || !strings.Contains(r.Message, "127.0.0.1:9") {
				t.Errorf("Ready %s/%s %q, want False/%s with helm's error", r.Status, r.Reason, r.Message, tc.want)
			}
		})
	}
}

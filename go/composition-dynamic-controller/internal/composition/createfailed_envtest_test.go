//go:build envtest

package composition

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	compositionMeta "github.com/krateo-platformops/composition-dynamic-controller/pkg/meta"
	"github.com/krateo-platformops/unstructured-runtime/pkg/controller"
	unstructuredtools "github.com/krateo-platformops/unstructured-runtime/pkg/tools/unstructured"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// inspector646 is a chart-inspector that refuses the render, or answers it, as told.
type inspector646 struct {
	url    string
	refuse atomic.Bool
	calls  atomic.Int32
}

const refusal646 = "mcp.audience is required when hermesClient.enabled"

func newInspector646(t *testing.T, e *env636) *inspector646 {
	t.Helper()
	body, valid := templateRefusedBody(t, refusal646), e.inspector
	i := &inspector646{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i.calls.Add(1)
		if !i.refuse.Load() {
			http.Redirect(w, r, valid, http.StatusTemporaryRedirect)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	i.url = srv.URL
	i.refuse.Store(true)
	return i
}

func (a *arm636) create646(status map[string]any) *unstructured.Unstructured {
	a.t.Helper()
	u, err := a.e.dyn.Resource(gvr636).Namespace(a.ns).Create(a.e.ctx, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": gvr636.GroupVersion().String(), "kind": "Firework",
		"metadata": map[string]any{"name": "fw", "namespace": a.ns}, "spec": map[string]any{"k": "v1", "hookns": a.ns},
	}}, metav1.CreateOptions{})
	if err != nil {
		a.t.Fatal(err)
	}
	if status == nil {
		return u
	}
	u.Object["status"] = status
	if u, err = a.e.dyn.Resource(gvr636).Namespace(a.ns).UpdateStatus(a.e.ctx, u, metav1.UpdateOptions{}); err != nil {
		a.t.Fatal(err)
	}
	return u
}

func (a *arm636) has646(typ, statusReason string) bool {
	c := condOf(a.get(), typ)
	return c != nil && string(c.Status)+"/"+c.Reason == statusReason
}

func (a *arm636) conds646() string {
	b, _ := json.Marshal(conds646(unstructuredtools.GetConditions(a.get())))
	return string(b)
}

// A first render the chart refuses, through the REAL unstructured-runtime: the composition carries the
// conditions and the Ready an installed one carries for the same refusal, on every state the apiserver
// served, with nothing counted as an install attempt; a valid answer then clears them. The second half IS
// that installed composition: the same refusal once it runs, read off Observe's own write.
func TestEnvtest_646_AFirstRenderRefusedReadsAsOnAnInstalledComposition(t *testing.T) {
	e := start636(t)
	a := e.arm(t, "refused", getter636{ann: map[string]string{}})
	i := newInspector646(t, e)
	a.h.chartInspectorUrl = i.url
	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	w, err := e.dyn.Resource(gvr636).Namespace(a.ns).Watch(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	refusedStates := make(chan map[string]int, 1)
	stopWatching := make(chan struct{})
	go func() {
		states := map[string]int{}
		for {
			select {
			case ev := <-w.ResultChan():
				if u, ok := ev.Object.(*unstructured.Unstructured); ok {
					if r := condOf(u, "Ready"); r != nil {
						states[string(r.Status)+"/"+r.Reason]++
					}
				}
			case <-stopWatching:
				refusedStates <- states
				return
			}
		}
	}()
	a.create646(nil)

	assertRefused := func(when string) {
		t.Helper()
		for typ, want := range map[string]string{
			"ChartRenderable": "False/TemplateRefused", "RBACComplete": "Unknown/NotEvaluated",
			"Ready": "False/Unavailable", "Synced": "False/ReconcileError",
		} {
			if !a.has646(typ, want) {
				t.Errorf("%s: %s is %+v, want %s", when, typ, condOf(a.get(), typ), want)
			}
		}
		for _, typ := range []string{"ChartRenderable", "Ready", "Synced"} {
			if c := condOf(a.get(), typ); c != nil && !strings.Contains(c.Message, refusal646) {
				t.Errorf("%s: %s message %q does not carry the refusal", when, typ, c.Message)
			}
		}
		for _, f := range []string{"installFailures", "installRecovery", "helmOperation"} {
			if v, found, _ := unstructured.NestedFieldNoCopy(a.get().Object, "status", f); found {
				t.Errorf("%s: a refused render is not an install attempt, and status.%s is %v", when, f, v)
			}
		}
	}

	stop := a.run636()
	// Several passes: what a later one overwrites is found. Synced is the witness of a finished one.
	waitUntil(t, "four refused passes", 60*time.Second, func() bool { return i.calls.Load() >= 4 && a.has646("Synced", "False/ReconcileError") })
	first := condOf(a.get(), "Ready")
	n := i.calls.Load()
	waitUntil(t, "two more", 60*time.Second, func() bool { return i.calls.Load() >= n+2 })
	stop()
	close(stopWatching)
	t.Logf("SAMPLE a first render refused, after %d renders: %s", i.calls.Load(), a.conds646())
	assertRefused("a first render refused")
	states := <-refusedStates
	t.Logf("Ready in every state served while it was refused: %v", states)
	if len(states) != 1 || states["False/Unavailable"] == 0 {
		t.Errorf("Ready flapped, or was never Unavailable: %v", states)
	}
	if last := condOf(a.get(), "Ready"); first != nil && last != nil && !first.LastTransitionTime.Equal(&last.LastTransitionTime) {
		t.Errorf("Ready's transition moved between passes: %s, %s", first.LastTransitionTime, last.LastTransitionTime)
	}
	if got := a.history(); got != "" {
		t.Errorf("a release was stored: %s", got)
	}
	onCreate := conds646(conditionsOn(a.get(), "ChartRenderable", "RBACComplete", "Ready"))

	i.refuse.Store(false)
	stop = a.run636()
	waitUntil(t, "the install", 120*time.Second, func() bool {
		return a.has646("Synced", "True/ReconcileSuccess") && a.history() == "1:deployed"
	})
	stop()
	t.Logf("SAMPLE after a valid answer, installed by Create: %s", a.conds646())
	if !a.has646("ChartRenderable", "True/Renderable") || !a.has646("RBACComplete", "True/Applied") || !a.has646("Ready", "False/Creating") {
		t.Errorf("not recovered: %s", a.conds646())
	}
	stop = a.run636() // the resync is an hour: a new controller observes what it finds
	waitUntil(t, "Available", 120*time.Second, func() bool { return a.ready() == "True/Available" && a.has646("Synced", "True/ReconcileSuccess") })
	stop()
	t.Logf("SAMPLE observed: %s", a.conds646())
	if !a.has646("ChartRenderable", "True/Renderable") || !a.has646("RBACComplete", "True/Applied") {
		t.Errorf("not recovered: %s", a.conds646())
	}

	i.refuse.Store(true)
	n = i.calls.Load()
	stop = a.run636()
	waitUntil(t, "the installed composition's refusal", 60*time.Second, func() bool { return i.calls.Load() >= n+2 && a.has646("Synced", "False/ReconcileError") })
	stop()
	assertRefused("installed")
	onObserve := conds646(conditionsOn(a.get(), "ChartRenderable", "RBACComplete", "Ready"))
	if len(onCreate) != 3 || len(onObserve) != 3 {
		t.Errorf("create %v, observe %v", onCreate, onObserve)
	}
	for typ, want := range onObserve {
		if onCreate[typ] != want {
			t.Errorf("%s differs:\n first render: %s\n installed:    %s", typ, onCreate[typ], want)
		}
	}
}

// A refused render between failed install attempts: Ready names the refusal, as on Observe, and the attempts
// already counted are neither added to nor forgotten. CONTROL: TestEnvtest_636_AFailedCreateEndsInstallFailing,
// the same stored count with an install that does run and fails.
func TestEnvtest_646_ARefusedRenderIsNotAnInstallFailure(t *testing.T) {
	e := start636(t)
	a := e.arm(t, "counted", getter636{ann: map[string]string{}})
	i := newInspector646(t, e)
	a.h.chartInspectorUrl = i.url
	a.create646(map[string]any{"installFailures": map[string]any{"count": int64(installFailingAfter - 1), "lastAt": "2026-11-23T21:00:00Z", "lastError": "x"}})
	stop := a.run636()
	waitUntil(t, "refused passes", 60*time.Second, func() bool { return i.calls.Load() >= 4 && a.has646("Synced", "False/ReconcileError") })
	stop()
	if !a.has646("Ready", "False/Unavailable") || installFailureCount(a.get()) != installFailingAfter-1 {
		t.Errorf("Ready %s, %d failed attempts counted", a.ready(), installFailureCount(a.get()))
	}
}

// The handler's own answer at the two ends of a create: a failure AFTER the render carries that the chart
// renders, so a refusal an earlier pass recorded does not outlive it, and no Ready; a refused render on a
// composition that is being deleted carries nothing.
func TestEnvtest_646_WhatCreateReturns(t *testing.T) {
	e := start636(t)
	refused := map[string]any{"conditions": []any{map[string]any{"type": "ChartRenderable", "status": "False", "reason": "TemplateRefused", "message": "m", "lastTransitionTime": "2026-11-24T10:00:00Z"}}}

	t.Run("after the render", func(t *testing.T) {
		a := e.arm(t, "after", getter636{ann: map[string]string{}})
		a.create646(refused)
		u := a.edit(func(u *unstructured.Unstructured) { compositionMeta.SetReleaseName(u, "fw-corrupt") })
		if _, err := e.kube.CoreV1().Secrets(a.ns).Create(e.ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "sh.helm.release.v1.fw-corrupt.v1", Labels: map[string]string{"owner": "helm", "name": "fw-corrupt", "status": "deployed", "version": "1"}},
			Type:       "helm.sh/release.v1", Data: map[string][]byte{"release": []byte("not a release")},
		}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		res, err := a.h.CreateWithResult(e.ctx, u)
		if err == nil || !strings.Contains(err.Error(), "finding helm release") {
			t.Fatalf("premise: it must fail after the render and before helm: %v", err)
		}
		got := conds646(res.Conditions)
		t.Logf("err %v\nreturned %v", err, got)
		if len(got) != 2 || !strings.HasPrefix(got["ChartRenderable"], "ChartRenderable=True/Renderable") || !strings.HasPrefix(got["RBACComplete"], "RBACComplete=True/Applied") {
			t.Errorf("returned %v", got)
		}
		if !a.has646("ChartRenderable", "False/TemplateRefused") {
			t.Error("premise: the stored condition is the earlier refusal; only what is returned can clear it")
		}
		if !strings.Contains(err.Error(), controller.ErrCreateNotCommitted.Error()) {
			t.Errorf("no longer classified as not committed: %v", err)
		}
	})

	t.Run("while being deleted", func(t *testing.T) {
		a := e.arm(t, "deleting", getter636{ann: map[string]string{}})
		a.h.chartInspectorUrl = newInspector646(t, e).url
		a.create646(nil)
		u := a.edit(func(u *unstructured.Unstructured) { u.SetFinalizers([]string{"test.krateo.io/hold"}) })
		res, err := a.h.CreateWithResult(e.ctx, u)
		if err == nil || len(res.Conditions) != 3 {
			t.Fatalf("control, not deleted: err %v, returned %v", err, conds646(res.Conditions))
		}
		if err := e.dyn.Resource(gvr636).Namespace(a.ns).Delete(e.ctx, "fw", metav1.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
		res, err = a.h.CreateWithResult(e.ctx, a.get())
		if err == nil || !strings.Contains(err.Error(), refusal646) || len(res.Conditions) != 0 {
			t.Errorf("err %v, returned %v", err, conds646(res.Conditions))
		}
	})
}

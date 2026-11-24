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
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
)

// inspector646 is a chart-inspector that fails as told, or answers.
type inspector646 struct {
	url    string
	failed atomic.Pointer[failure646] // nil: it answers
	calls  atomic.Int32
}

type failure646 struct {
	code int
	body string
}

const (
	refusal646  = "mcp.audience is required when hermesClient.enabled"
	unserved646 = `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"no matches for kind \"Widget\" in version \"example.com/v1\"","reason":"Invalid","code":422}`
)

func refused646(t *testing.T) *failure646 {
	return &failure646{http.StatusBadRequest, templateRefusedBody(t, refusal646)}
}

func (a *arm636) inspector646(first *failure646) *inspector646 {
	a.t.Helper()
	i, valid := &inspector646{}, a.e.inspector
	i.failed.Store(first)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i.calls.Add(1)
		f := i.failed.Load()
		if f == nil {
			http.Redirect(w, r, valid, http.StatusTemporaryRedirect)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.code)
		_, _ = w.Write([]byte(f.body))
	}))
	a.t.Cleanup(srv.Close)
	i.url = srv.URL
	a.h.chartInspectorUrl = i.url
	return i
}

// readyStates646 counts the Ready of every state the apiserver serves until it is called.
func (a *arm636) readyStates646() func() map[string]int {
	a.t.Helper()
	ctx, cancel := context.WithCancel(a.e.ctx)
	w, err := a.e.dyn.Resource(gvr636).Namespace(a.ns).Watch(ctx, metav1.ListOptions{})
	if err != nil {
		cancel()
		a.t.Fatal(err)
	}
	stop, out := make(chan struct{}), make(chan map[string]int, 1)
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
			case <-stop:
				out <- states
				return
			}
		}
	}()
	return func() map[string]int { close(stop); s := <-out; cancel(); return s }
}

// failedPasses646 waits until a pass that failed was recorded, and n more renders were asked for.
func (a *arm636) failedPasses646(i *inspector646, n int32) {
	a.t.Helper()
	had := i.calls.Load()
	waitUntil(a.t, "failed passes", 60*time.Second, func() bool {
		return i.calls.Load() >= had+n && a.has646("Synced", "False/ReconcileError")
	})
}

func (a *arm636) assert646(when string, want map[string]string) {
	a.t.Helper()
	for typ, w := range want {
		if !a.has646(typ, w) {
			a.t.Errorf("%s: %s is %+v, want %s", when, typ, condOf(a.get(), typ), w)
		}
	}
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
	i := a.inspector646(refused646(t))
	readyStates := a.readyStates646()
	a.create646(nil)

	assertRefused := func(when string) {
		t.Helper()
		a.assert646(when, map[string]string{"ChartRenderable": "False/TemplateRefused", "RBACComplete": "Unknown/NotEvaluated",
			"Ready": "False/Unavailable", "Synced": "False/ReconcileError"})
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
	a.failedPasses646(i, 4)
	first := condOf(a.get(), "Ready")
	a.failedPasses646(i, 2)
	stop()
	t.Logf("SAMPLE a first render refused, after %d renders: %s", i.calls.Load(), a.conds646())
	assertRefused("a first render refused")
	states := readyStates()
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

	i.failed.Store(nil)
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

	i.failed.Store(refused646(t))
	stop = a.run636()
	a.failedPasses646(i, 2)
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
	i := a.inspector646(refused646(t))
	a.create646(map[string]any{"installFailures": map[string]any{"count": int64(installFailingAfter - 1), "lastAt": "2026-11-23T21:00:00Z", "lastError": "x"}})
	stop := a.run636()
	a.failedPasses646(i, 4)
	stop()
	if !a.has646("Ready", "False/Unavailable") || installFailureCount(a.get()) != installFailingAfter-1 {
		t.Errorf("Ready %s, %d failed attempts counted", a.ready(), installFailureCount(a.get()))
	}
}

// What is not a verdict stays converging on a first create: a kind this cluster does not serve YET, as in an
// umbrella that installs its CRDs meanwhile. ChartRenderable says why; Ready is Creating in every state served;
// once the kind is served it heals by itself.
func TestEnvtest_646_AnUnservedKindStaysCreatingAndHeals(t *testing.T) {
	e := start636(t)
	a := e.arm(t, "unserved", getter636{ann: map[string]string{}})
	i := a.inspector646(&failure646{http.StatusUnprocessableEntity, unserved646})
	readyStates := a.readyStates646()
	a.create646(nil)
	stop := a.run636()
	a.failedPasses646(i, 4)
	t.Logf("SAMPLE an unserved kind: %s", a.conds646())
	a.assert646("an unserved kind", map[string]string{"ChartRenderable": "False/UnservedKind", "RBACComplete": "Unknown/NotEvaluated",
		"Ready": "False/Creating", "Synced": "False/ReconcileError"})
	if r := condOf(a.get(), "Ready"); r == nil || !strings.Contains(r.Message, "no matches for kind") {
		t.Errorf("Ready does not say why: %+v", r)
	}
	if states := readyStates(); len(states) != 1 || states["False/Creating"] == 0 {
		t.Errorf("Ready in every state served: %v", states)
	}
	i.failed.Store(nil) // the CRD arrives
	waitUntil(t, "the install", 120*time.Second, func() bool {
		return a.has646("Synced", "True/ReconcileSuccess") && a.history() == "1:deployed"
	})
	a.assert646("healed", map[string]string{"ChartRenderable": "True/Renderable", "RBACComplete": "True/Applied"})
	stop()
	stop = a.run636() // the resync is an hour: a new controller observes what it finds
	waitUntil(t, "Available", 120*time.Second, func() bool { return a.ready() == "True/Available" })
	stop()
}

// RBAC the API server REFUSES, for real: the handler acts as an identity that may write RBAC and may not grant
// what it does not hold, so the derived ClusterRole is an escalation. The chart renders, the RBAC is refused,
// and that is a verdict. Granted what was missing, it heals.
func TestEnvtest_646_RefusedRBACIsAVerdict(t *testing.T) {
	e := start636(t)
	a := e.arm(t, "escalation", getter636{ann: map[string]string{}})
	i := a.inspector646(nil)
	rules := []rbacv1.PolicyRule{
		{APIGroups: []string{"composition.krateo.io"}, Resources: []string{"*"}, Verbs: []string{"*"}},
		{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"*"}},
		{APIGroups: []string{"rbac.authorization.k8s.io"}, Resources: []string{"*"}, Verbs: []string{"get", "list", "create", "update", "patch", "delete"}},
	}
	if _, err := e.kube.RbacV1().ClusterRoles().Create(e.ctx, &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "cdc-646"}, Rules: rules}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	bind := func(role string) {
		t.Helper()
		if _, err := e.kube.RbacV1().ClusterRoleBindings().Create(e.ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "cdc-646-" + role},
			RoleRef: rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: role}, Subjects: []rbacv1.Subject{{Kind: "User", Name: "cdc-646", APIGroup: "rbac.authorization.k8s.io"}}}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	bind("cdc-646")
	a.h.kubeconfig = rest.CopyConfig(e.cfg)
	a.h.kubeconfig.Impersonate = rest.ImpersonationConfig{UserName: "cdc-646"}
	readyStates := a.readyStates646()
	a.create646(nil)
	stop := a.run636()
	a.failedPasses646(i, 4)
	t.Logf("SAMPLE an escalation refused: %s", a.conds646())
	a.assert646("refused", map[string]string{"ChartRenderable": "True/Renderable", "RBACComplete": "False/ApplyFailed",
		"Ready": "False/Unavailable", "Synced": "False/ReconcileError"})
	if r := condOf(a.get(), "Ready"); r == nil || !strings.Contains(r.Message, "attempting to grant RBAC permissions not currently held") {
		t.Errorf("premise: an escalation: %+v", r)
	}
	if states := readyStates(); len(states) != 1 || states["False/Unavailable"] == 0 {
		t.Errorf("Ready in every state served: %v", states)
	}
	bind("cluster-admin")
	waitUntil(t, "the install", 120*time.Second, func() bool {
		return a.has646("Synced", "True/ReconcileSuccess") && a.history() == "1:deployed"
	})
	a.assert646("healed", map[string]string{"ChartRenderable": "True/Renderable", "RBACComplete": "True/Applied"})
	stop()
}

// A first install that keeps failing does not read converging because the next retry met something that is not
// a verdict: InstallFailing is kept in every state served, through a failing chart-inspector.
func TestEnvtest_646_InstallFailingIsKeptOverANonVerdict(t *testing.T) {
	e := start636(t)
	a := e.arm(t, "kept", getter636{ann: map[string]string{}})
	i := a.inspector646(&failure646{http.StatusInternalServerError, `{"message":"boom"}`})
	a.create646(map[string]any{
		"installFailures": map[string]any{"count": int64(installFailingAfter), "lastAt": "2026-11-23T21:00:00Z", "lastError": "hook-2 failed"},
		"conditions":      []any{map[string]any{"type": "Ready", "status": "False", "reason": "InstallFailing", "message": "the first install has failed 3 times", "lastTransitionTime": "2026-11-24T10:00:00Z"}},
	})
	readyStates := a.readyStates646()
	stop := a.run636()
	a.failedPasses646(i, 4)
	stop()
	t.Logf("SAMPLE a non-verdict over InstallFailing: %s", a.conds646())
	a.assert646("kept", map[string]string{"ChartRenderable": "False/InspectorUnavailable", "RBACComplete": "Unknown/NotEvaluated",
		"Ready": "False/InstallFailing", "Synced": "False/ReconcileError"})
	if states := readyStates(); len(states) != 1 || states["False/InstallFailing"] == 0 {
		t.Errorf("Ready in every state served: %v", states)
	}
	r := condOf(a.get(), "Ready")
	if r == nil || !strings.Contains(r.Message, "last error: hook-2 failed; the next retry did not start: ") || r.LastTransitionTime.UTC().Format(time.RFC3339) != "2026-11-24T10:00:00Z" {
		t.Errorf("Ready %+v", r)
	}
	if n := installFailureCount(a.get()); n != installFailingAfter {
		t.Errorf("%d failed attempts counted", n)
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
		a.inspector646(refused646(t))
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

//go:build envtest

package composition

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	compositionMeta "github.com/krateo-platformops/composition-dynamic-controller/pkg/meta"
	helmconfig "github.com/krateo-platformops/plumbing/helm"
	helmv3 "github.com/krateo-platformops/plumbing/helm/v3"
	xcontext "github.com/krateo-platformops/unstructured-runtime/pkg/context"
	"github.com/krateo-platformops/unstructured-runtime/pkg/logging"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// core-provider#640 through the handler's own Observe, against a real apiserver and real helm storage:
// a release whose latest revision FAILED is healed against what that revision records as applied
// (plumbing v1.110.0), and the composition's log says so.
//
// This file uses nothing plumbing v1.110.0 added, so it compiles against v1.109.0 too: the 640 arms
// (pre-upgrade hook, Job template) are the ones that must FAIL there, which is the control.
//
// chart636 plus: b, a ConfigMap no value moves; new, only when k is v2; a pre-upgrade hook and a
// post-upgrade hook in the namespace prens / postns names (one that does not exist fails it, as hook-2
// fails an install); work, a plain Job (no hook) whose command is jobv, when job is set.
var chart640 = map[string]string{
	"b.yaml":        "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: b\n  namespace: {{ .Release.Namespace }}\ndata:\n  k: b\n",
	"new.yaml":      "{{- if eq .Values.k \"v2\" }}\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: new\n  namespace: {{ .Release.Namespace }}\ndata:\n  k: new\n{{- end }}\n",
	"prehook.yaml":  hookCM636("pre-hook", "pre-upgrade", 1, "{{ .Values.prens | default .Release.Namespace }}"),
	"posthook.yaml": hookCM636("post-hook", "post-upgrade", 5, "{{ .Values.postns | default .Release.Namespace }}"),
	"job.yaml": "{{- if .Values.job }}\napiVersion: batch/v1\nkind: Job\nmetadata:\n  name: work\n  namespace: {{ .Release.Namespace }}\nspec:\n  template:\n    spec:\n" +
		"      restartPolicy: Never\n      containers:\n      - name: c\n        image: busybox\n        command: [\"echo\", {{ .Values.jobv | default \"a\" | quote }}]\n{{- end }}\n",
}

// lines640 is the composition's log at Info and Warn, safe for concurrent writers.
type lines640 struct {
	mu    sync.Mutex
	lines []string
}

type logger640 struct{ l *lines640 }

func (c logger640) add(level, msg string, kv ...any) {
	parts := []string{level, msg}
	for i := 0; i+1 < len(kv); i += 2 {
		parts = append(parts, fmt.Sprintf("%v=%v", kv[i], kv[i+1]))
	}
	c.l.mu.Lock()
	c.l.lines = append(c.l.lines, strings.Join(parts, " "))
	c.l.mu.Unlock()
}
func (c logger640) Info(msg string, kv ...any) { c.add("INFO", msg, kv...) }
func (c logger640) Warn(msg string, kv ...any) { c.add("WARN", msg, kv...) }
func (c logger640) Debug(string, ...any)       {}
func (c logger640) Error(err error, msg string, kv ...any) {
	c.add("ERROR", msg, append(kv, "err", err)...)
}
func (c logger640) WithValues(...any) logging.Logger { return c }
func (c logger640) WithName(string) logging.Logger   { return c }

// matching is every line containing s, and how many.
func (l *lines640) matching(s string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, line := range l.lines {
		if strings.Contains(line, s) {
			out = append(out, line)
		}
	}
	return out
}

func (l *lines640) reset() { l.mu.Lock(); l.lines = nil; l.mu.Unlock() }

type arm640 struct {
	*arm636
	log *lines640
	ctx context.Context
}

func (e *env636) arm640(t *testing.T, ns string) *arm640 {
	t.Helper()
	a := e.arm(t, ns, getter636{url: serveChart636(t, "0.1.0", chart640), ann: map[string]string{}})
	l := &lines640{}
	ctx := xcontext.BuildContext(e.ctx, xcontext.WithLogger(logger640{l}))
	xcontext.Logger(ctx).Info("calibration") // the capture reaches what the handler reads, or every arm below is vacuous
	if len(l.matching("calibration")) != 1 {
		t.Fatal("control: the logger context does not reach this test's capture")
	}
	l.reset()
	return &arm640{arm636: a, log: l, ctx: ctx}
}

// observe640 is one Observe, with this arm's log, and the Update it would dispatch on drift.
func (a *arm640) observe640() error {
	a.t.Helper()
	obs, err := a.h.Observe(a.ctx, a.get())
	a.t.Logf("observe: %+v err=%v; history [%s]", obs, err, a.history())
	if err == nil && !obs.ResourceUpToDate {
		err = a.h.Update(a.ctx, a.get())
		a.t.Logf("update: err=%v; history [%s]", err, a.history())
	}
	return err
}

func (a *arm640) data(name string) string {
	if c := a.cmGet(name); c != nil {
		return c.Data["k"]
	}
	return "<absent>"
}

func (a *arm640) editCM(name, k string) {
	a.t.Helper()
	c := a.cmGet(name)
	c.Data["k"] = k
	if _, err := a.e.kube.CoreV1().ConfigMaps(a.ns).Update(a.e.ctx, c, metav1.UpdateOptions{}); err != nil {
		a.t.Fatal(err)
	}
}

func (a *arm640) deleteCM(name string) {
	a.t.Helper()
	if err := a.e.kube.CoreV1().ConfigMaps(a.ns).Delete(a.e.ctx, name, metav1.DeleteOptions{}); err != nil {
		a.t.Fatal(err)
	}
}

// failUpgrade installs the composition and moves its spec to spec, an upgrade that must fail: the
// latest revision is then 2, failed.
func (a *arm640) failUpgrade(spec map[string]any) {
	a.t.Helper()
	a.firstInstall(false)
	a.edit(func(u *unstructured.Unstructured) { u.Object["spec"] = spec })
	if err := a.observe640(); err == nil {
		a.t.Fatal("control: the upgrade must fail")
	}
	if h := a.history(); h != "1:deployed 2:failed" {
		a.t.Fatalf("control: history %q after the failed upgrade", h)
	}
}

const (
	healedLine640   = "Composition's failed release healed against what its failed revision applied"
	noRecordLine640 = "Composition's failed release cannot be healed: its failed revision carries no record of what was applied"
)

// A pre-upgrade hook that keeps failing: the next pass restores a child deleted and a field edited to
// what was last APPLIED (revision 1's objects), not to the failed render, and runs no upgrade to do it:
// the post-upgrade witness stays absent and the release stays failed. The log names the heal.
func TestEnvtest_640_PreUpgradeHookFailingHealsToWhatWasApplied(t *testing.T) {
	e := start636(t)
	a := e.arm640(t, "pre640")
	a.failUpgrade(map[string]any{"k": "v2", "hookns": a.ns, "both": true, "prens": "no-such-namespace"})
	if a.data("app") != "v1" || a.cm("new") || a.cm("upgrade-ran") {
		t.Fatalf("control: the failed pre-upgrade hook applied something: app %s new %v upgrade-ran %v", a.data("app"), a.cm("new"), a.cm("upgrade-ran"))
	}
	a.deleteCM("app")
	a.editCM("b", "edited")
	a.log.reset()

	err := a.observe640()
	if err == nil {
		t.Fatal("control: the pre-upgrade hook still fails, so the retry must")
	}
	// What was applied, not the render's v2.
	if got := a.data("app"); got != "v1" {
		t.Errorf("app is %s, want v1 restored (revision 1's applied object; the failed render says v2)", got)
	}
	if got := a.data("b"); got != "b" {
		t.Errorf("b is %s, want the edit reverted to b", got)
	}
	if a.cm("new") {
		t.Error("new exists: the heal applied the failed render")
	}
	if a.cm("upgrade-ran") || a.cm("pre-hook") {
		t.Errorf("upgrade-ran %v pre-hook %v: no upgrade got past its pre-upgrade hook", a.cm("upgrade-ran"), a.cm("pre-hook"))
	}
	if h := strings.Fields(a.history()); !strings.HasSuffix(h[len(h)-1], ":failed") {
		t.Errorf("history %v: the release must stay failed", h)
	}
	healed := a.log.matching(healedLine640)
	t.Logf("SAMPLE %v", healed)
	if len(healed) != 1 || !strings.Contains(healed[0], "revision=2") || !strings.Contains(healed[0], "recorded=true") ||
		!strings.Contains(healed[0], "created=1") || !strings.Contains(healed[0], "patched=1") ||
		!strings.Contains(healed[0], "/app") || !strings.Contains(healed[0], "/b") {
		t.Errorf("the heal line: %v, want one, naming revision 2, recorded, created=1 (app) patched=1 (b)", healed)
	}
	if n := len(a.log.matching(noRecordLine640)); n != 0 {
		t.Errorf("%d no-record lines for a recorded revision", n)
	}

	// Steady: nothing drifted, nothing healed, and nothing said.
	a.log.reset()
	_ = a.observe640()
	if l := a.log.matching(healedLine640); len(l) != 0 {
		t.Errorf("a pass with no drift logged a heal: %v", l)
	}
}

// The same heal reached by Update, which the runtime dispatches on drift: it reconciles too, so it says
// the heal as Observe does, on its failed outcome.
func TestEnvtest_640_UpdateSaysTheHealToo(t *testing.T) {
	e := start636(t)
	a := e.arm640(t, "upd640")
	a.failUpgrade(map[string]any{"k": "v2", "hookns": a.ns, "both": true, "prens": "no-such-namespace"})
	a.deleteCM("app")
	a.log.reset()
	if err := a.h.Update(a.ctx, a.get()); err == nil {
		t.Fatal("control: the pre-upgrade hook still fails, so the retry must")
	}
	if got := a.data("app"); got != "v1" {
		t.Errorf("app is %s, want v1 restored", got)
	}
	healed := a.log.matching(healedLine640)
	t.Logf("SAMPLE %v", healed)
	if len(healed) != 1 || !strings.Contains(healed[0], "revision=2") || !strings.Contains(healed[0], "created=1") || !strings.Contains(healed[0], "/app") {
		t.Errorf("the heal line from Update: %v, want one, naming revision 2, created=1 (app)", healed)
	}
}

// CONTROL for the arm above: a post-upgrade hook that fails, after the objects step. The heal restores
// the NEW render, which that attempt applied.
func TestEnvtest_640_PostUpgradeHookFailingHealsToTheNewRender(t *testing.T) {
	e := start636(t)
	a := e.arm640(t, "post640")
	a.failUpgrade(map[string]any{"k": "v2", "hookns": a.ns, "both": true, "postns": "no-such-namespace"})
	if a.data("app") != "v2" || !a.cm("new") || !a.cm("upgrade-ran") {
		t.Fatalf("control: the objects step must have run: app %s new %v upgrade-ran %v", a.data("app"), a.cm("new"), a.cm("upgrade-ran"))
	}
	a.deleteCM("new")
	a.editCM("app", "edited")
	a.log.reset()
	if err := a.observe640(); err == nil {
		t.Fatal("control: the post-upgrade hook still fails, so the retry must")
	}
	if a.data("app") != "v2" || a.data("new") != "new" {
		t.Errorf("app %s new %s, want the new render restored (v2, new)", a.data("app"), a.data("new"))
	}
	healed := a.log.matching(healedLine640)
	t.Logf("SAMPLE %v", healed)
	if len(healed) != 1 || !strings.Contains(healed[0], "revision=2") || !strings.Contains(healed[0], "created=1") ||
		!strings.Contains(healed[0], "patched=1") || !strings.Contains(healed[0], "/new") {
		t.Errorf("the heal line: %v, want one, naming revision 2, created=1 (new) patched=1 (app)", healed)
	}
}

// An upgrade that failed on a live Job's immutable template (helm's own upgrade, as a person or an older
// controller runs it), with the composition asking for that same state: the retry cannot be applied,
// so it is refused naming the Job, over three passes, with no new revision (plumbing v1.109.0 wrote a
// failed revision on every pass), and krateo.io/hooks-unverified, which only an upgrade that ran its
// hooks clears, stays. Synced, through the real runtime, names the Job.
func TestEnvtest_640_JobTemplateChangedOnAFailedRelease(t *testing.T) {
	e := start636(t)
	a := e.arm640(t, "job640")
	a.firstInstall(false)
	spec := map[string]any{"k": "v1", "hookns": a.ns, "both": true, "job": true}
	a.edit(func(u *unstructured.Unstructured) { u.Object["spec"] = spec })
	if err := a.observe640(); err != nil {
		t.Fatalf("control: adding the Job: %v", err)
	}
	if a.jobUID("work") == "" || !strings.HasSuffix(a.history(), ":deployed") {
		t.Fatalf("control: Job %q, history %s", a.jobUID("work"), a.history())
	}

	// The failed revision: helm's upgrade to the composition's next values, which patches the Job.
	name := a.relName()
	first, err := a.storage().Last(name)
	if err != nil {
		t.Fatal(err)
	}
	vals := runtime.DeepCopyJSON(first.Config)
	vals["jobv"] = "b"
	hc, err := helmv3.NewClient(e.cfg, helmv3.WithNamespace(a.ns))
	if err != nil {
		t.Fatal(err)
	}
	_, uerr := hc.Upgrade(e.ctx, name, a.h.packageInfoGetter.(getter636).url, &helmconfig.UpgradeConfig{MaxHistory: 3,
		ActionConfig: &helmconfig.ActionConfig{Values: vals}})
	hc.Close()
	if uerr == nil || !strings.Contains(uerr.Error(), "immutable") {
		t.Fatalf("control: the upgrade must fail on the Job's template: %v", uerr)
	}
	spec["jobv"] = "b"
	a.edit(func(u *unstructured.Unstructured) { u.Object["spec"] = spec })
	a.setAnnotation(compositionMeta.AnnotationKeyHooksUnverified, "2026-11-25T00:00:00Z")
	failed := a.history()
	if !strings.HasSuffix(failed, ":failed") {
		t.Fatalf("control: history %s", failed)
	}

	job := a.ns + "/work"
	for pass := 1; pass <= 3; pass++ {
		err := a.observe640()
		if err == nil || !errors.Is(err, helmconfig.ErrJobTemplateChanged) || !strings.Contains(err.Error(), job) {
			t.Errorf("pass %d: err %v, want ErrJobTemplateChanged naming %s", pass, err, job)
		}
		if h := a.history(); h != failed {
			t.Errorf("pass %d: history %s, was %s: a release that cannot be applied writes no revision", pass, h, failed)
		}
		if a.annotation(compositionMeta.AnnotationKeyHooksUnverified) == "" {
			t.Fatalf("pass %d: hooks-unverified cleared, and no upgrade ran its hooks", pass)
		}
	}

	i := a.inspector646(nil)
	stop := a.run636()
	a.failedPasses646(i, 2)
	stop()
	s := condOf(a.get(), "Synced")
	t.Logf("SAMPLE Synced %+v; history %s", s, a.history())
	if s == nil || s.Status != metav1.ConditionFalse || !strings.Contains(s.Message, job) {
		t.Errorf("Synced %+v, want False naming %s", s, job)
	}
	if h := a.history(); h != failed {
		t.Errorf("through the runtime: history %s, was %s", h, failed)
	}
	if a.annotation(compositionMeta.AnnotationKeyHooksUnverified) == "" {
		t.Error("through the runtime: hooks-unverified cleared")
	}
}

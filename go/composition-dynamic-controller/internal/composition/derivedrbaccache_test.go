package composition

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/krateo-platformops/composition-dynamic-controller/internal/tools/archive"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// #697. The cache of chart-inspector's answer, against a chart-inspector double that COUNTS calls.

// inspectorDouble answers /resources and counts how many times it was asked.
type inspectorDouble struct {
	srv   *httptest.Server
	calls atomic.Int32
	fail  atomic.Bool
	body  atomic.Value // string
}

const resourcesBodyA = `[{"group":"","version":"v1","resource":"configmaps","verbs":["*"]},` +
	`{"group":"","version":"v1","resource":"secrets","namespace":"ns1","name":"s","verbs":["*"]}]`
const resourcesBodyB = `[{"group":"apps","version":"v1","resource":"deployments","verbs":["*"]}]`

func newInspectorDouble(t *testing.T) *inspectorDouble {
	t.Helper()
	d := &inspectorDouble{}
	d.body.Store(resourcesBodyA)
	d.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		d.calls.Add(1)
		if d.fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"boom","code":500}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(d.body.Load().(string)))
	}))
	t.Cleanup(d.srv.Close)
	return d
}

func rbacFake() *dynamicfake.FakeDynamicClient {
	g := func(r string) schema.GroupVersionResource {
		return schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: r}
	}
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		g("clusterroles"):                       "ClusterRoleList",
		g("clusterrolebindings"):                "ClusterRoleBindingList",
		g("roles"):                              "RoleList",
		g("rolebindings"):                       "RoleBindingList",
		{Version: "v1", Resource: "namespaces"}: "NamespaceList",
	})
}

type rbacPass struct {
	t        *testing.T
	h        *handler
	dyn      *dynamicfake.FakeDynamicClient
	mg       *unstructured.Unstructured
	pkg      *archive.Info
	revision int
	release  string
}

func newRBACPass(t *testing.T, d *inspectorDouble) *rbacPass {
	t.Helper()
	mg := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"replicas": int64(1)},
	}}
	mg.SetName("fw")
	mg.SetNamespace("krateo-system")
	mg.SetUID(types.UID("uid-1"))
	mg.SetGroupVersionKind(schema.GroupVersionKind{Group: "composition.krateo.io", Version: "v1-0-0", Kind: "Firework"})
	return &rbacPass{
		t: t,
		h: &handler{
			pluralizer: stubPluralizerForRBAC{}, chartInspectorUrl: d.srv.URL, saName: "cdc", saNamespace: "krateo-system",
			derivedRBAC: newDerivedRBACCache(time.Hour, nil),
		},
		dyn: rbacFake(),
		mg:  mg,
		pkg: &archive.Info{
			URL: "oci://example/fw", Version: "1.0.0", DefinitionVersion: "1.0.0", Repo: "fw",
			Auth: &archive.Auth{Username: "u", Password: "p"},
			CompositionDefinitionInfo: &archive.CompositionDefinitionInfo{
				Name: "fireworksapp", Namespace: "krateo-system", Generation: 3, UID: "cd-uid",
				GVR: schema.GroupVersionResource{Group: "core.krateo.io", Version: "v1alpha1", Resource: "compositiondefinitions"},
			},
		},
		revision: 1,
		release:  "fw-abcdefgh",
	}
}

// observe is Observe's call: the cache is in play.
func (p *rbacPass) observe() error {
	p.t.Helper()
	_, err := p.h.applyDerivedRBACWith(context.Background(), p.mg, p.pkg, p.dyn, p.release, &rbacCacheUse{revision: p.revision})
	return err
}

func (p *rbacPass) mustObserve() {
	p.t.Helper()
	if err := p.observe(); err != nil {
		p.t.Fatalf("pass failed: %v", err)
	}
}

var (
	clusterRoleGVR = schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}
	roleGVR        = schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}
)

func (p *rbacPass) exists(gvr schema.GroupVersionResource, ns string) bool {
	p.t.Helper()
	_, err := p.dyn.Resource(gvr).Namespace(ns).Get(context.Background(), p.release+"-krateo-rbac", metav1.GetOptions{})
	return err == nil
}

func (p *rbacPass) clusterRoleResources() string {
	p.t.Helper()
	u, err := p.dyn.Resource(clusterRoleGVR).Get(context.Background(), p.release+"-krateo-rbac", metav1.GetOptions{})
	if err != nil {
		return "<absent>"
	}
	rules, _, _ := unstructured.NestedSlice(u.Object, "rules")
	var out []string
	for _, r := range rules {
		res, _, _ := unstructured.NestedStringSlice(r.(map[string]any), "resources")
		out = append(out, strings.Join(res, ","))
	}
	return strings.Join(out, ";")
}

func TestDerivedRBACCache_TwoIdenticalPassesAskChartInspectorOnce(t *testing.T) {
	d := newInspectorDouble(t)
	p := newRBACPass(t, d)

	p.mustObserve()
	p.mustObserve()
	p.mustObserve()

	if got := d.calls.Load(); got != 1 {
		t.Fatalf("chart-inspector was asked %d times over three identical passes, want 1", got)
	}
	if !p.exists(clusterRoleGVR, "") || !p.exists(roleGVR, "ns1") {
		t.Fatal("the derived ClusterRole/Role were not applied: the skipped passes must still apply")
	}
}

// Each input of the key, changed ALONE, must cost a call: a key too narrow grants the wrong RBAC.
func TestDerivedRBACCache_EachKeyInputChangedAloneAsksAgain(t *testing.T) {
	cases := []struct {
		name   string
		change func(p *rbacPass)
	}{
		{"chart version (what this controller renders)", func(p *rbacPass) { p.pkg.Version = "1.0.1" }},
		{"definition's current chart version", func(p *rbacPass) { p.pkg.DefinitionVersion = "1.0.1" }},
		{"chart url", func(p *rbacPass) { p.pkg.URL = "oci://example/other" }},
		{"chart repo", func(p *rbacPass) { p.pkg.Repo = "other" }},
		{"TLS verification", func(p *rbacPass) { p.pkg.InsecureSkipTLSverify = true }},
		{"registry password", func(p *rbacPass) { p.pkg.Auth.Password = "rotated" }},
		{"values (a spec field)", func(p *rbacPass) { _ = unstructured.SetNestedField(p.mg.Object, int64(2), "spec", "replicas") }},
		{"values (a new spec field)", func(p *rbacPass) { _ = unstructured.SetNestedField(p.mg.Object, "x", "spec", "extra") }},
		{"values (the paused annotation, which InjectGlobalValues renders)", func(p *rbacPass) {
			p.mg.SetAnnotations(map[string]string{"krateo.io/gracefully-paused": "true"})
		}},
		{"helm release revision", func(p *rbacPass) { p.revision = 2 }},
		{"release name", func(p *rbacPass) { p.release = "fw-other" }},
		{"definition generation", func(p *rbacPass) { p.pkg.CompositionDefinitionInfo.Generation = 4 }},
		{"definition UID (recreated definition)", func(p *rbacPass) { p.pkg.CompositionDefinitionInfo.UID = "cd-uid-2" }},
		{"definition identity", func(p *rbacPass) { p.pkg.CompositionDefinitionInfo.Name = "other" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newInspectorDouble(t)
			p := newRBACPass(t, d)
			p.mustObserve()
			p.mustObserve()
			if got := d.calls.Load(); got != 1 {
				t.Fatalf("control: %d calls over two identical passes, want 1", got)
			}
			tc.change(p)
			p.mustObserve()
			if got := d.calls.Load(); got != 2 {
				t.Fatalf("after changing %s alone: %d calls in all, want 2 (the change must be asked about)", tc.name, got)
			}
			p.mustObserve()
			if got := d.calls.Load(); got != 2 {
				t.Fatalf("the pass after the change: %d calls in all, want 2 (the new answer is cached under the new key)", got)
			}
		})
	}
}

// A changed key takes the NEW answer, not the cached one.
func TestDerivedRBACCache_ChangedKeyAppliesTheNewAnswer(t *testing.T) {
	d := newInspectorDouble(t)
	p := newRBACPass(t, d)
	p.mustObserve()
	if got := p.clusterRoleResources(); got != "configmaps" {
		t.Fatalf("first answer applied as %q", got)
	}
	d.body.Store(resourcesBodyB)
	p.mustObserve() // key unchanged: still the cached answer
	if got := p.clusterRoleResources(); got != "configmaps" {
		t.Fatalf("an unchanged key must keep applying the cached answer, got %q", got)
	}
	p.pkg.Version = "2.0.0"
	p.mustObserve()
	if got := p.clusterRoleResources(); !strings.Contains(got, "deployments") {
		t.Fatalf("a changed key must apply the new answer, got %q", got)
	}
}

// The point of re-applying the cached rules: a grant someone deleted comes back with no call.
func TestDerivedRBACCache_DeletedGrantIsRestoredWithoutACall(t *testing.T) {
	d := newInspectorDouble(t)
	p := newRBACPass(t, d)
	p.mustObserve()

	name := p.release + "-krateo-rbac"
	if err := p.dyn.Resource(clusterRoleGVR).Delete(context.Background(), name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := p.dyn.Resource(roleGVR).Namespace("ns1").Delete(context.Background(), name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if p.exists(clusterRoleGVR, "") || p.exists(roleGVR, "ns1") {
		t.Fatal("setup: the grants are still there")
	}

	p.mustObserve()
	if !p.exists(clusterRoleGVR, "") {
		t.Error("a deleted ClusterRole was not restored on the next pass")
	}
	if !p.exists(roleGVR, "ns1") {
		t.Error("a deleted Role was not restored on the next pass")
	}
	if got := d.calls.Load(); got != 1 {
		t.Errorf("restoring cost a chart-inspector call: %d calls in all, want 1", got)
	}
}

func TestDerivedRBACCache_FreshHandlerAsksAgain(t *testing.T) {
	d := newInspectorDouble(t)
	p := newRBACPass(t, d)
	p.mustObserve()
	p.mustObserve()
	// A restart: same inputs, nothing remembered.
	p.h.derivedRBAC = newDerivedRBACCache(time.Hour, nil)
	p.mustObserve()
	p.mustObserve()
	if got := d.calls.Load(); got != 2 {
		t.Fatalf("%d calls across a restart, want 2 (one per process)", got)
	}
}

// chart-inspector failing after a key change is today's behaviour: the error comes back, nothing stale
// is applied, and what was remembered is dropped.
func TestDerivedRBACCache_FailureAfterAKeyChangeIsTodaysBehaviour(t *testing.T) {
	d := newInspectorDouble(t)
	p := newRBACPass(t, d)
	p.mustObserve()
	before := p.clusterRoleResources()

	d.fail.Store(true)
	p.revision = 2
	err := p.observe()
	if err == nil {
		t.Fatal("a changed key with chart-inspector failing returned nil: the cached rules were applied for a key they were not computed under")
	}
	if !strings.Contains(err.Error(), "generating RBAC using chart-inspector") {
		t.Errorf("error %q is not the one applyDerivedRBAC has always returned", err)
	}
	if got := p.clusterRoleResources(); got != before {
		t.Errorf("the failed pass changed the applied rules: %q -> %q", before, got)
	}
	if got := d.calls.Load(); got != 2 {
		t.Fatalf("%d calls, want 2", got)
	}

	// The old key is not honoured again after the failure: what was kept was dropped.
	d.fail.Store(false)
	p.revision = 1
	p.mustObserve()
	if got := d.calls.Load(); got != 3 {
		t.Errorf("%d calls after returning to the first key, want 3: the entry survived a failed recomputation", got)
	}
}

// With the key unchanged there is no call, so a chart-inspector outage costs nothing.
func TestDerivedRBACCache_UnchangedKeyNeedsNoChartInspector(t *testing.T) {
	d := newInspectorDouble(t)
	p := newRBACPass(t, d)
	p.mustObserve()
	d.fail.Store(true)
	p.mustObserve()
	if got := d.calls.Load(); got != 1 {
		t.Fatalf("%d calls, want 1", got)
	}
}

func TestDerivedRBACCache_CompositionsNeverShareAnEntry(t *testing.T) {
	d := newInspectorDouble(t)
	a := newRBACPass(t, d)
	b := newRBACPass(t, d)
	b.h = a.h // one process, one cache, two compositions

	// identical inputs but another UID
	b.mg.SetUID("uid-2")
	// identical inputs but another kind (a pooled process)
	c := newRBACPass(t, d)
	c.h = a.h
	c.mg.SetGroupVersionKind(schema.GroupVersionKind{Group: "composition.krateo.io", Version: "v1-0-0", Kind: "Sparkler"})
	c.mg.SetUID("uid-3")

	a.mustObserve()
	b.mustObserve()
	c.mustObserve()
	if got := d.calls.Load(); got != 3 {
		t.Fatalf("%d calls for three compositions, want 3: one answered for another", got)
	}
	a.mustObserve()
	b.mustObserve()
	c.mustObserve()
	if got := d.calls.Load(); got != 3 {
		t.Fatalf("%d calls after a second round, want 3: each has its own entry", got)
	}
	if a.h.derivedRBAC.size() != 3 {
		t.Errorf("%d entries, want 3", a.h.derivedRBAC.size())
	}
	// same UID, but another kind: not a hit either (the UID alone does not identify the answer)
	d2 := newRBACPass(t, d)
	d2.h = a.h
	d2.mg.SetGroupVersionKind(schema.GroupVersionKind{Group: "composition.krateo.io", Version: "v1-0-0", Kind: "Sparkler"})
	d2.mg.SetUID("uid-1")
	d2.mustObserve()
	if got := d.calls.Load(); got != 4 {
		t.Errorf("%d calls, want 4: a composition of another kind with a known UID read that UID's entry", got)
	}
}

func TestDerivedRBACCache_DeleteForgetsTheComposition(t *testing.T) {
	d := newInspectorDouble(t)
	p := newRBACPass(t, d)
	p.mustObserve()
	if p.h.derivedRBAC.size() != 1 {
		t.Fatal("nothing cached")
	}
	p.h.derivedRBAC.forget(p.mg.GetUID())
	if p.h.derivedRBAC.size() != 0 {
		t.Fatal("forget kept the entry")
	}
	p.mustObserve()
	if got := d.calls.Load(); got != 2 {
		t.Errorf("%d calls, want 2", got)
	}
}

func TestDerivedRBACCache_EntriesExpire(t *testing.T) {
	d := newInspectorDouble(t)
	p := newRBACPass(t, d)
	now := time.Now()
	p.h.derivedRBAC = newDerivedRBACCache(10*time.Minute, func() time.Time { return now })

	p.mustObserve()
	now = now.Add(9 * time.Minute)
	p.mustObserve()
	if got := d.calls.Load(); got != 1 {
		t.Fatalf("%d calls inside the max age, want 1", got)
	}
	now = now.Add(2 * time.Minute)
	p.mustObserve()
	if got := d.calls.Load(); got != 2 {
		t.Fatalf("%d calls past the max age, want 2: lookup state is only bounded by this", got)
	}
}

func TestDerivedRBACCache_OffMeansEveryPassAsks(t *testing.T) {
	for name, c := range map[string]*derivedRBACCache{"nil": nil, "max age 0": newDerivedRBACCache(0, nil)} {
		t.Run(name, func(t *testing.T) {
			d := newInspectorDouble(t)
			p := newRBACPass(t, d)
			p.h.derivedRBAC = c
			p.mustObserve()
			p.mustObserve()
			if got := d.calls.Load(); got != 2 {
				t.Fatalf("%d calls with the cache off, want 2", got)
			}
		})
	}
}

// Create and the pre-teardown refresh call applyDerivedRBAC, which never uses the cache.
func TestDerivedRBACCache_OnlyObserveUsesIt(t *testing.T) {
	d := newInspectorDouble(t)
	p := newRBACPass(t, d)
	for i := 0; i < 2; i++ {
		if _, err := p.h.applyDerivedRBAC(context.Background(), p.mg, p.pkg, p.dyn, p.release); err != nil {
			t.Fatal(err)
		}
	}
	if got := d.calls.Load(); got != 2 {
		t.Fatalf("%d calls from applyDerivedRBAC, want 2", got)
	}
	if p.h.derivedRBAC.size() != 0 {
		t.Fatal("applyDerivedRBAC stored an entry")
	}
}

// Cached rules are handed out as a copy, so an apply cannot corrupt what the next pass applies.
func TestDerivedRBACCache_LookupReturnsACopy(t *testing.T) {
	d := newInspectorDouble(t)
	p := newRBACPass(t, d)
	p.mustObserve()
	key, ok := p.h.derivedRBACKeyFor(p.mg, p.pkg, p.release, p.revision)
	if !ok {
		t.Fatal("no key")
	}
	r1, hit := p.h.derivedRBAC.lookup(p.mg.GetUID(), key)
	if !hit {
		t.Fatal("miss")
	}
	r1.ClusterRole.Rules = nil
	r1.ClusterRole.Labels["x"] = "y"
	r2, _ := p.h.derivedRBAC.lookup(p.mg.GetUID(), key)
	if len(r2.ClusterRole.Rules) == 0 || r2.ClusterRole.Labels["x"] != "" {
		t.Fatal("mutating one lookup's result changed the cached rules")
	}
}

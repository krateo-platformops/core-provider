//go:build envtest

package compositiondefinitions

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	rtv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	"github.com/krateo-platformops/provider-runtime/pkg/reconciler"
	"github.com/krateo-platformops/provider-runtime/pkg/resource"
	"github.com/krateo-platformops/provider-runtime/pkg/resource/fake"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// core-provider#644 on a real apiserver, through provider-runtime's own reconciler and the real
// connector: status.observedGeneration is the generation the Ready beside it was determined from.
//
// Every claim is read from what the apiserver SERVED, through a watch that sees each persisted version
// of the definition and not only the last: a reader polls at any moment, so a false "processed" that
// lasts one write is still false.

var cdGVR644 = schema.GroupVersionResource{Group: "core.krateo.io", Version: "v1alpha1", Resource: "compositiondefinitions"}

// charts644 serves charts by path; a path it does not have is a 404, an unreadable chart.
type charts644 struct {
	srv *httptest.Server
	mu  sync.Mutex
	tgz map[string][]byte
}

func (s *charts644) url(name, version string) string {
	return s.srv.URL + "/" + name + "-" + version + ".tgz"
}

const noSchema644 = "\x00none"

// add serves chart name at version with valuesSchema (noSchema644: no values.schema.json at all).
func (s *charts644) add(t *testing.T, name, version, valuesSchema string) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for f, body := range map[string]string{
		"Chart.yaml":         "apiVersion: v2\nname: " + name + "\nversion: " + version + "\n",
		"values.yaml":        "{}\n",
		"values.schema.json": valuesSchema,
		"templates/t.yaml":   twoConfigMaps,
	} {
		if body == noSchema644 {
			continue
		}
		if err := tw.WriteHeader(&tar.Header{Name: name + "/" + f, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := errors.Join(tw.Close(), gz.Close()); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tgz["/"+name+"-"+version+".tgz"] = buf.Bytes()
}

// reader644 is the live reader Observe re-reads the definition through. after runs once, right after
// the next read of a definition: an edit made there lands between the read and the pass's own writes.
type reader644 struct {
	client.Reader
	mu    sync.Mutex
	after func()
	fail  error
}

func (r *reader644) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	r.mu.Lock()
	after, fail := r.after, r.fail
	if fail == nil {
		r.after = nil
	}
	r.mu.Unlock()
	if fail != nil {
		return fail
	}
	err := r.Reader.Get(ctx, key, obj, opts...)
	if after != nil {
		after()
	}
	return err
}

// kube644 is the manager's client: lagging644, and a CRD's conditions as hook leaves them.
type kube644 struct {
	*lagging644
	mu  sync.Mutex
	crd func(*apiextensionsv1.CustomResourceDefinition)
}

func (k *kube644) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	err := k.lagging644.Get(ctx, key, obj, opts...)
	k.mu.Lock()
	hook := k.crd
	k.mu.Unlock()
	if crd, ok := obj.(*apiextensionsv1.CustomResourceDefinition); ok && err == nil && hook != nil {
		hook(crd)
	}
	return err
}

func (k *kube644) onCRD(f func(*apiextensionsv1.CustomResourceDefinition)) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.crd = f
}

// pluralizer644 answers from the apiserver's discovery, as the real one does in a cluster.
type pluralizer644 struct{ d discovery.DiscoveryInterface }

func (p pluralizer644) GVKtoGVR(gvk schema.GroupVersionKind) (schema.GroupVersionResource, error) {
	l, err := p.d.ServerResourcesForGroupVersion(gvk.GroupVersion().String())
	if err == nil {
		for _, r := range l.APIResources {
			if r.Kind == gvk.Kind && !strings.Contains(r.Name, "/") {
				return gvk.GroupVersion().WithResource(r.Name), nil
			}
		}
	} else if !apierrors.IsNotFound(err) {
		return schema.GroupVersionResource{}, err
	}
	return schema.GroupVersionResource{}, apierrors.NewNotFound(schema.GroupResource{Group: gvk.Group, Resource: gvk.Kind}, gvk.Kind)
}

// env644 is one apiserver with core-provider's reconciler on it.
type env644 struct {
	c      *cluster642
	charts *charts644
	kube   *kube644
	reader *reader644
	base   *bench642
}

func start644(t *testing.T) *env644 {
	t.Helper()
	c := start642(t)
	base := newBench642(t, c, c) // the templates, globals and fake authn; its own definition is not used
	if err := c.cl.Delete(context.Background(), base.cd()); err != nil {
		t.Fatal(err)
	}
	e := &env644{c: c, base: base, kube: &kube644{lagging644: &lagging644{Client: c.cl}}, reader: &reader644{Reader: c.cl},
		charts: &charts644{tgz: map[string][]byte{}}}
	e.charts.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.charts.mu.Lock()
		b, ok := e.charts.tgz[r.URL.Path]
		e.charts.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(e.charts.srv.Close)
	return e
}

// reconciler is the production wiring: the runtime's reconciler over the manager's client, and the
// real connector, which builds a new external per pass from the object the runtime read.
func (e *env644) reconciler() *reconciler.Reconciler {
	return reconciler.NewReconciler(&fake.Manager{Client: e.kube, Scheme: e.c.cl.Scheme()},
		resource.ManagedKind(compositiondefinitionsv1alpha1.CompositionDefinitionGroupVersionKind),
		reconciler.WithExternalConnecter(&connector{
			dynamic: e.c.dyn, client: e.c.cs, kube: e.kube, apiReader: e.reader,
			log: logging.NewNopLogger(), pluralizer: pluralizer644{e.c.cs.Discovery()},
		}),
		reconciler.WithCreationGracePeriod(0),
	)
}

// cd644 is one definition of chart name.
type cd644 struct {
	*bench642
	e    *env644
	name string
}

// define creates a definition of the chart name at 1.0.0, mutated by edit before it is created.
func (e *env644) define(t *testing.T, chartName, cdName string, edit func(*compositiondefinitionsv1alpha1.CompositionDefinition)) *cd644 {
	t.Helper()
	cd := &compositiondefinitionsv1alpha1.CompositionDefinition{ObjectMeta: metav1.ObjectMeta{Name: cdName, Namespace: self642CDNS}}
	cd.Spec.Chart = &compositiondefinitionsv1alpha1.ChartInfo{Url: e.charts.url(chartName, "1.0.0"), Version: "1.0.0"}
	if edit != nil {
		edit(cd)
	}
	if err := e.c.cl.Create(context.Background(), cd); err != nil {
		t.Fatal(err)
	}
	b := *e.base
	b.t, b.key, b.r = t, client.ObjectKeyFromObject(cd), e.reconciler()
	return &cd644{bench642: &b, e: e, name: chartName}
}

// settled defines chartName at 1.0.0 and reconciles it to rest.
func (e *env644) settled(t *testing.T, chartName string) *cd644 {
	t.Helper()
	e.charts.add(t, chartName, "1.0.0", `{"type":"object"}`)
	d := e.define(t, chartName, chartName, nil)
	d.rest(6)
	return d
}

func (d *cd644) generations() (gen, observed int64) {
	cd := d.cd()
	return cd.Generation, cd.Status.ObservedGeneration
}

// rest runs passes until Ready=True, Synced=True and observedGeneration is the generation.
func (d *cd644) rest(max int) int {
	d.t.Helper()
	for i := 1; i <= max; i++ {
		_, err := d.pass()
		cd := d.cd()
		if err == nil && cd.GetCondition(rtv1.TypeReady).Status == metav1.ConditionTrue && cd.GetCondition(rtv1.TypeSynced).Status == metav1.ConditionTrue &&
			cd.Status.ObservedGeneration == cd.Generation {
			return i
		}
	}
	gen, obs := d.generations()
	d.t.Fatalf("not at rest after %d passes: generation %d observedGeneration %d Ready=%s (%q) Synced=%s (%q)", max, gen, obs,
		d.cond("Ready"), d.message("Ready"), d.cond("Synced"), d.message("Synced"))
	return 0
}

// edit updates the spec (or metadata) of the live definition, on a fresh read.
func (d *cd644) edit(f func(*compositiondefinitionsv1alpha1.CompositionDefinition)) {
	d.t.Helper()
	cd := d.cd()
	f(cd)
	if err := d.e.c.cl.Update(context.Background(), cd); err != nil {
		d.t.Fatal(err)
	}
}

// bump points the definition at version of its chart, served with valuesSchema.
func (d *cd644) bump(version, valuesSchema string) {
	d.t.Helper()
	d.e.charts.add(d.t, d.name, version, valuesSchema)
	d.repoint(version)
}

// repoint points the definition at version of its chart, whether or not it is served.
func (d *cd644) repoint(version string) {
	d.t.Helper()
	d.edit(func(cd *compositiondefinitionsv1alpha1.CompositionDefinition) {
		cd.Spec.Chart.Url, cd.Spec.Chart.Version = d.e.charts.url(d.name, version), version
	})
}

// want asserts the live generation, observedGeneration and Ready.
func (d *cd644) want(when string, wantGen, wantObs int64, ready string) {
	d.t.Helper()
	gen, obs := d.generations()
	if gen != wantGen || obs != wantObs || d.cond("Ready") != ready {
		d.t.Errorf("%s: generation %d observedGeneration %d Ready=%s (%q) Synced=%s (%q), want %d/%d %s", when, gen, obs,
			d.cond("Ready"), d.message("Ready"), d.cond("Synced"), d.message("Synced"), wantGen, wantObs, ready)
	}
}

// ---- the watch ----

type watch644 struct {
	d   *cd644
	w   watch.Interface
	mu  sync.Mutex
	evs []*unstructured.Unstructured
}

// watch sees every version of d persisted from now on.
func (d *cd644) watch() *watch644 {
	d.t.Helper()
	w, err := d.e.c.raw.Resource(cdGVR644).Namespace(d.key.Namespace).Watch(context.Background(), metav1.ListOptions{
		ResourceVersion: d.cd().ResourceVersion, FieldSelector: "metadata.name=" + d.key.Name})
	if err != nil {
		d.t.Fatal(err)
	}
	out := &watch644{d: d, w: w}
	go func() {
		for ev := range w.ResultChan() {
			if u, ok := ev.Object.(*unstructured.Unstructured); ok && (ev.Type == watch.Added || ev.Type == watch.Modified) {
				out.mu.Lock()
				out.evs = append(out.evs, u)
				out.mu.Unlock()
			}
		}
	}()
	d.t.Cleanup(w.Stop)
	return out
}

// seen644 is what a reader of one served version sees.
type seen644 struct {
	gen, obs             int64
	reported, deleting   bool
	ready, readyReason   string
	synced, apiVersion   string
	wantAPIVersion, spec string
}

// served waits until the watch has delivered the definition's current version, then returns them all.
func (w *watch644) served() []seen644 {
	t := w.d.t
	t.Helper()
	last := ""
	if cd := (&compositiondefinitionsv1alpha1.CompositionDefinition{}); w.d.e.c.cl.Get(context.Background(), w.d.key, cd) == nil {
		last = cd.ResourceVersion
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		w.mu.Lock()
		n := len(w.evs)
		caughtUp := last == "" || (n > 0 && w.evs[n-1].GetResourceVersion() == last)
		w.mu.Unlock()
		if caughtUp {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("instrument: the watch delivered %d versions and never the current one", n)
		}
		time.Sleep(50 * time.Millisecond)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []seen644
	for _, u := range w.evs {
		s := seen644{gen: u.GetGeneration(), deleting: u.GetDeletionTimestamp() != nil}
		s.obs, s.reported, _ = unstructured.NestedInt64(u.Object, "status", "observedGeneration")
		s.apiVersion, _, _ = unstructured.NestedString(u.Object, "status", "apiVersion")
		s.spec, _, _ = unstructured.NestedString(u.Object, "spec", "chart", "version")
		s.wantAPIVersion = "composition.krateo.io/v" + strings.ReplaceAll(s.spec, ".", "-")
		conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
		for _, c := range conds {
			m, _ := c.(map[string]any)
			switch m["type"] {
			case "Ready":
				s.ready, s.readyReason = fmt.Sprint(m["status"]), fmt.Sprint(m["reason"])
			case "Synced":
				s.synced = fmt.Sprint(m["status"])
			}
		}
		out = append(out, s)
	}
	return out
}

// holds asserts, on EVERY served version, what a reader may conclude. It returns how many versions had
// the rule's premise (equal and Ready=True), and how many were Ready=True while behind: the ones a
// reader without the field, or with one written on every status write, gets wrong.
func (w *watch644) holds() (equalAndTrue, behindAndTrue int) {
	t := w.d.t
	t.Helper()
	for i, s := range w.served() {
		if s.obs > s.gen {
			t.Errorf("served version %d: observedGeneration %d is AHEAD of generation %d", i, s.obs, s.gen)
		}
		if s.deleting || s.ready != "True" || !s.reported {
			continue
		}
		if s.obs < s.gen {
			behindAndTrue++
			continue
		}
		equalAndTrue++
		if s.apiVersion != s.wantAPIVersion {
			t.Errorf("served version %d: observedGeneration == generation (%d) and Ready=True/%s, but status.apiVersion is %s while spec.chart is at %s",
				i, s.gen, s.readyReason, s.apiVersion, s.spec)
		}
	}
	return equalAndTrue, behindAndTrue
}

// spokeIsThisCluster creates a KubernetesTarget "spoke" whose kubeconfig is this very apiserver's.
func (e *env644) spokeIsThisCluster(t *testing.T, authnURL string) {
	t.Helper()
	kc, err := clientcmd.Write(clientcmdapi.Config{
		Clusters:  map[string]*clientcmdapi.Cluster{"c": {Server: e.c.cfg.Host, CertificateAuthorityData: e.c.cfg.CAData}},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{"u": {ClientCertificateData: e.c.cfg.CertData, ClientKeyData: e.c.cfg.KeyData, Token: e.c.cfg.BearerToken}},
		Contexts:  map[string]*clientcmdapi.Context{"x": {Cluster: "c", AuthInfo: "u"}}, CurrentContext: "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "spoke", Namespace: self642CDNS}, Data: map[string][]byte{"kubeconfig": kc}}
	if err := e.c.cl.Create(context.Background(), sec); err != nil {
		t.Fatal(err)
	}
	kt := &compositiondefinitionsv1alpha1.KubernetesTarget{ObjectMeta: metav1.ObjectMeta{Name: "spoke", Namespace: self642CDNS}}
	kt.Spec.KubeconfigRef = rtv1.SecretKeySelector{Reference: rtv1.Reference{Name: "spoke", Namespace: self642CDNS}, Key: "kubeconfig"}
	kt.Spec.AuthnURL = authnURL
	if err := e.c.cl.Create(context.Background(), kt); err != nil {
		t.Fatal(err)
	}
}

// ---- tests ----

// A version bump, from what every served version says.
func TestEnvtest_644_VersionBump(t *testing.T) {
	e := start644(t)
	d := e.settled(t, "bumped")
	d.want("at rest at 1.0.0", 1, 1, "True/Available")

	w := d.watch()
	d.bump("2.0.0", `{"type":"object"}`)
	d.want("bumped, not reconciled", 2, 1, "True/Available")
	if _, err := d.pass(); err != nil {
		t.Fatalf("the pass that applies it: %v", err)
	}
	// Update switched the version; its controller has not been observed yet.
	d.want("applied, not observed", 2, 1, "False/Creating")
	n := d.rest(4)
	d.want("at rest at 2.0.0", 2, 2, "True/Available")

	equal, behind := w.holds()
	served := w.served()
	t.Logf("%d served versions, %d equal and Ready=True, %d behind and Ready=True; %d further passes", len(served), equal, behind, n)
	if equal < 1 || behind < 1 || len(served) < 3 {
		t.Errorf("control: %d served versions, %d equal and True, %d behind and True: want the kept Ready=True and the new one both seen", len(served), equal, behind)
	}

	// At rest: it does not churn.
	rv := d.cd().ResourceVersion
	for i := 0; i < 3; i++ {
		if _, err := d.pass(); err != nil {
			t.Fatal(err)
		}
	}
	if got := d.cd().ResourceVersion; got != rv {
		t.Errorf("at rest the definition is still being written: resourceVersion %s -> %s", rv, got)
	}
}

// The add of a new version waits out the grace of a vacated one (#484): Update does nothing and
// succeeds, so the definition keeps Ready=True and Synced=True about the OLD version for minutes. A
// field recorded by every completed Observe would call that processed.
func TestEnvtest_644_AddWaitStaysBehind(t *testing.T) {
	e := start644(t)
	d := e.settled(t, "waiting")
	d.bump("2.0.0", `{"type":"object"}`)
	d.rest(5)
	w := d.watch()
	d.bump("3.0.0", `{"type":"object"}`)
	for i := 1; i <= 3; i++ {
		if _, err := d.pass(); err != nil {
			t.Fatalf("pass %d of the wait: %v", i, err)
		}
		d.want(fmt.Sprintf("waiting, pass %d", i), 3, 2, "True/Available")
		if d.cond("Synced") != "True/ReconcileSuccess" || !strings.HasSuffix(d.cd().Status.ApiVersion, "/v2-0-0") {
			t.Fatalf("premise: the wait keeps Synced=True and serves 2.0.0; Synced=%s apiVersion=%s", d.cond("Synced"), d.cd().Status.ApiVersion)
		}
	}
	if p := d.cd().Status.ServedVersionsPendingPrune; len(p) != 1 {
		t.Fatalf("premise: one vacated version is waited for, got %+v", p)
	}

	// The grace elapses.
	cd := d.cd()
	cd.Status.ServedVersionsPendingPrune[0].EligibleSince = metav1.NewTime(time.Now().Add(-servedVersionPruneGrace - time.Second))
	if err := e.c.cl.Status().Update(context.Background(), cd); err != nil {
		t.Fatal(err)
	}
	d.rest(5)
	d.want("at rest at 3.0.0", 3, 3, "True/Available")
	if equal, behind := w.holds(); equal < 1 || behind < 2 {
		t.Errorf("control: %d equal and True, %d behind and True", equal, behind)
	}
}

// An update that fails after the bump: Ready is kept, then unknown, and the field stays behind for as
// long as it fails.
func TestEnvtest_644_FailingUpdateStaysBehind(t *testing.T) {
	e := start644(t)
	d := e.settled(t, "failing")
	w := d.watch()
	d.bump("2.0.0", `{"type":"object"}`)
	good := CDCtemplateDeploymentPath
	CDCtemplateDeploymentPath = filepath.Join(t.TempDir(), "gone.yaml")
	t.Cleanup(func() { CDCtemplateDeploymentPath = good })
	if _, err := d.pass(); err == nil {
		t.Fatal("premise: the Update must fail")
	}
	d.want("Update failed", 2, 1, "True/Available")
	if d.cond("Synced") != "False/ReconcileError" {
		t.Errorf("Synced = %s, want the error", d.cond("Synced"))
	}
	for i := 0; i < 2; i++ {
		if _, err := d.pass(); err == nil {
			t.Fatal("premise: Observe must fail too, now that the version is served")
		}
		d.want("Observe failing", 2, 1, "Unknown/NotObserved")
	}
	CDCtemplateDeploymentPath = good
	d.rest(5)
	d.want("recovered", 2, 2, "True/Available")
	if equal, behind := w.holds(); equal < 1 || behind < 2 {
		t.Errorf("control: %d equal and True, %d behind and True", equal, behind)
	}
}

const contradicting644 = `{"type":"object","properties":{"security":{"type":"object","required":["authentication"],"properties":{"authentication":{"type":"object","properties":{"enabled":{"type":"boolean"}}}},"default":{}}}}`

// A spec that is REFUSED is determined: Ready says why, about this spec, so the field is equal. Each
// place Observe decides Ready is an arm, and each starts from a definition whose last verdict is about
// an earlier spec (the CONTROL: behind before the pass).
func TestEnvtest_644_RefusalsAreAboutThisSpec(t *testing.T) {
	e := start644(t)
	for _, tc := range []struct {
		name, chart string
		to          func(*cd644)
		ready       string
	}{
		{"the chart cannot be read", "unreadable", func(d *cd644) { d.repoint("9.9.9") }, "False/Unavailable"},
		{"no values.schema.json", "schemaless", func(d *cd644) { d.bump("2.0.0", noSchema644) }, "False/ValuesSchemaMissing"},
		{"a contradicting default", "contradict", func(d *cd644) { d.bump("2.0.0", contradicting644) }, "False/SchemaDefaultContradiction"},
		{"a derived name too long", "derivedname", func(d *cd644) { d.bump("2.0.0-"+strings.Repeat("a", 45), `{"type":"object"}`) }, "False/DerivedNameTooLong"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := e.settled(t, tc.chart)
			w := d.watch()
			tc.to(d)
			d.want("edited, not reconciled", 2, 1, "True/Available")
			_, _ = d.pass()
			d.want("refused", 2, 2, tc.ready)
			// Corrected: the refusal is about the spec before, until the next pass.
			d.bump("3.0.0", `{"type":"object"}`)
			d.want("corrected, not reconciled", 3, 2, tc.ready)
			d.rest(5)
			d.want("at rest", 3, 3, "True/Available")
			w.holds()
		})
	}

	// The CRD is gone and the policy withholds Create: Ready is CreateWithheldByPolicy, about this spec.
	// With Create allowed, Create re-reads the status, so that pass records nothing.
	t.Run("the CRD does not exist", func(t *testing.T) {
		e.charts.add(t, "absent", "1.0.0", `{"type":"object"}`)
		d := e.define(t, "absent", "absent", func(cd *compositiondefinitionsv1alpha1.CompositionDefinition) {
			cd.Annotations = map[string]string{"krateo.io/management-policy": "observe,update"}
		})
		_, _ = d.pass() // adds the finalizer, whose response replaces what Observe put in memory
		_, _ = d.pass()
		d.want("create withheld", 1, 1, "False/CreateWithheldByPolicy")
		d.annotate(map[string]string{"krateo.io/management-policy": ""})
		d.rest(6)
		d.want("created", 1, 1, "True/Available")
	})

	// A new definition whose chart was never readable, Create withheld: the never-reconciled return.
	t.Run("the chart of a new definition cannot be read", func(t *testing.T) {
		d := e.define(t, "never", "never", func(cd *compositiondefinitionsv1alpha1.CompositionDefinition) {
			cd.Annotations = map[string]string{"krateo.io/management-policy": "observe,update"}
		})
		_, _ = d.pass()
		_, _ = d.pass()
		d.want("unreadable, create withheld", 1, 1, "False/CreateWithheldByPolicy")
		// With Create allowed, Create's own re-read drops it: a NEW definition that cannot be read
		// reports Ready=False/Creating and no field. Absent, never ahead.
		d2 := e.define(t, "never2", "never2", nil)
		for i := 0; i < 2; i++ {
			_, _ = d2.pass()
		}
		d2.want("unreadable, create attempted", 1, 0, "False/Creating")
		if u, _ := e.c.raw.Resource(cdGVR644).Namespace(d2.key.Namespace).Get(context.Background(), d2.key.Name, metav1.GetOptions{}); u != nil {
			if _, found, _ := unstructured.NestedFieldNoCopy(u.Object, "status", "observedGeneration"); found {
				t.Error("observedGeneration is served as 0: it must be ABSENT, which a reader tells from behind")
			}
		}
	})

	// The CRD serves the version and is not Established yet.
	t.Run("not established", func(t *testing.T) {
		d := e.settled(t, "establishing")
		e.kube.onCRD(func(crd *apiextensionsv1.CustomResourceDefinition) {
			if strings.HasPrefix(crd.Name, "establishing") {
				crd.Status.Conditions = nil
			}
		})
		defer e.kube.onCRD(nil)
		d.edit(func(cd *compositiondefinitionsv1alpha1.CompositionDefinition) {
			cd.Spec.UpgradePolicy = compositiondefinitionsv1alpha1.UpgradePolicyManual
		})
		d.want("edited, not reconciled", 2, 1, "True/Available")
		if _, err := d.pass(); err != nil {
			t.Fatal(err)
		}
		d.want("not established", 2, 2, "False/Creating")
		e.kube.onCRD(nil)
		d.rest(2)
	})

	// The add of a version that would not fit beside a held one is REFUSED (#494), for as long as it is
	// held: Ready=False/Unavailable about this spec, with Synced=True.
	t.Run("the add is refused", func(t *testing.T) {
		big := string(bigSchema(560000))
		e.charts.add(t, "heldadd", "1.0.0", big)
		d := e.define(t, "heldadd", "heldadd", nil)
		d.rest(6)
		storedHelmRelease(t, context.Background(), e.c.cl, "default", "holder644", 1, "deployed",
			"apiVersion: "+compositionGroup+"/v1-0-0\nkind: "+d.cd().Status.Kind+"\nmetadata:\n  name: child\n")
		d.bump("2.0.0", big)
		d.rest(6)
		d.bump("3.0.0", big)
		d.want("bumped, not reconciled", 3, 2, "True/Available")
		if _, err := d.pass(); err != nil {
			t.Fatal(err)
		}
		d.want("refused", 3, 3, "False/Unavailable")
		if m := d.message("Ready"); !strings.Contains(m, "held by") {
			t.Errorf("premise: Ready must be the refused add, got %q", m)
		}
	})
}

// A change that moves no version: Update applies it, and the Ready kept beside it is about the
// controller before. The next pass re-reads the Deployment.
func TestEnvtest_644_SpecChangeWithoutAVersion(t *testing.T) {
	e := start644(t)
	d := e.settled(t, "tuned")
	w := d.watch()
	digest := d.cd().Status.Digest
	d.edit(func(cd *compositiondefinitionsv1alpha1.CompositionDefinition) {
		n := int32(7)
		cd.Spec.Controller = &compositiondefinitionsv1alpha1.ControllerConfig{Workers: &n}
	})
	if _, err := d.pass(); err != nil {
		t.Fatal(err)
	}
	if d.cd().Status.Digest == digest {
		t.Fatal("premise: Update must have applied it")
	}
	d.want("applied, not observed", 2, 1, "True/Available")
	d.rest(5)
	d.want("at rest", 2, 2, "True/Available")
	if equal, behind := w.holds(); equal < 1 || behind < 2 {
		t.Errorf("control: %d equal and True, %d behind and True", equal, behind)
	}

	// A change nothing renders (the migration policy) is processed by the Observe that sees it.
	d.edit(func(cd *compositiondefinitionsv1alpha1.CompositionDefinition) {
		cd.Spec.UpgradePolicy = compositiondefinitionsv1alpha1.UpgradePolicyPaused
	})
	if _, err := d.pass(); err != nil {
		t.Fatal(err)
	}
	d.want("nothing to apply", 3, 3, "True/Available")
}

// Paused (krateo.io/paused): the runtime writes the status on every pass and nothing is processed.
func TestEnvtest_644_PausedStaysBehind(t *testing.T) {
	e := start644(t)
	d := e.settled(t, "paused")
	d.annotate(map[string]string{"krateo.io/paused": "true"})
	w := d.watch()
	d.bump("2.0.0", `{"type":"object"}`)
	for i := 0; i < 2; i++ {
		if _, err := d.pass(); err != nil {
			t.Fatal(err)
		}
		d.want("paused", 2, 1, "True/Available")
	}
	if d.cond("Synced") != "False/ReconcilePaused" {
		t.Fatalf("premise: the runtime must have written the pause, Synced=%s", d.cond("Synced"))
	}
	d.annotate(map[string]string{"krateo.io/paused": ""})
	d.rest(5)
	d.want("unpaused", 2, 2, "True/Available")
	if _, behind := w.holds(); behind < 2 {
		t.Errorf("control: %d behind and True", behind)
	}
}

// Deleting: the deletion itself moves metadata.generation, and the field stays where it was, whatever
// the teardown's passes write. The chart is unreadable, the one path on which a deleting Observe sets
// Ready through a place that records.
func TestEnvtest_644_FrozenWhileDeleting(t *testing.T) {
	e := start644(t)
	d := e.settled(t, "deleted")
	d.edit(func(cd *compositiondefinitionsv1alpha1.CompositionDefinition) {
		cd.Finalizers = append(cd.Finalizers, "test.krateo.io/hold")
		cd.Spec.Chart.Url = e.charts.url("deleted", "9.9.9")
	})
	w := d.watch()
	if err := e.c.cl.Delete(context.Background(), d.cd()); err != nil {
		t.Fatal(err)
	}
	if gen, obs := d.generations(); gen != 3 || obs != 1 {
		t.Fatalf("premise: the edit and the deletion each move the generation; generation %d observedGeneration %d", gen, obs)
	}
	writes := 0
	for i := 0; i < 4; i++ {
		_, _ = d.pass()
		if d.cond("Ready") == "False/Deleting" {
			writes++
		}
	}
	if writes == 0 {
		t.Fatalf("control: no teardown pass wrote the status; Ready=%s Synced=%s (%q)", d.cond("Ready"), d.cond("Synced"), d.message("Synced"))
	}
	n := 0
	for _, s := range w.served() {
		if s.deleting {
			n++
			if s.obs != 1 {
				t.Errorf("a served version being deleted has observedGeneration %d, want it frozen at 1", s.obs)
			}
		}
	}
	if n == 0 {
		t.Error("control: no served version was being deleted")
	}
	d.edit(func(cd *compositiondefinitionsv1alpha1.CompositionDefinition) { cd.Finalizers = nil })
}

// A spec edited after Observe read it, before the pass wrote its status: the write conflicts and
// nothing of the pass is persisted, so no served version pairs the new generation with the old spec's
// conditions.
func TestEnvtest_644_EditMidPassConflicts(t *testing.T) {
	e := start644(t)
	d := e.settled(t, "conflict")
	w := d.watch()
	d.edit(func(cd *compositiondefinitionsv1alpha1.CompositionDefinition) {
		cd.Spec.UpgradePolicy = compositiondefinitionsv1alpha1.UpgradePolicyManual
	})
	e.charts.add(t, "conflict", "2.0.0", `{"type":"object"}`)
	e.reader.after = func() { d.repoint("2.0.0") }
	_, err := d.pass()
	if err == nil || !apierrors.IsConflict(err) {
		t.Fatalf("premise: the status write must conflict, got %v", err)
	}
	d.want("conflicted", 3, 1, "True/Available")
	d.rest(5)
	d.want("at rest", 3, 3, "True/Available")
	w.holds()
}

// The one path on which the runtime re-reads the object after Observe and still writes its status: an
// unresolved create handshake adopted (UpdateCriticalAnnotations Gets it again). A spec edited in
// between arrives with that read, at its NEWER generation. The pass's conditions were computed from the
// older one, so the field must not take it.
func TestEnvtest_644_ReReadMidPassIsNeverAhead(t *testing.T) {
	e := start644(t)
	d := e.settled(t, "handshake")
	w := d.watch()
	d.annotate(map[string]string{
		"krateo.io/external-create-pending":   time.Now().Add(-time.Hour).Format(time.RFC3339),
		"krateo.io/external-create-succeeded": time.Now().Add(-2 * time.Hour).Format(time.RFC3339),
	})
	e.reader.after = func() {
		d.edit(func(cd *compositiondefinitionsv1alpha1.CompositionDefinition) {
			cd.Spec.UpgradePolicy = compositiondefinitionsv1alpha1.UpgradePolicyManual
		})
	}
	if _, err := d.pass(); err != nil {
		t.Fatalf("premise: the re-read must carry the pass past the edit, got %v", err)
	}
	if got := d.cd().Annotations["krateo.io/external-create-succeeded"]; got <= d.cd().Annotations["krateo.io/external-create-pending"] {
		t.Fatalf("premise: the handshake must have been adopted, which is the write that re-reads; succeeded=%s", got)
	}
	d.want("re-read mid-pass", 2, 1, "True/Available")
	d.rest(3)
	d.want("at rest", 2, 2, "True/Available")
	w.holds()
}

// The generation recorded is the one whose spec was used: Observe's live read, while the cache lags.
// With the cache stuck at the old generation the whole bump is applied and recorded from the live one,
// and a removed field is applied as removed (liveread_644_envtest_test.go).
func TestEnvtest_644_LaggingCache(t *testing.T) {
	e := start644(t)
	d := e.settled(t, "lagging")
	w := d.watch()
	cached := d.cd()
	d.bump("2.0.0", `{"type":"object"}`)
	d.edit(func(cd *compositiondefinitionsv1alpha1.CompositionDefinition) { cd.Spec.UpgradePolicy = "Manual" })
	e.kube.lag(cached)
	for i := 0; i < 4; i++ {
		if _, err := d.pass(); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}
	if e.kube.served < 4 {
		t.Fatalf("control: the lagging cache answered %d reads", e.kube.served)
	}
	d.want("applied with the cache at generation 1", 3, 3, "True/Available")
	if v := d.cd().Status.ApiVersion; !strings.HasSuffix(v, "/v2-0-0") {
		t.Errorf("status.apiVersion = %s", v)
	}

	// The live read fails: the cached copy is used, spec AND generation, and the status write carries
	// its resourceVersion, so it conflicts and nothing is recorded.
	d.edit(func(cd *compositiondefinitionsv1alpha1.CompositionDefinition) { cd.Spec.UpgradePolicy = "Paused" })
	e.reader.fail = errors.New("injected")
	if _, err := d.pass(); err == nil || !apierrors.IsConflict(err) {
		t.Errorf("a pass on a stale copy must conflict, got %v", err)
	}
	d.want("live read failing", 4, 3, "True/Available")
	e.reader.fail = nil
	e.kube.lag(nil)
	d.rest(3)
	w.holds()
}

// Connect builds the clients from the CACHED spec and Observe reads the LIVE one. When deploy.targetRef
// changed in between, the pass observed the cluster the spec no longer names, so it records nothing.
func TestEnvtest_644_AnotherTargetRecordsNothing(t *testing.T) {
	e := start644(t)
	d := e.settled(t, "retargeted")

	// A KubernetesTarget that is this very apiserver, and whose authn is the one the local controller is
	// given, so a pass connected to it renders and observes the same and reaches the same verdict: only
	// the TARGET is wrong.
	e.spokeIsThisCluster(t, AuthnURL)

	digest := d.cd().Status.Digest
	cached := d.cd()
	cached.Spec.Deploy = &compositiondefinitionsv1alpha1.DeploymentTarget{TargetRef: &compositiondefinitionsv1alpha1.TargetReference{Name: "spoke"}}
	d.edit(func(cd *compositiondefinitionsv1alpha1.CompositionDefinition) { cd.Spec.UpgradePolicy = "Manual" })
	e.kube.lag(cached)
	if _, err := d.pass(); err != nil {
		t.Fatalf("premise: the pass must complete on the wrong target, got %v", err)
	}
	if e.kube.served == 0 || d.cond("Synced") != "True/ReconcileSuccess" || d.cd().Status.Digest != digest ||
		d.cd().Status.Target == nil || d.cd().Status.Target.KubeconfigSecretResourceVersion == "" {
		t.Fatalf("premise: one up-to-date pass, connected to the spoke; served=%d Synced=%s digest %s -> %s target=%+v",
			e.kube.served, d.cond("Synced"), digest, d.cd().Status.Digest, d.cd().Status.Target)
	}
	d.want("connected to the old target", 2, 1, "True/Available")

	// CONTROL: the same pass connected to the target the live spec names.
	e.kube.lag(nil)
	if _, err := d.pass(); err != nil {
		t.Fatal(err)
	}
	d.want("connected to the spec's target", 2, 2, "True/Available")
}

// authn refuses (#642): Observe errors, Ready is unknown, and the field stays behind until it recovers.
func TestEnvtest_644_AuthnBlockedStaysBehind(t *testing.T) {
	e := start644(t)
	d := e.settled(t, "blocked")
	w := d.watch()
	d.edit(func(cd *compositiondefinitionsv1alpha1.CompositionDefinition) {
		cd.Spec.ApiRef = &compositiondefinitionsv1alpha1.ApiReference{Name: "status-sources", Namespace: self642CDNS}
	})
	for i := 0; i < 2; i++ {
		if _, err := d.pass(); err == nil {
			t.Fatal("premise: authn is not installed, so the pass must fail")
		}
		d.want("authn missing", 2, 1, "Unknown/NotObserved")
	}
	e.c.installAuthnCRD(t, true)
	d.rest(6)
	d.want("recovered", 2, 2, "True/Available")
	w.holds()
}

// A follower (another definition owns its Kind and version) determines its Ready from the owner's
// controller, on its own spec.
func TestEnvtest_644_Follower(t *testing.T) {
	e := start644(t)
	owner := e.settled(t, "shared")
	f := e.define(t, "shared", "shared-follower", nil)
	f.rest(6)
	if f.cd().Status.Owner == nil {
		t.Fatal("premise: the second definition of one Kind and version must be a follower")
	}
	gen, _ := f.generations()
	f.edit(func(cd *compositiondefinitionsv1alpha1.CompositionDefinition) { cd.Spec.UpgradePolicy = "Manual" })
	f.want("edited, not reconciled", gen+1, gen, "True/Available")
	if _, err := f.pass(); err != nil {
		t.Fatal(err)
	}
	f.want("observed", gen+1, gen+1, "True/Available")
	owner.want("the owner is untouched", 1, 1, "True/Available")
}

// The CRD of an older chart prunes the field: it is absent, nothing fails, and the definition does not
// churn. With the CRD of this chart it appears. And an older core-provider's status write, which does
// not carry the field, REMOVES it: absent, so a reader must take absent as "not reported".
func TestEnvtest_644_OlderCRDAndOlderWriter(t *testing.T) {
	e := start644(t)
	ctx := context.Background()
	crdName := types.NamespacedName{Name: "compositiondefinitions.core.krateo.io"}
	full := &apiextensionsv1.CustomResourceDefinition{}
	if err := e.c.cl.Get(ctx, crdName, full); err != nil {
		t.Fatal(err)
	}
	setProp := func(present bool) {
		t.Helper()
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := e.c.cl.Get(ctx, crdName, crd); err != nil {
			t.Fatal(err)
		}
		props := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["status"].Properties
		if present {
			props["observedGeneration"] = full.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["status"].Properties["observedGeneration"]
		} else {
			if _, ok := props["observedGeneration"]; !ok {
				t.Fatal("the shipped CRD does not declare status.observedGeneration, so every write of it is pruned")
			}
			delete(props, "observedGeneration")
		}
		if err := e.c.cl.Update(ctx, crd); err != nil {
			t.Fatal(err)
		}
	}
	raw := func(d *cd644) (int64, bool) {
		t.Helper()
		u, err := e.c.raw.Resource(cdGVR644).Namespace(d.key.Namespace).Get(ctx, d.key.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		v, ok, _ := unstructured.NestedInt64(u.Object, "status", "observedGeneration")
		return v, ok
	}

	setProp(false)
	e.charts.add(t, "olderone", "1.0.0", `{"type":"object"}`)
	d := e.define(t, "olderone", "olderone", nil)
	for i := 0; i < 5; i++ {
		if _, err := d.pass(); err != nil {
			t.Fatal(err)
		}
	}
	if d.cond("Ready") != "True/Available" {
		t.Fatalf("premise: Ready=%s", d.cond("Ready"))
	}
	if v, ok := raw(d); ok {
		t.Fatalf("premise: an older CRD must prune it, got %d", v)
	}
	rv := d.cd().ResourceVersion
	for i := 0; i < 3; i++ {
		if _, err := d.pass(); err != nil {
			t.Fatal(err)
		}
	}
	if got := d.cd().ResourceVersion; got != rv {
		t.Errorf("a pruned field churns the definition: resourceVersion %s -> %s", rv, got)
	}

	setProp(true)
	deadline := time.Now().Add(20 * time.Second)
	for { // the apiserver swaps the schema it serves a moment after the CRD write
		if _, err := d.pass(); err != nil {
			t.Fatal(err)
		}
		if v, ok := raw(d); ok && v == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the field never appeared with this chart's CRD")
		}
		time.Sleep(200 * time.Millisecond)
	}

	// An older writer: the whole status PUT, without the field.
	u, err := e.c.raw.Resource(cdGVR644).Namespace(d.key.Namespace).Get(ctx, d.key.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	unstructured.RemoveNestedField(u.Object, "status", "observedGeneration")
	if _, err := e.c.raw.Resource(cdGVR644).Namespace(d.key.Namespace).UpdateStatus(ctx, u, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if v, ok := raw(d); ok {
		t.Errorf("an older core-provider's status write left a stale observedGeneration %d behind", v)
	}
	d.rest(2)
	if v, ok := raw(d); !ok || v != 1 {
		t.Errorf("the next pass did not record it again: %d %v", v, ok)
	}
}

// SIDE FINDING, not fixed here: what a remote definition reports when its chart is cluster-local to the
// management cluster. cdreason declares Ready=False/RemoteChartUnreachable (parent verdict failed). The
// seed sets it and returns a plain error from Observe, so the runtime replaces it with
// Ready=Unknown/NotObserved, and from Create the status re-read drops it. This records what is served
// today; a fix changes the assertion.
func TestEnvtest_644_RemoteChartUnreachableIsOverwritten(t *testing.T) {
	e := start644(t)
	e.charts.add(t, "remotelocal", "1.0.0", `{"type":"object"}`) // served from 127.0.0.1: cluster-local
	e.spokeIsThisCluster(t, "")
	d := e.define(t, "remotelocal", "remotelocal", func(cd *compositiondefinitionsv1alpha1.CompositionDefinition) {
		cd.Spec.Deploy = &compositiondefinitionsv1alpha1.DeploymentTarget{TargetRef: &compositiondefinitionsv1alpha1.TargetReference{Name: "spoke"}}
	})
	w := d.watch()
	for i := 0; i < 4; i++ {
		_, err := d.pass()
		t.Logf("pass %d: err=%v Ready=%s Synced=%s", i, err, d.cond("Ready"), d.cond("Synced"))
	}
	if !strings.Contains(d.message("Synced"), "cluster-local") {
		t.Fatalf("premise: the remote seed must have refused the chart; Synced=%s %q", d.cond("Synced"), d.message("Synced"))
	}
	for i, s := range w.served() {
		if s.readyReason == "RemoteChartUnreachable" {
			t.Errorf("served version %d carries RemoteChartUnreachable: the finding does not hold", i)
		}
	}
	if got := d.cond("Ready"); got != "Unknown/NotObserved" {
		t.Errorf("Ready = %s, recorded finding is Unknown/NotObserved", got)
	}
}

//go:build envtest

package composition

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/krateo-platformops/composition-dynamic-controller/internal/tools/archive"
	compositionMeta "github.com/krateo-platformops/composition-dynamic-controller/pkg/meta"
	helmconfig "github.com/krateo-platformops/plumbing/helm"
	helmv3 "github.com/krateo-platformops/plumbing/helm/v3"
	"github.com/krateo-platformops/plumbing/kubeutil/event"
	"github.com/krateo-platformops/unstructured-runtime/pkg/logging"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"
	helmtime "helm.sh/helm/v3/pkg/time"
	corev1 "k8s.io/api/core/v1"
	apixv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// core-provider#636 against a real apiserver and real helm storage, through the handler's own Create,
// Observe, Update and Delete.
//
// The chart's three post-install hooks run by weight. hook-1 is a Job whose hook-delete-policy omits
// before-hook-creation, so one left behind collides with the retry's. hook-2 goes to the namespace the
// values name: one that does not exist makes the install fail there, AFTER the app and hook-1 exist and
// before hook-3. upgrade-ran is the witness that an upgrade ran, predelete-ran that an uninstall ran
// with hooks. `both` adds an object and a key that the edited-values arm drops.
var chart636 = map[string]string{
	"app.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: app\n  namespace: {{ .Release.Namespace }}\ndata:\n  k: {{ .Values.k | quote }}\n" +
		"{{- if .Values.both }}\n  dropped: \"yes\"\n{{- end }}\n",
	"extra.yaml": "{{- if .Values.both }}\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: extra\n  namespace: {{ .Release.Namespace }}\ndata:\n  k: v\n{{- end }}\n",
	"pvc.yaml":   "apiVersion: v1\nkind: PersistentVolumeClaim\nmetadata:\n  name: data\n  namespace: {{ .Release.Namespace }}\nspec:\n  accessModes: [ReadWriteOnce]\n  resources:\n    requests:\n      storage: 1Gi\n",
	"hook1.yaml": "apiVersion: batch/v1\nkind: Job\nmetadata:\n  name: hook-1\n  namespace: {{ .Release.Namespace }}\n  annotations:\n    helm.sh/hook: post-install\n" +
		"    helm.sh/hook-weight: \"1\"\n    helm.sh/hook-delete-policy: hook-failed\nspec:\n  template:\n    spec:\n      restartPolicy: Never\n      containers:\n      - name: c\n        image: busybox\n",
	"hook2.yaml": hookCM636("hook-2", "post-install", 2, "{{ .Values.hookns }}"),
	"hook3.yaml": hookCM636("hook-3", "post-install", 3, "{{ .Release.Namespace }}"),
	"upg.yaml":   hookCM636("upgrade-ran", "post-upgrade", 1, "{{ .Release.Namespace }}"),
	"pre.yaml":   hookCM636("predelete-ran", "pre-delete", 1, "{{ .Release.Namespace }}"),
}

func hookCM636(name, hook string, weight int, ns string) string {
	return fmt.Sprintf("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: %s\n  namespace: %s\n  annotations:\n    helm.sh/hook: %s\n    helm.sh/hook-weight: \"%d\"\ndata:\n  k: v\n", name, ns, hook, weight)
}

func serveChart636(t *testing.T, version string, override map[string]string) string {
	t.Helper()
	files := map[string]string{"Chart.yaml": "apiVersion: v2\nname: p636\nversion: " + version + "\n"}
	for name, body := range chart636 {
		files["templates/"+name] = body
	}
	for name, body := range override {
		files["templates/"+name] = body
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: "p636/" + name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	_ = tw.Close()
	_ = gz.Close()
	tgz := buf.Bytes()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(tgz) }))
	t.Cleanup(srv.Close)
	return srv.URL + "/p636-" + version + ".tgz"
}

type getter636 struct {
	url, version, definitionVersion string
	ann                             map[string]string // the CompositionDefinition's; nil = not read
}

func (g getter636) info() *archive.Info {
	return &archive.Info{URL: g.url, Version: g.version, DefinitionVersion: g.definitionVersion, Auth: &archive.Auth{},
		CompositionDefinitionInfo: &archive.CompositionDefinitionInfo{Name: "fw", Namespace: "krateo-system", Annotations: g.ann,
			GVR: schema.GroupVersionResource{Group: "core.krateo.io", Version: "v1alpha1", Resource: "compositiondefinitions"}}}
}
func (g getter636) Get(*unstructured.Unstructured) (*archive.Info, error)    { return g.info(), nil }
func (g getter636) Locate(*unstructured.Unstructured) (*archive.Info, error) { return g.info(), nil }
func (g getter636) WithLogger(logging.Logger) archive.Getter                 { return g }

type env636 struct {
	ctx       context.Context
	cfg       *rest.Config
	kube      kubernetes.Interface
	dyn       dynamic.Interface
	mapper    apimeta.RESTMapper
	inspector string
	url       string
}

var gvr636 = schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v0-1-0", Resource: "fireworks"}

func start636(t *testing.T) *env636 {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via setup-envtest")
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	preserve := true
	if _, err := apiextclient.NewForConfigOrDie(cfg).ApiextensionsV1().CustomResourceDefinitions().Create(ctx, &apixv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: gvr636.Resource + "." + gvr636.Group},
		Spec: apixv1.CustomResourceDefinitionSpec{
			Group: gvr636.Group, Scope: apixv1.NamespaceScoped,
			Names: apixv1.CustomResourceDefinitionNames{Plural: gvr636.Resource, Singular: "firework", Kind: "Firework", ListKind: "FireworkList"},
			Versions: []apixv1.CustomResourceDefinitionVersion{{
				Name: gvr636.Version, Served: true, Storage: true,
				Subresources: &apixv1.CustomResourceSubresources{Status: &apixv1.CustomResourceSubresourceStatus{}},
				Schema: &apixv1.CustomResourceValidation{OpenAPIV3Schema: &apixv1.JSONSchemaProps{Type: "object", XPreserveUnknownFields: &preserve,
					// core-provider's status schema defaults these; Observe reads status.digest.
					Properties: map[string]apixv1.JSONSchemaProps{"status": {Type: "object", XPreserveUnknownFields: &preserve,
						Properties: map[string]apixv1.JSONSchemaProps{"digest": {Type: "string", Default: &apixv1.JSON{Raw: []byte(`""`)}}}}}}},
			}},
		},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := waitServed(cfg, gvr636); err != nil {
		t.Fatal(err)
	}
	groupResources, err := restmapper.GetAPIGroupResources(discovery.NewDiscoveryClientForConfigOrDie(cfg))
	if err != nil {
		t.Fatal(err)
	}
	inspector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"group":"","version":"v1","resource":"configmaps","verbs":["*"]},{"group":"","version":"v1","resource":"persistentvolumeclaims","verbs":["*"]},{"group":"batch","version":"v1","resource":"jobs","verbs":["*"]}]`))
	}))
	t.Cleanup(inspector.Close)
	oldPoll := hookJobsGonePoll
	hookJobsGonePoll = 50 * time.Millisecond
	t.Cleanup(func() { hookJobsGonePoll = oldPoll })
	return &env636{ctx: ctx, cfg: cfg, kube: kubernetes.NewForConfigOrDie(cfg), dyn: dynamic.NewForConfigOrDie(cfg),
		mapper: restmapper.NewDiscoveryRESTMapper(groupResources), inspector: inspector.URL, url: serveChart636(t, "0.1.0", nil)}
}

// arm636 is one composition in its own namespace.
type arm636 struct {
	e   *env636
	t   *testing.T
	ns  string
	h   *handler
	rec *events.FakeRecorder
	gc  atomic.Bool // plays the garbage collector's foreground deletion for Jobs
}

func (e *env636) arm(t *testing.T, ns string, g getter636) *arm636 {
	t.Helper()
	if _, err := e.kube.CoreV1().Namespaces().Create(e.ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if g.url == "" {
		g.url = e.url
	}
	if g.version == "" {
		g.version = "0.1.0"
	}
	if g.definitionVersion == "" {
		g.definitionVersion = g.version
	}
	ctx, cancel := context.WithCancel(e.ctx)
	t.Cleanup(cancel)
	a := &arm636{e: e, t: t, ns: ns, rec: events.NewFakeRecorder(1000)}
	a.gc.Store(true)
	a.h = &handler{kubeconfig: e.cfg, pluralizer: p597Pluralizer{}, packageInfoGetter: g, chartInspectorUrl: e.inspector,
		saName: "cdc", saNamespace: ns, eventRecorder: *event.NewAPIRecorder(a.rec), mapper: e.mapper}
	completeJobs(ctx, t, e.kube, ns)
	go func() { // envtest runs no garbage collector: drop the finalizer it would, once a Job is deleted
		for ctx.Err() == nil {
			// Nor a volume binder: an unbound PVC is a child that is not ready.
			if pvc, err := e.kube.CoreV1().PersistentVolumeClaims(ns).Get(ctx, "data", metav1.GetOptions{}); err == nil && pvc.Status.Phase != corev1.ClaimBound {
				pvc.Status.Phase = corev1.ClaimBound
				_, _ = e.kube.CoreV1().PersistentVolumeClaims(ns).UpdateStatus(ctx, pvc, metav1.UpdateOptions{})
			}
			if jobs, err := e.kube.BatchV1().Jobs(ns).List(ctx, metav1.ListOptions{}); err == nil && a.gc.Load() {
				for i := range jobs.Items {
					if j := &jobs.Items[i]; j.DeletionTimestamp != nil && len(j.Finalizers) > 0 {
						j.Finalizers = nil
						_, _ = e.kube.BatchV1().Jobs(ns).Update(ctx, j, metav1.UpdateOptions{})
					}
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()
	return a
}

func (a *arm636) getter(f func(*getter636)) {
	g := a.h.packageInfoGetter.(getter636)
	f(&g)
	a.h.packageInfoGetter = g
}

func (a *arm636) get() *unstructured.Unstructured {
	a.t.Helper()
	u, err := a.e.dyn.Resource(gvr636).Namespace(a.ns).Get(a.e.ctx, "fw", metav1.GetOptions{})
	if err != nil {
		a.t.Fatal(err)
	}
	return u
}

func (a *arm636) edit(f func(*unstructured.Unstructured)) *unstructured.Unstructured {
	a.t.Helper()
	u := a.get()
	f(u)
	u, err := a.e.dyn.Resource(gvr636).Namespace(a.ns).Update(a.e.ctx, u, metav1.UpdateOptions{})
	if err != nil {
		a.t.Fatal(err)
	}
	return u
}

func (a *arm636) setAnnotation(k, v string) {
	a.edit(func(u *unstructured.Unstructured) {
		ann := u.GetAnnotations()
		if ann == nil {
			ann = map[string]string{}
		}
		ann[k] = v
		u.SetAnnotations(ann)
	})
}

// fixSpec makes hook-2 succeed, and drops what `both` added.
func (a *arm636) fixSpec(u *unstructured.Unstructured) {
	u.Object["spec"] = map[string]any{"k": "v1", "hookns": a.ns}
}

// firstInstall creates the composition and runs Create. fail: hook-2 cannot run.
func (a *arm636) firstInstall(fail bool) {
	a.t.Helper()
	spec := map[string]any{"k": "v1", "hookns": a.ns, "both": true}
	if fail {
		spec["hookns"] = "no-such-namespace"
	}
	u, err := a.e.dyn.Resource(gvr636).Namespace(a.ns).Create(a.e.ctx, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": gvr636.GroupVersion().String(), "kind": "Firework",
		"metadata": map[string]any{"name": "fw", "namespace": a.ns, "finalizers": []any{"test.krateo.io/hold"}},
		"spec":     spec,
	}}, metav1.CreateOptions{})
	if err != nil {
		a.t.Fatal(err)
	}
	err = a.h.Create(a.e.ctx, u)
	if (err != nil) != fail {
		a.t.Fatalf("control: first install: err=%v, want a failure: %v", err, fail)
	}
	want := "1:deployed"
	if fail {
		want = "1:failed"
	}
	if got := a.history(); got != want {
		a.t.Fatalf("control: history %q after the first install, want %q", got, want)
	}
	if a.app() == "" || a.jobUID("hook-1") == "" || !fail != a.cm("hook-3") || a.cm("upgrade-ran") {
		a.t.Fatalf("control: after the first install: app %q, hook-1 %q, hook-3 %v, upgrade-ran %v", a.app(), a.jobUID("hook-1"), a.cm("hook-3"), a.cm("upgrade-ran"))
	}
	a.drainEvents()
}

func (a *arm636) relName() string {
	a.t.Helper()
	n := a.get().GetLabels()[compositionMeta.ReleaseNameLabel]
	if n == "" {
		a.t.Fatal("no release-name label")
	}
	return n
}

func (a *arm636) storage() *storage.Storage {
	return storage.Init(driver.NewSecrets(a.e.kube.CoreV1().Secrets(a.ns)))
}

// history is "rev:status ..." in revision order, "" when there is no release.
func (a *arm636) history() string {
	a.t.Helper()
	rs, err := a.storage().History(a.relName())
	if err != nil && err != driver.ErrReleaseNotFound {
		a.t.Fatal(err)
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].Version < rs[j].Version })
	var out []string
	for _, r := range rs {
		out = append(out, fmt.Sprintf("%d:%s", r.Version, r.Info.Status))
	}
	return strings.Join(out, " ")
}

// recordInstall writes this controller's operation record of an install started just before updated, as
// a controller that died in it leaves it.
func (a *arm636) recordInstall(updated time.Time) {
	a.t.Helper()
	u := a.get()
	_ = unstructured.SetNestedStringMap(u.Object, map[string]string{"phase": "install", "startedBy": a.ns + "/a-pod-that-is-gone",
		"startedAt": updated.Add(-time.Second).UTC().Format(time.RFC3339Nano)}, "status", "helmOperation")
	if _, err := a.e.dyn.Resource(gvr636).Namespace(a.ns).UpdateStatus(a.e.ctx, u, metav1.UpdateOptions{}); err != nil {
		a.t.Fatal(err)
	}
}

// recovery is status.installRecovery as "kind/count", "" when there is none.
func (a *arm636) recovery() string {
	a.t.Helper()
	r, found, _ := unstructured.NestedMap(a.get().Object, "status", "installRecovery")
	if !found {
		return ""
	}
	return fmt.Sprintf("%v/%v", r["kind"], r["count"])
}

// handInstalled creates the composition labelled with a release a human installed with helm: it ends
// failed at hook-2, or is left pending-install.
func (a *arm636) handInstalled(status release.Status) time.Time {
	a.t.Helper()
	hc, err := helmv3.NewClient(a.e.cfg, helmv3.WithNamespace(a.ns))
	if err != nil {
		a.t.Fatal(err)
	}
	defer hc.Close()
	_, err = hc.Install(a.e.ctx, "by-hand", a.e.url, &helmconfig.InstallConfig{Namespace: a.ns, ActionConfig: &helmconfig.ActionConfig{
		Values: map[string]any{"k": "v1", "hookns": "no-such-namespace"}}})
	if err == nil {
		a.t.Fatal("control: the hand install must fail at hook-2")
	}
	if _, err := a.e.dyn.Resource(gvr636).Namespace(a.ns).Create(a.e.ctx, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": gvr636.GroupVersion().String(), "kind": "Firework",
		"metadata": map[string]any{"name": "fw", "namespace": a.ns, "labels": map[string]any{compositionMeta.ReleaseNameLabel: "by-hand"}},
		"spec":     map[string]any{"k": "v1", "hookns": a.ns},
	}}, metav1.CreateOptions{}); err != nil {
		a.t.Fatal(err)
	}
	st := a.storage()
	r, err := st.Get("by-hand", 1)
	if err != nil {
		a.t.Fatal(err)
	}
	r.Info.Status = status
	r.Info.LastDeployed = helmtime.Time{Time: time.Now().Add(-time.Hour)}
	if err := st.Update(r); err != nil {
		a.t.Fatal(err)
	}
	return r.Info.LastDeployed.Time
}

// rewriteHistory replaces the stored history with copies of revision 1 at these revisions and statuses,
// and records the latest as this controller's install.
func (a *arm636) rewriteHistory(revs map[int]release.Status) {
	a.t.Helper()
	st, name := a.storage(), a.relName()
	first, err := st.Get(name, 1)
	if err != nil {
		a.t.Fatal(err)
	}
	if _, err := st.Delete(name, 1); err != nil {
		a.t.Fatal(err)
	}
	for v, status := range revs {
		r := *first
		info := *first.Info
		r.Version, r.Info = v, &info
		r.Info.Status = status
		r.Info.LastDeployed = helmtime.Time{Time: time.Now().Add(-time.Hour)} // past every grace
		if err := st.Create(&r); err != nil {
			a.t.Fatal(err)
		}
	}
	a.recordInstall(time.Now().Add(-time.Hour))
}

func (a *arm636) cmGet(name string) *corev1.ConfigMap {
	a.t.Helper()
	c, err := a.e.kube.CoreV1().ConfigMaps(a.ns).Get(a.e.ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		a.t.Fatal(err)
	}
	return c
}

func (a *arm636) cm(name string) bool { return a.cmGet(name) != nil }

// app is the app object's UID and the PVC's, "" when they are gone (or the PVC is being deleted: envtest
// runs no controller to let it go).
func (a *arm636) app() string {
	a.t.Helper()
	pvc, err := a.e.kube.CoreV1().PersistentVolumeClaims(a.ns).Get(a.e.ctx, "data", metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		a.t.Fatal(err)
	}
	out := ""
	if c := a.cmGet("app"); c != nil {
		out = string(c.UID)
	}
	if err == nil && pvc.DeletionTimestamp == nil {
		out += "+" + string(pvc.UID)
	}
	return out
}

func (a *arm636) jobUID(name string) string {
	a.t.Helper()
	j, err := a.e.kube.BatchV1().Jobs(a.ns).Get(a.e.ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return ""
	}
	if err != nil {
		a.t.Fatal(err)
	}
	return string(j.UID)
}

// ready is "status/reason" of the stored Ready.
func (a *arm636) ready() string {
	a.t.Helper()
	conds, _, _ := unstructured.NestedSlice(a.get().Object, "status", "conditions")
	for _, c := range conds {
		if m, ok := c.(map[string]any); ok && m["type"] == "Ready" {
			return fmt.Sprintf("%v/%v", m["status"], m["reason"])
		}
	}
	return "absent"
}

func (a *arm636) annotation(k string) string { return a.get().GetAnnotations()[k] }

func (a *arm636) drainEvents() string {
	var out []string
	for len(a.rec.Events) > 0 {
		out = append(out, <-a.rec.Events)
	}
	return strings.Join(out, "|")
}

// observe is one Observe pass, and the Update the runtime dispatches when it reports drift.
func (a *arm636) observe() error {
	a.t.Helper()
	obs, err := a.h.Observe(a.e.ctx, a.get())
	a.t.Logf("observe: %+v err=%v; history [%s] Ready %s", obs, err, a.history(), a.ready())
	if err != nil {
		return err
	}
	if !obs.ResourceExists {
		a.t.Fatal("reported absent: something is installed")
	}
	if !obs.ResourceUpToDate {
		if err := a.h.Update(a.e.ctx, a.get()); err != nil {
			return err
		}
		a.t.Logf("update: history [%s] Ready %s", a.history(), a.ready())
	}
	return nil
}

// counted636 counts the calls to plumbing's own two, which do the work.
type counted636 struct {
	plumbingRetrier
	retries, settles int
}

func (c *counted636) RetryFailedInstall(ctx context.Context, hc helmconfig.Client, name, url string, ac *helmconfig.ActionConfig, maxHistory int) (*helmconfig.OperationResult, error) {
	c.retries++
	return c.plumbingRetrier.RetryFailedInstall(ctx, hc, name, url, ac, maxHistory)
}

func (c *counted636) SettleInterruptedInstall(ctx context.Context, hc helmconfig.Client, name string, revision int) error {
	c.settles++
	return c.plumbingRetrier.SettleInterruptedInstall(ctx, hc, name, revision)
}

// killedRetry636 is a retry killed half-way: the new revision is left pending-install.
type killedRetry636 struct {
	*counted636
	a *arm636
}

func (k *killedRetry636) RetryFailedInstall(ctx context.Context, hc helmconfig.Client, name, url string, ac *helmconfig.ActionConfig, maxHistory int) (*helmconfig.OperationResult, error) {
	op, err := k.counted636.RetryFailedInstall(ctx, hc, name, url, ac, maxHistory)
	if err != nil {
		return op, err
	}
	// What a dead process leaves: the revision pending, long ago, and no more hooks.
	st := k.a.storage()
	r, err := st.Last(name)
	if err != nil {
		return nil, err
	}
	r.Info.Status, r.Info.LastDeployed = release.StatusPendingInstall, helmtime.Time{Time: time.Now().Add(-time.Hour)}
	if err := st.Update(r); err != nil {
		return nil, err
	}
	for _, n := range []string{"hook-2", "hook-3"} {
		if err := k.a.e.kube.CoreV1().ConfigMaps(k.a.ns).Delete(ctx, n, metav1.DeleteOptions{}); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("killed")
}

// countRetries counts what the handler asks of plumbing, for one test.
func countRetries(t *testing.T) *counted636 {
	t.Helper()
	c := &counted636{}
	setRetrier(t, c)
	return c
}

// installedWithHooks asserts the end of a successful retry: an INSTALL ran (every install hook, no
// upgrade hook), over the same app object, with hook-1 a new Job, and nothing left to say about hooks.
func (a *arm636) installedWithHooks(appUID, oldJob string) {
	a.t.Helper()
	if !a.cm("hook-2") || !a.cm("hook-3") || a.cm("upgrade-ran") || a.cm("predelete-ran") {
		a.t.Errorf("hook-2 %v hook-3 %v upgrade-ran %v predelete-ran %v; want the install hooks and no other", a.cm("hook-2"), a.cm("hook-3"), a.cm("upgrade-ran"), a.cm("predelete-ran"))
	}
	if a.app() != appUID {
		a.t.Errorf("app object %s, was %s: the retry must keep it", a.app(), appUID)
	}
	if j := a.jobUID("hook-1"); j == "" || j == oldJob {
		a.t.Errorf("hook-1 Job %s, was %s: want a new one", j, oldJob)
	}
	if h := strings.Fields(a.history()); !strings.HasSuffix(h[len(h)-1], ":deployed") {
		a.t.Errorf("history %v", h)
	}
	if m := a.annotation(compositionMeta.AnnotationKeyReleaseNeverDeployed); m != "" {
		a.t.Errorf("never-deployed mark %s on a deployed release", m)
	}
	if u := a.annotation(compositionMeta.AnnotationKeyHooksUnverified); u != "" {
		a.t.Errorf("hooks-unverified %s although every install hook ran", u)
	}
}

func TestEnvtest_636_FailedFirstInstall(t *testing.T) {
	e := start636(t)
	known := func() getter636 { return getter636{ann: map[string]string{}} }
	sw := compositionMeta.AnnotationKeyInterruptedInstallRecovery

	// The issue itself. A permanently failing install hook never reads Ready=True and its history stays
	// bounded; by the fourth attempt revision 1 is pruned, so the mark is the only proof left; and once
	// the spec is fixed, the next pass installs with every hook.
	t.Run("a failing hook is retried as an install until it is fixed", func(t *testing.T) {
		a := e.arm(t, "retry", known())
		a.firstInstall(true)
		app, job := a.app(), a.jobUID("hook-1")
		for i := 2; i <= 6; i++ {
			if err := a.observe(); err == nil {
				t.Fatalf("attempt %d: control: hook-2 still cannot run, so the attempt must fail", i)
			}
			h := strings.Fields(a.history())
			if len(h) != min(i, 3) || h[len(h)-1] != fmt.Sprintf("%d:failed", i) {
				t.Fatalf("attempt %d: history %v, want the last %d, failed", i, h, min(i, 3))
			}
			want := "False/Creating"
			if i >= installFailingAfter {
				want = "False/InstallFailing"
			}
			if r := a.ready(); r != want {
				t.Errorf("attempt %d: Ready %s, want %s", i, r, want)
			}
			if a.cm("upgrade-ran") || a.cm("hook-3") {
				t.Fatalf("attempt %d: upgrade-ran %v, hook-3 %v", i, a.cm("upgrade-ran"), a.cm("hook-3"))
			}
		}
		if r := a.recovery(); r != "Retried/5" {
			t.Errorf("status.installRecovery %s after 5 retries", r)
		}
		if h := a.history(); strings.HasPrefix(h, "1:") {
			t.Fatalf("control: revision 1 must be pruned by now, so the mark is the only proof: %s", h)
		}
		if err := a.h.Update(e.ctx, a.edit(a.fixSpec)); err != nil { // a spec change reaches Update directly
			t.Fatalf("the fixed spec: %v", err)
		}
		a.installedWithHooks(app, job)
		if err := a.observe(); err != nil || a.ready() != "True/Available" || a.recovery() != "Retried/6" {
			t.Errorf("after the fix: err=%v Ready %s installRecovery %s; want it kept after success", err, a.ready(), a.recovery())
		}
	})

	// Edited values: what only the failed attempt had is gone, the key as well as the object.
	t.Run("edited values leave nothing of the failed attempt behind", func(t *testing.T) {
		a := e.arm(t, "edited", known())
		a.firstInstall(true)
		if !a.cm("extra") || a.cmGet("app").Data["dropped"] != "yes" {
			t.Fatal("control: the failed attempt must have created the extra object and key")
		}
		app, job := a.app(), a.jobUID("hook-1")
		a.edit(a.fixSpec)
		if err := a.observe(); err != nil {
			t.Fatal(err)
		}
		a.installedWithHooks(app, job)
		if a.cm("extra") || a.cmGet("app").Data["dropped"] != "" {
			t.Errorf("left behind: object %v, key %q", a.cm("extra"), a.cmGet("app").Data["dropped"])
		}
	})

	// A graceful stop cancels the mark's write and the status write with the install itself: what is stored is
	// the operation record written before it, and that proves it.
	t.Run("the mark lost with the controller, the operation record stands in", func(t *testing.T) {
		a := e.arm(t, "nomark", known())
		a.firstInstall(true)
		a.edit(func(u *unstructured.Unstructured) { u.SetAnnotations(nil) })
		r, err := a.storage().Get(a.relName(), 1)
		if err != nil {
			t.Fatal(err)
		}
		a.recordInstall(r.Info.LastDeployed.Time)
		app, job := a.app(), a.jobUID("hook-1")
		a.edit(a.fixSpec)
		if err := a.observe(); err != nil {
			t.Fatal(err)
		}
		a.installedWithHooks(app, job)
		// Control: neither mark nor record, and it is the upgrade.
		c := e.arm(t, "nomark-ctl", known())
		c.firstInstall(true)
		c.edit(func(u *unstructured.Unstructured) { u.SetAnnotations(nil) })
		if _, found, _ := unstructured.NestedMap(c.get().Object, "status", "helmOperation"); found {
			t.Fatal("control: the record must be gone once the mark stands in for it")
		}
		c.edit(c.fixSpec)
		if err := c.observe(); err != nil || !c.cm("upgrade-ran") || c.cm("hook-3") {
			t.Errorf("control: err=%v upgrade-ran %v hook-3 %v", err, c.cm("upgrade-ran"), c.cm("hook-3"))
		}
	})

	t.Run("a fixed chart version is picked up", func(t *testing.T) {
		a := e.arm(t, "version", known())
		a.firstInstall(true)
		app, job := a.app(), a.jobUID("hook-1")
		fixed := serveChart636(t, "0.2.0", map[string]string{"hook2.yaml": hookCM636("hook-2", "post-install", 2, "{{ .Release.Namespace }}")})
		a.getter(func(g *getter636) { g.url, g.version, g.definitionVersion = fixed, "0.2.0", "0.2.0" })
		if err := a.observe(); err != nil {
			t.Fatal(err)
		}
		a.installedWithHooks(app, job)
		if v, _, _ := unstructured.NestedString(a.get().Object, "status", "helmChartVersion"); v != "0.2.0" {
			t.Errorf("helmChartVersion %q", v)
		}
	})

	t.Run("Create over a failed first install", func(t *testing.T) {
		a := e.arm(t, "create", known())
		a.firstInstall(true)
		app, job := a.app(), a.jobUID("hook-1")
		if err := a.h.Create(e.ctx, a.edit(a.fixSpec)); err != nil {
			t.Fatal(err)
		}
		a.installedWithHooks(app, job)
	})

	// The previous attempt's hook Job may still be running. Nothing is installed until it is gone.
	t.Run("the failed attempt's hook Job must be gone first", func(t *testing.T) {
		a := e.arm(t, "jobgone", known())
		a.firstInstall(true)
		app, job := a.app(), a.jobUID("hook-1")
		a.edit(a.fixSpec)
		a.gc.Store(false) // the Job's pods do not stop
		old := hookJobsGoneTimeout
		hookJobsGoneTimeout = time.Second
		err := a.observe()
		hookJobsGoneTimeout = old
		if err == nil || !strings.Contains(err.Error(), "still exists") || a.history() != "1:failed" || a.cm("hook-3") {
			t.Fatalf("err=%v history %s hook-3 %v; want the pass stopped with nothing installed", err, a.history(), a.cm("hook-3"))
		}
		a.gc.Store(true) // control: they stop
		if err := a.observe(); err != nil {
			t.Fatal(err)
		}
		a.installedWithHooks(app, job)
	})

	// ONCE LIVE. The release deployed; its live revision was superseded by a failed rollback and pruned.
	// Its history is what a never-deployed release's looks like, failed revisions only.
	t.Run("a once-live release pruned to failed revisions is upgraded, never installed again", func(t *testing.T) {
		a := e.arm(t, "oncelive", known())
		a.firstInstall(false)
		a.rewriteHistory(map[int]release.Status{4: release.StatusFailed, 5: release.StatusFailed, 6: release.StatusFailed})
		app, job := a.app(), a.jobUID("hook-1")
		if err := a.observe(); err != nil {
			t.Fatal(err)
		}
		if !a.cm("upgrade-ran") || !strings.HasSuffix(a.history(), "7:deployed") {
			t.Errorf("upgrade-ran %v history %s; want today's upgrade", a.cm("upgrade-ran"), a.history())
		}
		if a.app() != app || a.jobUID("hook-1") != job || a.cm("predelete-ran") {
			t.Errorf("app %s (was %s), hook-1 %s (was %s), predelete-ran %v: untouched", a.app(), app, a.jobUID("hook-1"), job, a.cm("predelete-ran"))
		}
		if ev := a.drainEvents(); strings.Contains(ev, string(reasonFailedInstallRetried)) || a.annotation(compositionMeta.AnnotationKeyReleaseNeverDeployed) != "" {
			t.Errorf("events %s, mark %s", ev, a.annotation(compositionMeta.AnnotationKeyReleaseNeverDeployed))
		}
	})

	// The same, with the latest attempt interrupted. Control: the same state whose history accounts for
	// every revision is settled and retried.
	t.Run("a once-live release interrupted at revision 6 is rolled back, as before", func(t *testing.T) {
		d := countRetries(t)
		a := e.arm(t, "oncelive-pending", known())
		a.firstInstall(false)
		a.rewriteHistory(map[int]release.Status{4: release.StatusFailed, 5: release.StatusFailed, 6: release.StatusPendingInstall})
		app, job := a.app(), a.jobUID("hook-1")
		if obs, err := a.h.Observe(e.ctx, a.get()); err != nil || !obs.ResourceExists {
			t.Fatalf("%+v err=%v", obs, err)
		}
		if a.app() != app || a.jobUID("hook-1") != job || a.cm("predelete-ran") || a.recovery() != "" || a.annotation(compositionMeta.AnnotationKeyHooksUnverified) == "" || !strings.Contains(a.history(), "7:deployed") {
			t.Errorf("app %s (was %s) history %s installRecovery %q hooks-unverified %q; want #563's rollback to itself, marked",
				a.app(), app, a.history(), a.recovery(), a.annotation(compositionMeta.AnnotationKeyHooksUnverified))
		}
		if d.settles != 0 || d.retries != 0 {
			t.Errorf("settled %d, retried %d", d.settles, d.retries)
		}
	})
	// An interrupted first install, at revision 1 and as a killed retry: kept, object for object and
	// volume for volume, and installed again with every install hook. Interrupted again, it is retried
	// again: each retry records itself.
	for _, tc := range []struct {
		name, ns string
		revs     map[int]release.Status
		want     string // the history it ends with
	}{
		{"an interrupted first install is retried, everything kept", "int-1", map[int]release.Status{1: release.StatusPendingInstall}, "1:failed 2:deployed"},
		{"a killed retry is retried, everything kept", "int-2", map[int]release.Status{1: release.StatusFailed, 2: release.StatusPendingInstall}, "1:failed 2:failed 3:deployed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := e.arm(t, tc.ns, known())
			a.firstInstall(true) // hook-1 ran, hook-2 and hook-3 never did
			a.edit(a.fixSpec)
			a.rewriteHistory(tc.revs)
			app, job := a.app(), a.jobUID("hook-1")
			if err := a.observe(); err != nil {
				t.Fatal(err)
			}
			a.installedWithHooks(app, job)
			if a.history() != tc.want || a.recovery() != "Retried/1" || a.ready() != "True/Available" {
				t.Errorf("history %s installRecovery %s Ready %s; want %s, Retried/1, True/Available", a.history(), a.recovery(), a.ready(), tc.want)
			}
			if ev := a.drainEvents(); !strings.Contains(ev, string(reasonInterruptedInstallRetried)) || strings.Contains(ev, string(reasonFailedInstallRetried)) {
				t.Errorf("events %s", ev)
			}
		})
	}
	t.Run("a second interruption is retried too", func(t *testing.T) {
		d := countRetries(t)
		a := e.arm(t, "int-twice", known())
		a.firstInstall(true)
		a.edit(a.fixSpec)
		a.rewriteHistory(map[int]release.Status{1: release.StatusPendingInstall})
		app := a.app()
		// The retry is killed mid-way: its record stands, and the release is pending-install at revision 2.
		setRetrier(t, &killedRetry636{counted636: d, a: a})
		if err := a.observe(); err == nil || !strings.HasSuffix(a.history(), "2:pending-install") {
			t.Fatalf("control: err=%v history %s; want the retry killed at revision 2", err, a.history())
		}
		setRetrier(t, d)
		// The record its own beforeInstallAgain wrote, which a dead process leaves and this live one, returning
		// from the call, cleared (TestBeforeInstallAgain shows the one it writes is found orphaned).
		a.recordInstall(time.Now().Add(-time.Hour))
		job := a.jobUID("hook-1")
		if err := a.observe(); err != nil {
			t.Fatal(err)
		}
		a.installedWithHooks(app, job)
		if a.history() != "1:failed 2:failed 3:deployed" || a.recovery() != "Retried/2" {
			t.Errorf("history %s installRecovery %s", a.history(), a.recovery())
		}
	})
	// A live install is never settled: within the grace, and its starter not known to be gone.
	t.Run("an install that may still be running is left alone", func(t *testing.T) {
		d := countRetries(t)
		a := e.arm(t, "int-live", known())
		a.firstInstall(true)
		a.rewriteHistory(map[int]release.Status{1: release.StatusPendingInstall})
		st := a.storage()
		r, err := st.Get(a.relName(), 1)
		if err != nil {
			t.Fatal(err)
		}
		r.Info.LastDeployed = helmtime.Time{Time: time.Now()}
		if err := st.Update(r); err != nil {
			t.Fatal(err)
		}
		a.recordInstall(r.Info.LastDeployed.Time)
		// Its starter's pod exists: not orphaned.
		if _, err := e.kube.CoreV1().Pods(a.ns).Create(e.ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "a-pod-that-is-gone"},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "i"}}}}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		if obs, err := a.h.Observe(e.ctx, a.get()); err != nil || !obs.ResourceUpToDate || a.history() != "1:pending-install" || a.ready() != "False/PendingHelmOperation" || a.recovery() != "" {
			t.Errorf("%+v err=%v history %s Ready %s installRecovery %q; want it left to finish", obs, err, a.history(), a.ready(), a.recovery())
		}
		if d.settles != 0 {
			t.Errorf("settled %d times", d.settles)
		}
		// Control: its starter gone, the same install is settled and retried.
		a.edit(a.fixSpec)
		zero := int64(0)
		if err := e.kube.CoreV1().Pods(a.ns).Delete(e.ctx, "a-pod-that-is-gone", metav1.DeleteOptions{GracePeriodSeconds: &zero}); err != nil {
			t.Fatal(err)
		}
		if err := a.observe(); err != nil || a.history() != "1:failed 2:deployed" {
			t.Errorf("control: err=%v history %s", err, a.history())
		}
	})

	// HAND-INSTALLED. The release-name label points at a release a human installed, never deployed: this
	// controller did not start that install. Control: the same release with this controller's record.
	for _, tc := range []struct {
		name, ns string
		status   release.Status
		recorded bool
	}{
		{"a hand-installed failed release is upgraded, never installed again", "hand-failed", release.StatusFailed, false},
		{"control: the same failed release, this controller's install recorded", "hand-failed-ctl", release.StatusFailed, true},
		{"a hand-installed pending release is never settled nor retried", "hand-pending", release.StatusPendingInstall, false},
		{"control: the same pending release, this controller's install recorded", "hand-pending-ctl", release.StatusPendingInstall, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := countRetries(t)
			a := e.arm(t, tc.ns, known())
			updated := a.handInstalled(tc.status)
			if tc.recorded {
				a.recordInstall(updated)
			}
			app, job := a.app(), a.jobUID("hook-1")
			obs, err := a.h.Observe(e.ctx, a.get())
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case tc.status == release.StatusFailed && tc.recorded:
				a.installedWithHooks(app, job)
			case tc.status == release.StatusFailed:
				if !a.cm("upgrade-ran") || a.cm("hook-3") || a.app() != app || a.jobUID("hook-1") != job || a.recovery() != "" {
					t.Errorf("upgrade-ran %v hook-3 %v app %s (was %s) hook-1 %s (was %s) installRecovery %q; want today's upgrade",
						a.cm("upgrade-ran"), a.cm("hook-3"), a.app(), app, a.jobUID("hook-1"), job, a.recovery())
				}
			case tc.recorded:
				a.installedWithHooks(app, job)
			default:
				if !strings.HasPrefix(a.history(), "1:pending-install 2:") || a.app() != app || a.jobUID("hook-1") != job || a.recovery() != "" || a.cm("hook-3") {
					t.Errorf("%+v app %s (was %s) history %s installRecovery %q hook-3 %v; want #563's rollback to itself",
						obs, a.app(), app, a.history(), a.recovery(), a.cm("hook-3"))
				}
			}
			if !tc.recorded && (d.settles != 0 || d.retries != 0) {
				t.Errorf("settled %d, retried %d: not this controller's install", d.settles, d.retries)
			}
		})
	}

	// Under the switch, or at the cap, nothing is retried. Revision 1 is rolled back to itself (its stored
	// manifest is the render). A KILLED RETRY is never rolled back: what it stores is what the failed
	// attempts applied, here the object extra, which the values have since dropped. It is settled, and the
	// upgrade converges to the values. Either way the mark outlives the upgrade of the same pass.
	for _, tc := range []struct {
		name, ns   string
		revs       map[int]release.Status
		ann, value string
		rolledBack bool
	}{
		{"kill switch, revision 1", "sw-1", map[int]release.Status{1: release.StatusPendingInstall}, sw, "manual", true},
		{"kill switch, a killed retry", "sw-2", map[int]release.Status{1: release.StatusFailed, 2: release.StatusPendingInstall}, sw, "manual", false},
		{"cap, revision 1", "cap-1", map[int]release.Status{1: release.StatusPendingInstall}, compositionMeta.AnnotationKeyInterruptedInstallReinstalls, "3", true},
		{"cap, a killed retry", "cap-2", map[int]release.Status{1: release.StatusFailed, 2: release.StatusPendingInstall}, compositionMeta.AnnotationKeyInterruptedInstallReinstalls, "3", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := countRetries(t)
			a := e.arm(t, tc.ns, known())
			a.firstInstall(true)
			a.rewriteHistory(tc.revs)
			a.setAnnotation(tc.ann, tc.value)
			a.edit(a.fixSpec)
			app, job := a.app(), a.jobUID("hook-1")
			for pass := 1; pass <= 3; pass++ {
				if err := a.observe(); err != nil {
					t.Fatal(err)
				}
				if r := a.ready(); r != "False/HooksMayNotHaveRun" {
					t.Fatalf("pass %d: Ready %s", pass, r)
				}
				if rolledBack := strings.HasPrefix(a.history(), "1:pending-install 2:superseded 3:deployed"); rolledBack != tc.rolledBack {
					t.Errorf("history %s; rolled back to itself %v, want %v", a.history(), rolledBack, tc.rolledBack)
				}
			}
			if !a.cm("upgrade-ran") || a.cm("hook-3") || a.cm("extra") || a.app() != app || a.jobUID("hook-1") != job || a.cm("predelete-ran") || a.recovery() != "" {
				t.Errorf("upgrade-ran %v hook-3 %v extra %v app %s (was %s) hook-1 %s (was %s) installRecovery %q; want the upgrade to the values, nothing else",
					a.cm("upgrade-ran"), a.cm("hook-3"), a.cm("extra"), a.app(), app, a.jobUID("hook-1"), job, a.recovery())
			}
			if d.retries != 0 || (d.settles == 1) == tc.rolledBack {
				t.Errorf("retried %d, settled %d", d.retries, d.settles)
			}
			a.edit(func(u *unstructured.Unstructured) { u.Object["spec"].(map[string]any)["k"] = "v2" })
			if err := a.observe(); err != nil || a.ready() != "True/Available" {
				t.Errorf("control: after a later upgrade: err=%v Ready %s", err, a.ready())
			}
		})
	}

	// The failed attempt's recorded hook Jobs are what the retry's would collide with: control B of #629's
	// envtest, the retry over a hook Job that is still there fails on it.
	t.Run("control: a retry over a leftover hook Job fails on it", func(t *testing.T) {
		d := countRetries(t)
		a := e.arm(t, "leftover", known())
		a.firstInstall(true)
		hc, err := helmv3.NewClient(e.cfg, helmv3.WithNamespace(a.ns))
		if err != nil {
			t.Fatal(err)
		}
		defer hc.Close()
		_, err = d.RetryFailedInstall(e.ctx, hc, a.relName(), e.url, &helmconfig.ActionConfig{TakeOwnership: true, Values: map[string]any{"k": "v1", "hookns": a.ns}}, 3)
		if err == nil || !strings.Contains(err.Error(), "exists") {
			t.Fatalf("err=%v", err)
		}
	})

	// DELETING with a failed release: reported as existing, so the runtime's Delete uninstalls it; and a
	// delete that lands while a pass is in flight stops the retry.
	t.Run("a deleting composition's failed release is uninstalled, not retried", func(t *testing.T) {
		a := e.arm(t, "deleting", known())
		a.firstInstall(true)
		inFlight := a.get() // read before the delete
		if err := e.dyn.Resource(gvr636).Namespace(a.ns).Delete(e.ctx, "fw", metav1.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
		if a.get().GetDeletionTimestamp() == nil {
			t.Fatal("control: the composition must be deleting, held by its finalizer")
		}
		for name, u := range map[string]*unstructured.Unstructured{"deleting": a.get(), "deleted while in flight": inFlight} {
			obs, err := a.h.Observe(e.ctx, u)
			if err != nil || !obs.ResourceExists || !obs.ResourceUpToDate || a.history() != "1:failed" {
				t.Errorf("%s: observation %+v err %v history %s; want exists, up to date, untouched", name, obs, err, a.history())
			}
		}
		// Update has no deletion guard of its own before this point, so this is the retry's.
		if err := a.h.Update(e.ctx, a.get()); err != nil || a.history() != "1:failed" {
			t.Errorf("Update on a deleting composition: err=%v history %s; want nothing installed", err, a.history())
		}
		if a.app() == "" {
			t.Fatal("control: the app must still be there before Delete")
		}
		if err := a.h.Delete(e.ctx, a.get()); err != nil {
			t.Fatal(err)
		}
		if a.history() != "" || a.app() != "" || !a.cm("predelete-ran") {
			t.Errorf("after Delete: history %q app %q predelete-ran %v; want it uninstalled with its hooks", a.history(), a.app(), a.cm("predelete-ran"))
		}
		if a.jobUID("hook-1") != "" {
			t.Error("after Delete: the failed install's hook Job is still there")
		}
	})

	// THE KILL SWITCH keeps today's upgrade and says so: Ready is never True over install hooks that did
	// not run, and the upgrade that skipped them does not clear the mark. Control, in each: a later
	// upgrade does (#563's contract).
	for _, tc := range []struct {
		name, ns string
		getter   getter636
		ann      map[string]string
	}{
		{"kill switch on the composition", "ks-comp", known(), map[string]string{sw: "manual"}},
		{"kill switch on the CompositionDefinition, an unrecognised value", "ks-cd", getter636{ann: map[string]string{sw: "x"}}, nil},
		{"the CompositionDefinition's annotations unread", "ks-unread", getter636{}, nil},
		{"the policy withholds create", "ks-create", known(), map[string]string{"krateo.io/management-policy": "observe,update,delete"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := e.arm(t, tc.ns, tc.getter)
			a.firstInstall(true)
			for k, v := range tc.ann {
				a.setAnnotation(k, v)
			}
			app, job := a.app(), a.jobUID("hook-1")
			for pass := 1; pass <= 3; pass++ {
				if err := a.observe(); err != nil {
					t.Fatal(err)
				}
				if r := a.ready(); r != "False/HooksMayNotHaveRun" || a.annotation(compositionMeta.AnnotationKeyHooksUnverified) == "" {
					t.Fatalf("pass %d: Ready %s, mark %q; want False/HooksMayNotHaveRun, marked", pass, r, a.annotation(compositionMeta.AnnotationKeyHooksUnverified))
				}
			}
			if !a.cm("upgrade-ran") || a.cm("hook-3") || a.history() != "1:superseded 2:deployed" {
				t.Errorf("upgrade-ran %v hook-3 %v history %s; want today's upgrade, once", a.cm("upgrade-ran"), a.cm("hook-3"), a.history())
			}
			if a.app() != app || a.jobUID("hook-1") != job || a.cm("predelete-ran") {
				t.Errorf("app %s (was %s) hook-1 %s (was %s) predelete-ran %v", a.app(), app, a.jobUID("hook-1"), job, a.cm("predelete-ran"))
			}
			if m := a.annotation(compositionMeta.AnnotationKeyReleaseNeverDeployed); m != "" {
				t.Errorf("never-deployed mark %s on a deployed release", m)
			}
			a.edit(a.fixSpec)
			if err := a.observe(); err != nil || a.ready() != "True/Available" || a.annotation(compositionMeta.AnnotationKeyHooksUnverified) != "" {
				t.Errorf("control: after a later upgrade: err=%v Ready %s mark %q", err, a.ready(), a.annotation(compositionMeta.AnnotationKeyHooksUnverified))
			}
		})
	}

	// Create's own upgrade, under the switch.
	t.Run("Create over a failed first install under the kill switch", func(t *testing.T) {
		a := e.arm(t, "ks-createsite", known())
		a.firstInstall(true)
		a.setAnnotation(sw, "manual")
		if err := a.h.Create(e.ctx, a.get()); err != nil {
			t.Fatal(err)
		}
		if !a.cm("upgrade-ran") || a.cm("hook-3") || a.annotation(compositionMeta.AnnotationKeyHooksUnverified) == "" {
			t.Errorf("upgrade-ran %v hook-3 %v mark %q; want the upgrade, marked", a.cm("upgrade-ran"), a.cm("hook-3"), a.annotation(compositionMeta.AnnotationKeyHooksUnverified))
		}
		if err := a.observe(); err != nil || a.ready() != "False/HooksMayNotHaveRun" {
			t.Errorf("then: err=%v Ready %s", err, a.ready())
		}
	})

	t.Run("a withheld update: nothing is written, and Ready is not True", func(t *testing.T) {
		a := e.arm(t, "withheld", known())
		a.firstInstall(true)
		a.setAnnotation("krateo.io/management-policy", "observe")
		app, mark := a.app(), a.annotation(compositionMeta.AnnotationKeyReleaseNeverDeployed)
		for pass := 1; pass <= 2; pass++ {
			obs, err := a.h.Observe(e.ctx, a.get())
			if err != nil || !obs.ResourceExists || a.history() != "1:failed" || a.ready() != "False/HooksMayNotHaveRun" {
				t.Fatalf("pass %d: %+v err=%v history %s Ready %s", pass, obs, err, a.history(), a.ready())
			}
		}
		if a.app() != app || a.cm("upgrade-ran") || a.cm("hook-3") ||
			a.annotation(compositionMeta.AnnotationKeyHooksUnverified) != "" || a.annotation(compositionMeta.AnnotationKeyReleaseNeverDeployed) != mark || a.recovery() != "" {
			t.Error("something was written")
		}
		// Control: the instrument sees a write. The same composition, the policy lifted and the switch set.
		a.setAnnotation("krateo.io/management-policy", "default")
		a.setAnnotation(sw, "manual")
		if err := a.observe(); err != nil || a.history() != "1:superseded 2:deployed" {
			t.Errorf("control: err=%v history %s", err, a.history())
		}
	})

	t.Run("a retiring controller leaves it", func(t *testing.T) {
		a := e.arm(t, "retiring", known())
		a.firstInstall(true)
		a.getter(func(g *getter636) { g.definitionVersion = "0.2.0" })
		before := a.ready()
		if err := a.observe(); err != nil {
			t.Fatal(err)
		}
		if err := a.h.Update(e.ctx, a.get()); err != nil {
			t.Fatal(err)
		}
		if a.history() != "1:failed" || a.ready() != before || a.cm("upgrade-ran") || a.cm("hook-3") {
			t.Errorf("history %s Ready %s (was %s) upgrade-ran %v hook-3 %v; want it left alone", a.history(), a.ready(), before, a.cm("upgrade-ran"), a.cm("hook-3"))
		}
		if ev := a.drainEvents(); !strings.Contains(ev, string(reasonRetiringLeftInstall)) {
			t.Errorf("events %s", ev)
		}
		// Control: the same composition, this controller current again, under the switch: it writes.
		a.getter(func(g *getter636) { g.definitionVersion = "0.1.0" })
		a.setAnnotation(sw, "manual")
		if err := a.observe(); err != nil || a.history() != "1:superseded 2:deployed" {
			t.Errorf("control: err=%v history %s", err, a.history())
		}
	})

	// A NEW release of the same name restarts at revision 1, which an old mark would vouch for: Create
	// removes it first.
	t.Run("a fresh install removes an earlier release's mark", func(t *testing.T) {
		a := e.arm(t, "freshmark", known())
		a.firstInstall(true)
		a.setAnnotation(compositionMeta.AnnotationKeyReleaseNeverDeployed, ndMark(string(a.get().GetUID()), a.relName(), 3))
		st := a.storage()
		if _, err := st.Delete(a.relName(), 1); err != nil {
			t.Fatal(err)
		}
		if err := a.e.kube.BatchV1().Jobs(a.ns).Delete(e.ctx, "hook-1", metav1.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
		// This install succeeds, so nothing else of this pass rewrites or removes the mark.
		a.edit(a.fixSpec)
		if err := a.h.Create(e.ctx, a.get()); err != nil || a.history() != "1:deployed" {
			t.Fatalf("control: err=%v history %s", err, a.history())
		}
		if m := a.annotation(compositionMeta.AnnotationKeyReleaseNeverDeployed); m != "" {
			t.Errorf("the mark of the earlier release is still there: %s", m)
		}
	})
}

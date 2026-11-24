//go:build envtest

package compositiondefinitions

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/krateo-platformops/core-provider/apis"
	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/tools/deploy"
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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"
)

// #642 on a real apiserver, through provider-runtime's own reconciler: pass after pass of Reconcile on
// one CompositionDefinition with an apiRef, never a restart. authn and snowplow are an HTTP server that
// answers as they do: authn exchanges a token only while core-provider's OWN mapping exists ON THE HUB
// and names its ServiceAccount, and answers 403 NoMapping otherwise; snowplow's /rbac answers only a
// caller carrying the token authn issued.
//
// Everything is observed from outside the code under test: the live objects, and the requests that
// reached each apiserver. Nothing here calls a #642 function, so the same file runs against the
// unfixed sources.

const (
	self642NS      = "krateo-system"
	self642Name    = "core-provider"
	self642CDNS    = "team-642"
	self642CRDName = "serviceaccounts.serviceaccount.authn.krateo.io"
	// The definition's controller mapping: cdc-<controller namespace>-<sa>.
	self642Controller = "cdc-krateo-system-contents-v1-0-0"
	self642JWT        = "x-self642-issued"
)

var self642GVR = schema.GroupVersionResource{Group: "serviceaccount.authn.krateo.io", Version: "v1alpha1", Resource: "serviceaccounts"}

// The authn mapping CRD: the fields core-provider writes, structural and pruning like authn's own.
const self642CRD = `
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: serviceaccounts.serviceaccount.authn.krateo.io
spec:
  group: serviceaccount.authn.krateo.io
  names: {kind: ServiceAccount, listKind: ServiceAccountList, plural: serviceaccounts, singular: serviceaccount}
  scope: Namespaced
  versions:
  - name: v1alpha1
    served: true
    storage: true
    schema:
      openAPIV3Schema:
        type: object
        required: [spec]
        properties:
          apiVersion: {type: string}
          kind: {type: string}
          metadata: {type: object}
          spec:
            type: object
            required: [serviceAccountRef]
            properties:
              displayName: {type: string}
              principal: {type: string, enum: [agent, controller, service]}
              groups: {type: array, items: {type: string}}
              delegation:
                type: object
                required: [allowed]
                properties:
                  allowed: {type: boolean}
                  maxDepth: {type: integer, format: int32}
              serviceAccountRef:
                type: object
                required: [name, namespace]
                properties:
                  name: {type: string}
                  namespace: {type: string}
`

// authn642 is the fake authn and snowplow. One per test binary: core-provider's authn client is a
// process-wide singleton that keeps the first URL it sees.
type authn642 struct {
	srv *httptest.Server
	// hub reads the hub's mappings; nil answers NoMapping.
	hub atomic.Pointer[dynamic.DynamicClient]
	// lag is how many exchanges still answer NoMapping although the mapping exists (authn's cache has
	// not seen it yet).
	lag atomic.Int32
	// exchanges and noMapping count what authn was asked and how often it refused; rbac counts /rbac
	// answers given.
	exchanges, noMapping, rbac atomic.Int32
}

var (
	authn642Once sync.Once
	authn642Inst *authn642
)

func theAuthn642() *authn642 {
	authn642Once.Do(func() {
		a := &authn642{}
		mux := http.NewServeMux()
		mux.HandleFunc("/serviceaccount/login", a.login)
		mux.HandleFunc("/rbac", a.rbacHandler)
		a.srv = httptest.NewServer(mux)
		authn642Inst = a
	})
	return authn642Inst
}

// token is a JWT that is already inside the client's refresh skew, so every pass exchanges again: the
// instrument must see each pass's answer, not the first one's from a cache.
func (a *authn642) token() string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"HS256"}`)) + "." + enc([]byte(fmt.Sprintf(`{"exp":%d,"n":%q}`, time.Now().Unix()+1, self642JWT))) + ".sig"
}

func (a *authn642) login(w http.ResponseWriter, r *http.Request) {
	a.exchanges.Add(1)
	refuse := func() {
		a.noMapping.Add(1)
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"kind":"Status","reason":"NoMapping","message":"no ServiceAccount mapping for the caller's ServiceAccount (not in the exchange allowlist)"}`))
	}
	hub := a.hub.Load()
	if hub == nil {
		refuse()
		return
	}
	m, err := hub.Resource(self642GVR).Namespace(self642NS).Get(r.Context(), self642Name, metav1.GetOptions{})
	if err != nil {
		refuse()
		return
	}
	name, _, _ := unstructured.NestedString(m.Object, "spec", "serviceAccountRef", "name")
	ns, _, _ := unstructured.NestedString(m.Object, "spec", "serviceAccountRef", "namespace")
	if name != self642Name || ns != self642NS || a.lag.Add(-1) >= 0 {
		refuse()
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"accessToken": a.token()})
}

func (a *authn642) rbacHandler(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), ".")
	var payload []byte
	if len(parts) == 3 {
		payload, _ = base64.RawURLEncoding.DecodeString(parts[1])
	}
	if !strings.Contains(string(payload), self642JWT) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	a.rbac.Add(1)
	_, _ = w.Write([]byte(`{"restaction":{"name":"status-sources","namespace":"` + self642CDNS + `"},"readSet":[{"group":"","version":"v1","resource":"configmaps","namespace":"` + self642CDNS + `","verb":"get"}]}`))
}

// cluster642 is one apiserver, its clients, and a record of every request for an authn mapping.
type cluster642 struct {
	cl  client.Client
	dyn dynamic.Interface
	cs  kubernetes.Interface
	// raw bypasses the recording transport: the test's own reads and writes.
	raw *dynamic.DynamicClient

	mu     sync.Mutex
	writes []string
	// fail, when set, answers a mapping request itself.
	fail func(*http.Request) *http.Response
}

type rec642 struct {
	c    *cluster642
	next http.RoundTripper
}

func (t rec642) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.Contains(r.URL.Path, "/apis/serviceaccount.authn.krateo.io/") {
		t.c.mu.Lock()
		if r.Method != http.MethodGet {
			name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			if r.Method == http.MethodPost {
				name = "(create)"
				if r.GetBody != nil {
					if b, err := r.GetBody(); err == nil {
						var o struct{ Metadata struct{ Name string } }
						_ = json.NewDecoder(b).Decode(&o)
						name = o.Metadata.Name
					}
				}
			}
			w := r.Method + " " + name
			if r.URL.Query().Get("dryRun") != "" {
				w += " dryRun"
			}
			t.c.writes = append(t.c.writes, w)
		}
		fail := t.c.fail
		t.c.mu.Unlock()
		if fail != nil {
			if resp := fail(r); resp != nil {
				return resp, nil
			}
		}
	}
	return t.next.RoundTrip(r)
}

// take returns the mapping write requests about name since the last take, dry runs included.
func (c *cluster642) take(name string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, w := range c.writes {
		if name == "" || strings.Contains(w+" ", " "+name+" ") {
			out = append(out, w)
		}
	}
	c.writes = nil
	return out
}

func (c *cluster642) setFail(f func(*http.Request) *http.Response) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fail = f
}

func start642(t *testing.T) *cluster642 {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Fatal("KUBEBUILDER_ASSETS is not set: this test exists to run against a real apiserver, and a skip would read as a pass")
	}
	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "..", "crds")}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	c := &cluster642{}
	rawCfg := rest.CopyConfig(cfg)
	cfg.WrapTransport = func(rt http.RoundTripper) http.RoundTripper { return rec642{c: c, next: rt} }
	sc := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(sc)
	_ = apiextensionsv1.AddToScheme(sc)
	_ = apis.AddToScheme(sc)
	if c.cl, err = client.New(cfg, client.Options{Scheme: sc}); err != nil {
		t.Fatal(err)
	}
	c.dyn = dynamic.NewForConfigOrDie(cfg)
	c.cs = kubernetes.NewForConfigOrDie(cfg)
	c.raw = dynamic.NewForConfigOrDie(rawCfg)
	for _, ns := range []string{self642NS, self642CDNS} {
		if err := c.cl.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

// installAuthnCRD installs authn's mapping CRD; serving waits until the apiserver answers for it, as
// authn's own start does.
func (c *cluster642) installAuthnCRD(t *testing.T, serving bool) {
	t.Helper()
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := yaml.Unmarshal([]byte(self642CRD), crd); err != nil {
		t.Fatal(err)
	}
	if err := c.cl.Create(context.Background(), crd); err != nil {
		t.Fatal(err)
	}
	if !serving {
		return
	}
	for i := 0; i < 100; i++ {
		if _, err := c.raw.Resource(self642GVR).Namespace(self642NS).List(context.Background(), metav1.ListOptions{}); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("premise: the authn CRD never served")
}

// mapping reads a live mapping; nil when it (or the kind) is not there.
func (c *cluster642) mapping(t *testing.T, name string) *unstructured.Unstructured {
	t.Helper()
	m, err := c.raw.Resource(self642GVR).Namespace(self642NS).Get(context.Background(), name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func (c *cluster642) deleteMapping(t *testing.T, name string) {
	t.Helper()
	if err := c.raw.Resource(self642GVR).Namespace(self642NS).Delete(context.Background(), name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if c.mapping(t, name) != nil {
		t.Fatalf("premise: mapping %s is still there", name)
	}
}

// bench642 is one definition reconciled against hub (and target: the same cluster, or a spoke).
type bench642 struct {
	t           *testing.T
	hub, target *cluster642
	authn       *authn642
	ext         *external
	r           *reconciler.Reconciler
	key         types.NamespacedName
}

func newBench642(t *testing.T, hub, target *cluster642) *bench642 {
	t.Helper()
	a := theAuthn642()
	a.hub.Store(hub.raw)
	a.lag.Store(0)

	root := t.TempDir()
	if _, err := renderChartAssets(root, self642NS, "ghcr.io/krateo-platformops/composition-dynamic-controller:642-test"); err != nil {
		t.Fatal(err)
	}
	set := func(p *string, v string) {
		old := *p
		*p = v
		t.Cleanup(func() { *p = old })
	}
	set(&CDCrbacConfigFolder, filepath.Join(root, "cdc-rbac"))
	set(&CDCtemplateDeploymentPath, filepath.Join(root, "cdc-deployment", filepath.Base(CDCtemplateDeploymentPath)))
	set(&CDCtemplateConfigmapPath, filepath.Join(root, "cdc-configmap", filepath.Base(CDCtemplateConfigmapPath)))
	set(&JSONSchemaTemplateConfigmapPath, filepath.Join(root, "json-schema-configmap", filepath.Base(JSONSchemaTemplateConfigmapPath)))
	set(&ServiceTemplatePath, filepath.Join(root, "cdc-service", filepath.Base(ServiceTemplatePath)))
	set(&ManagementNamespace, self642NS)
	set(&AuthnNamespace, self642NS)
	set(&SelfSAName, self642Name)
	set(&SelfSANamespace, self642NS)
	set(&AuthnURL, a.srv.URL)
	set(&SnowplowURL, a.srv.URL)
	// Not restored: the client core-provider built from it lives as long as the test binary.
	deploy.AuthnTokenPath = tokenPath642(t)

	tgz := contentsChartTGZ(t, "1.0.0", twoConfigMaps)
	chartSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(tgz) }))
	t.Cleanup(chartSrv.Close)

	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go markAvailable(context.Background(), target.cl, self642NS, stop)

	cd := &compositiondefinitionsv1alpha1.CompositionDefinition{ObjectMeta: metav1.ObjectMeta{Name: "contents", Namespace: self642CDNS}}
	cd.Spec.Chart = &compositiondefinitionsv1alpha1.ChartInfo{Url: chartSrv.URL + "/contents-1.0.0.tgz", Version: "1.0.0"}
	cd.Spec.ApiRef = &compositiondefinitionsv1alpha1.ApiReference{Name: "status-sources", Namespace: self642CDNS}
	if err := hub.cl.Create(context.Background(), cd); err != nil {
		t.Fatal(err)
	}

	b := &bench642{t: t, hub: hub, target: target, authn: a, key: client.ObjectKeyFromObject(cd)}
	// What Connect builds: the hub's clients, and the target's swapped in for a remote definition. The
	// spec has no deploy.targetRef, so the remote SEED (a spoke-reachable chart, chart-inspector) does
	// not run; which client each write goes through is the whole of what is under test.
	b.ext = &external{
		mgmt: hub.cl, mgmtDynamic: hub.dyn, apiReader: hub.cl,
		kube: target.cl, dynamic: target.dyn, client: target.cs, remote: hub != target,
		log: logging.NewNopLogger(), pluralizer: notFoundPluralizer{},
	}
	b.r = reconciler.NewReconciler(&fake.Manager{Client: hub.cl, Scheme: hub.cl.Scheme()},
		resource.ManagedKind(compositiondefinitionsv1alpha1.CompositionDefinitionGroupVersionKind),
		reconciler.WithExternalConnecter(reconciler.ExternalConnectorFn(func(context.Context, resource.Managed) (reconciler.ExternalClient, error) {
			return b.ext, nil
		})),
	)
	return b
}

var (
	token642Once sync.Once
	token642Path string
)

// tokenPath642 is a token file that outlives any one test: core-provider's authn client is a singleton
// that keeps the path it was built with.
func tokenPath642(t *testing.T) string {
	t.Helper()
	token642Once.Do(func() {
		f, err := os.CreateTemp("", "self642-token")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.WriteString("SA-TOKEN")
		_ = f.Close()
		token642Path = f.Name()
	})
	return token642Path
}

// in makes the bench report on t for the rest of a subtest.
func (b *bench642) in(t *testing.T) {
	outer := b.t
	b.t = t
	t.Cleanup(func() { b.t = outer })
}

// pass runs one reconcile, and returns what the runtime would do with it: an error is a requeue with
// backoff.
func (b *bench642) pass() (ctrl.Result, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	return b.r.Reconcile(ctx, ctrl.Request{NamespacedName: b.key})
}

func (b *bench642) cd() *compositiondefinitionsv1alpha1.CompositionDefinition {
	b.t.Helper()
	cd := &compositiondefinitionsv1alpha1.CompositionDefinition{}
	if err := b.hub.cl.Get(context.Background(), b.key, cd); err != nil {
		b.t.Fatal(err)
	}
	return cd
}

// cond is a condition as "Status/Reason", or "absent".
func (b *bench642) cond(ct string) string {
	b.t.Helper()
	for _, c := range b.cd().Status.Conditions {
		if string(c.Type) == ct {
			return string(c.Status) + "/" + string(c.Reason)
		}
	}
	return "absent"
}

func (b *bench642) message(ct string) string {
	b.t.Helper()
	for _, c := range b.cd().Status.Conditions {
		if string(c.Type) == ct {
			return c.Message
		}
	}
	return ""
}

// settle runs passes until the definition is Ready with nothing left to do, at most max.
func (b *bench642) settle(max int) (passes int) {
	b.t.Helper()
	for passes = 1; passes <= max; passes++ {
		_, err := b.pass()
		cd := b.cd()
		if err == nil && cd.Status.Digest != "" && cd.GetCondition(rtv1.TypeReady).Status == metav1.ConditionTrue &&
			cd.GetCondition(rtv1.TypeSynced).Status == metav1.ConditionTrue {
			return passes
		}
		b.t.Logf("pass %d: err=%v ready=%s synced=%s AuthnMappingApplied=%s", passes, err, b.cond("Ready"), b.cond("Synced"), b.cond("AuthnMappingApplied"))
	}
	b.t.Fatalf("not settled after %d passes: ready=%s synced=%s %q; own mapping present=%v; exchanges=%d refused=%d",
		max, b.cond("Ready"), b.cond("Synced"), b.message("Synced"), b.hub.mapping(b.t, self642Name) != nil,
		b.authn.exchanges.Load(), b.authn.noMapping.Load())
	return 0
}

func (b *bench642) annotate(kv map[string]string) {
	b.t.Helper()
	cd := b.cd()
	an := cd.GetAnnotations()
	if an == nil {
		an = map[string]string{}
	}
	for k, v := range kv {
		if v == "" {
			delete(an, k)
			continue
		}
		an[k] = v
	}
	cd.SetAnnotations(an)
	if err := b.hub.cl.Update(context.Background(), cd); err != nil {
		b.t.Fatal(err)
	}
}

func (b *bench642) wantCond(when, want string) {
	b.t.Helper()
	if got := b.cond("AuthnMappingApplied"); got != want {
		b.t.Errorf("%s: AuthnMappingApplied = %s (%q), want %s", when, got, b.message("AuthnMappingApplied"), want)
	}
}

// (a), (c): the fresh install of #642. The definition's first passes precede authn's CRD; it is
// reported as waiting for authn, and recovers by itself once the CRD is there.
func TestEnvtest_642_FreshInstallRecoversOnceAuthnIsInstalled(t *testing.T) {
	c := start642(t)
	b := newBench642(t, c, c)
	for i := 1; i <= 3; i++ {
		if _, err := b.pass(); err == nil {
			t.Fatalf("pass %d: no error, so nothing retries it while authn is missing", i)
		}
	}
	// Named as authn not being installed, on the management cluster, and not as a deploy failure.
	b.wantCond("authn not installed", "False/AuthnNotInstalled")
	if m := b.message("AuthnMappingApplied"); !strings.Contains(m, "core-provider has no authn mapping of its own") || !strings.Contains(m, "waiting for authn") {
		t.Errorf("AuthnMappingApplied message = %q, want it to say core-provider is waiting for authn", m)
	}
	if m := b.message("Synced"); strings.Contains(m, "could not find the requested resource") {
		t.Errorf("Synced = %q: the missing CRD failed the deploy", m)
	}
	if w := c.take(self642Name); len(w) != 0 {
		t.Errorf("writes of core-provider's mapping with no CRD = %v, want none", w)
	}

	c.installAuthnCRD(t, true)
	n := b.settle(4)
	t.Logf("recovered in %d passes after the CRD was installed", n)
	if c.mapping(t, self642Name) == nil || c.mapping(t, self642Controller) == nil {
		t.Errorf("mappings: own=%v controller=%v, want both", c.mapping(t, self642Name) != nil, c.mapping(t, self642Controller) != nil)
	}
	b.wantCond("recovered", "True/Applied")
	if b.authn.rbac.Load() == 0 {
		t.Error("control: snowplow was never reached with authn's token, so this proved nothing about the exchange")
	}
}

// (a), tighter: the CRD appears between two passes and is looked for on the very next one, so nothing
// remembers that it was missing. It is also installed WITHOUT waiting for it to serve.
func TestEnvtest_642_CRDJustAppearedIsSeenOnTheNextPass(t *testing.T) {
	c := start642(t)
	b := newBench642(t, c, c)
	for i := 0; i < 2; i++ {
		_, _ = b.pass()
	}
	b.wantCond("authn not installed", "False/AuthnNotInstalled")
	c.installAuthnCRD(t, false)
	b.settle(8)
	b.wantCond("recovered", "True/Applied")

	// The differential: with the CRD serving, ONE pass is enough to write the mapping.
	c.deleteMapping(t, self642Name)
	_, _ = b.pass()
	if c.mapping(t, self642Name) == nil {
		t.Error("the mapping was not written on the first pass")
	}
}

// (b), (c): a mapping deleted later is written again on the next pass: core-provider's own, the
// controller's, and one whose spec no longer names core-provider's ServiceAccount.
func TestEnvtest_642_DeletedMappingIsRewritten(t *testing.T) {
	// One apiserver each: an arm that wedges must not decide the next one.
	settled := func(t *testing.T) (*cluster642, *bench642) {
		c := start642(t)
		c.installAuthnCRD(t, true)
		b := newBench642(t, c, c)
		b.settle(5)
		b.wantCond("settled", "True/Applied")
		return c, b
	}
	t.Run("core-provider's own", func(t *testing.T) {
		c, b := settled(t)
		c.deleteMapping(t, self642Name)
		if _, err := b.pass(); err != nil {
			t.Errorf("the pass after the delete: %v", err)
		}
		if c.mapping(t, self642Name) == nil {
			t.Fatalf("core-provider's mapping was not rewritten; synced=%s %q", b.cond("Synced"), b.message("Synced"))
		}
		b.settle(2)
		b.wantCond("after", "True/Applied")
	})
	t.Run("core-provider's own, re-pointed", func(t *testing.T) {
		c, b := settled(t)
		m := c.mapping(t, self642Name)
		_ = unstructured.SetNestedField(m.Object, "someone-else", "spec", "serviceAccountRef", "name")
		m.SetLabels(map[string]string{"kept": "yes"})
		if _, err := c.raw.Resource(self642GVR).Namespace(self642NS).Update(context.Background(), m, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		_, _ = b.pass()
		m = c.mapping(t, self642Name)
		if got, _, _ := unstructured.NestedString(m.Object, "spec", "serviceAccountRef", "name"); got != self642Name {
			t.Errorf("serviceAccountRef.name = %q, want it corrected to %q", got, self642Name)
		}
		if m.GetLabels()["kept"] != "yes" {
			t.Errorf("labels = %v: correcting the spec dropped another writer's metadata", m.GetLabels())
		}
		b.settle(2)
		b.wantCond("after", "True/Applied")
	})
	t.Run("the controller's", func(t *testing.T) {
		c, b := settled(t)
		c.deleteMapping(t, self642Controller)
		if _, err := b.pass(); err != nil {
			t.Errorf("the pass after the delete: %v", err)
		}
		if c.mapping(t, self642Controller) == nil {
			t.Fatalf("the controller's mapping was not rewritten; ready=%s %q", b.cond("Ready"), b.message("Ready"))
		}
		b.settle(2)
		b.wantCond("after", "True/Applied")
	})
}

// Create and Update ensure it themselves: Observe returns before its own ensure when the CRD does not
// exist yet (Create follows), and when the generated CRD changed (Update follows). One pass is enough in
// both, so neither leaves it to the next Observe.
func TestEnvtest_642_CreateAndUpdateEnsureItThemselves(t *testing.T) {
	c := start642(t)
	c.installAuthnCRD(t, true)
	b := newBench642(t, c, c)
	if _, err := b.pass(); err != nil {
		t.Errorf("Create's pass: %v", err)
	}
	if c.mapping(t, self642Name) == nil {
		t.Error("Create did not write core-provider's mapping")
	}
	b.settle(5)

	c.deleteMapping(t, self642Name)
	cd := b.cd()
	cd.Spec.StatusDataTemplate = []compositiondefinitionsv1alpha1.StatusFieldMapping{{ForPath: "added", Expression: "${ .spec }"}}
	if err := c.cl.Update(context.Background(), cd); err != nil {
		t.Fatal(err)
	}
	if _, err := b.pass(); err != nil {
		t.Errorf("Update's pass: %v", err)
	}
	if c.mapping(t, self642Name) == nil {
		t.Error("Update did not write core-provider's mapping")
	}
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := c.cl.Get(context.Background(), types.NamespacedName{Name: "contents.composition.krateo.io"}, crd); err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(crd); !strings.Contains(string(b), `"added"`) {
		t.Fatal("premise: Update did not run, so it proved nothing")
	}
	b.settle(3)
}

// authn answers NoMapping for a while after the mapping is written (its cache has not seen it). The
// passes fail, so the runtime retries them, and the first one authn accepts recovers.
func TestEnvtest_642_NoMappingRightAfterTheWriteRecovers(t *testing.T) {
	c := start642(t)
	c.installAuthnCRD(t, true)
	b := newBench642(t, c, c)
	b.settle(5)
	c.deleteMapping(t, self642Name)
	b.authn.lag.Store(2)
	refusedBefore := b.authn.noMapping.Load()
	for i := 1; i <= 2; i++ {
		_, err := b.pass()
		if err == nil || !strings.Contains(err.Error(), "NoMapping") {
			t.Fatalf("pass %d while authn lags: err=%v, want NoMapping returned so the runtime retries", i, err)
		}
		if got := b.cond("ApiRefRBACComplete"); got != "False/Unauthorized" {
			t.Errorf("pass %d: ApiRefRBACComplete = %s, want False/Unauthorized", i, got)
		}
		// The mapping IS live, whatever authn has seen of it.
		b.wantCond("mapping written, authn lagging", "True/Applied")
	}
	if got := b.authn.noMapping.Load() - refusedBefore; got != 2 {
		t.Fatalf("control: authn refused %d exchanges, want 2", got)
	}
	b.settle(2)
}

// (d) A definition that may not be acted on writes nothing, while its mapping is missing. CONTROL in
// each arm: lifting the restriction writes it on the next pass, so the silence was the restriction's.
func TestEnvtest_642_WithheldDefinitionWritesNothing(t *testing.T) {
	c := start642(t)
	c.installAuthnCRD(t, true)
	b := newBench642(t, c, c)
	b.settle(5)

	for _, tc := range []struct{ name, key, value string }{
		{"observe-only", "krateo.io/management-policy", "observe"},
		{"observe and create, update withheld", "krateo.io/management-policy", "observe,create"},
		{"a policy that does not parse", "krateo.io/management-policy", "obsreve"},
		{"paused", "krateo.io/paused", "true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b.in(t)
			b.annotate(map[string]string{tc.key: tc.value})
			c.deleteMapping(t, self642Name)
			c.take("")
			for i := 0; i < 2; i++ {
				_, _ = b.pass()
			}
			if w := c.take(self642Name); len(w) != 0 {
				t.Errorf("write requests for core-provider's mapping = %v, want none, dry runs included", w)
			}
			if c.mapping(t, self642Name) != nil {
				t.Error("the mapping was written")
			}
			if tc.key != "krateo.io/paused" { // a paused pass observes nothing, and says nothing
				if got := b.cond("AuthnMappingApplied"); strings.HasPrefix(got, "True") {
					t.Errorf("AuthnMappingApplied = %s while core-provider's mapping is missing", got)
				}
			}
			b.annotate(map[string]string{tc.key: ""})
			_, _ = b.pass()
			if c.mapping(t, self642Name) == nil {
				t.Fatal("control: the mapping was not written once the restriction was lifted")
			}
			b.settle(3)
		})
	}

	// The runtime's pause check reads the cached copy, and Observe re-reads the definition: a pause set
	// in between reaches Observe itself.
	t.Run("paused, seen only by Observe's own re-read", func(t *testing.T) {
		b.in(t)
		stale := b.cd()
		b.annotate(map[string]string{"krateo.io/paused": "true"})
		c.deleteMapping(t, self642Name)
		c.take("")
		_, _ = b.ext.Observe(context.Background(), stale)
		if stale.GetAnnotations()["krateo.io/paused"] != "true" {
			t.Fatal("premise: Observe did not re-read the definition")
		}
		if w := c.take(self642Name); len(w) != 0 || c.mapping(t, self642Name) != nil {
			t.Errorf("write requests = %v, mapping present = %v: a paused definition wrote", w, c.mapping(t, self642Name) != nil)
		}
		b.annotate(map[string]string{"krateo.io/paused": ""})
		b.settle(3)
	})
}

// (e) A remote definition: core-provider's own mapping on the hub and never on the spoke, the
// controller's on the spoke and never on the hub. The spoke HAS authn's CRD and the hub does not at
// first, so a probe of the wrong cluster writes into a NoMatch or reports the wrong state.
func TestEnvtest_642_RemoteWritesOwnMappingOnTheHub(t *testing.T) {
	hub, spoke := start642(t), start642(t)
	spoke.installAuthnCRD(t, true)
	b := newBench642(t, hub, spoke)
	for i := 0; i < 3; i++ {
		if _, err := b.pass(); err == nil {
			t.Fatal("no error while the hub has no authn")
		}
	}
	b.wantCond("authn on the spoke only", "False/AuthnNotInstalled")
	if m := b.message("AuthnMappingApplied"); !strings.Contains(m, "the management cluster serves no") {
		t.Errorf("message = %q, want it to name the management cluster", m)
	}
	hub.installAuthnCRD(t, true)
	b.settle(5)
	b.wantCond("both", "True/Applied")
	for _, tc := range []struct {
		where string
		c     *cluster642
		name  string
		want  bool
	}{
		{"hub", hub, self642Name, true}, {"spoke", spoke, self642Name, false},
		{"hub", hub, self642Controller, false}, {"spoke", spoke, self642Controller, true},
	} {
		if got := tc.c.mapping(t, tc.name) != nil; got != tc.want {
			t.Errorf("mapping %s on the %s: present = %v, want %v", tc.name, tc.where, got, tc.want)
		}
	}
	// Deleted on the hub: rewritten on the hub.
	hub.deleteMapping(t, self642Name)
	if _, err := b.pass(); err != nil {
		t.Errorf("the pass after the delete: %v", err)
	}
	if hub.mapping(t, self642Name) == nil || spoke.mapping(t, self642Name) != nil {
		t.Errorf("after the delete: hub present = %v, spoke present = %v", hub.mapping(t, self642Name) != nil, spoke.mapping(t, self642Name) != nil)
	}
}

// (f) Steady state: no write request for core-provider's mapping at all, dry runs included, and its
// resourceVersion does not move. CONTROL: the same instrument sees the write that follows a delete.
func TestEnvtest_642_SteadyStateSendsNoWrite(t *testing.T) {
	c := start642(t)
	c.installAuthnCRD(t, true)
	b := newBench642(t, c, c)
	b.settle(5)
	rv := c.mapping(t, self642Name).GetResourceVersion()
	c.take("")
	for i := 0; i < 5; i++ {
		if _, err := b.pass(); err != nil {
			t.Fatalf("steady pass: %v", err)
		}
	}
	c.mu.Lock()
	all := len(c.writes)
	c.mu.Unlock()
	if w := c.take(self642Name); len(w) != 0 {
		t.Errorf("5 steady passes sent %v for core-provider's mapping, want nothing", w)
	}
	if got := c.mapping(t, self642Name).GetResourceVersion(); got != rv {
		t.Errorf("resourceVersion moved %s -> %s", rv, got)
	}
	if all == 0 {
		t.Error("control: the recorder saw no mapping request at all, not even the controller mapping's dry runs")
	}
	// The controller's live mapping is only dry-run in a steady pass.
	c.deleteMapping(t, self642Name)
	_, _ = b.pass()
	if w := c.take(self642Name); len(w) != 1 || w[0] != "POST "+self642Name {
		t.Errorf("control: after a delete the recorder saw %v, want one real create", w)
	}
}

// A mapping that cannot be READ is neither missing nor present: the pass fails and is retried, nothing
// is written, and the condition claims nothing. Differentials: the same definition before and after.
func TestEnvtest_642_UnreadableMappingIsNotMissing(t *testing.T) {
	c := start642(t)
	c.installAuthnCRD(t, true)
	b := newBench642(t, c, c)
	b.settle(5)
	for _, tc := range []struct {
		arm, name string
		// afterPut refuses only the reads that follow a PUT of the mapping: the dry run's own read and
		// write succeed, and the read the condition is computed from fails.
		afterPut bool
	}{
		{"core-provider's own", self642Name, false},
		{"the controller's", self642Controller, false},
		{"the controller's, after the dry run", self642Controller, true},
	} {
		t.Run(tc.arm, func(t *testing.T) {
			b.in(t)
			name, put := tc.name, false
			b.wantCond("before", "True/Applied")
			c.take("")
			c.setFail(func(r *http.Request) *http.Response {
				if !strings.HasSuffix(r.URL.Path, "/"+name) {
					return nil
				}
				put = put || r.Method == http.MethodPut
				if r.Method != http.MethodGet || (tc.afterPut && !put) {
					return nil
				}
				body := `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Forbidden","message":"reads refused by the test","code":403}`
				return &http.Response{StatusCode: 403, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}
			})
			_, err := b.pass()
			c.setFail(nil)
			if err == nil || !strings.Contains(err.Error(), "reads refused by the test") {
				t.Errorf("err = %v, want the read failure returned", err)
			}
			for _, w := range c.take(name) {
				if !strings.HasSuffix(w, "dryRun") {
					t.Errorf("a real write was sent: %q", w)
				}
			}
			b.wantCond("unreadable", "Unknown/NotObserved")
			// Only the mapping whose read failed is blamed for it: one entry per mapping, joined by "; ".
			blamed := ""
			for _, entry := range strings.Split(b.message("AuthnMappingApplied"), "; ") {
				if strings.Contains(entry, "reads refused by the test") {
					blamed += map[bool]string{true: "controller's ", false: "own "}[strings.HasPrefix(entry, "the authn mapping of this definition's controller")]
				}
			}
			if want := map[bool]string{true: "own ", false: "controller's "}[strings.HasPrefix(tc.arm, "core-provider's own")]; blamed != want {
				t.Errorf("message %q blames %q, want %q", b.message("AuthnMappingApplied"), blamed, want)
			}
			b.settle(2)
			b.wantCond("after", "True/Applied")
		})
	}
}

//go:build envtest

package composition

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/krateo-platformops/composition-dynamic-controller/internal/tools/archive"
	"github.com/krateo-platformops/plumbing/kubeutil/event"
	"github.com/krateo-platformops/unstructured-runtime/pkg/controller"
	"github.com/krateo-platformops/unstructured-runtime/pkg/logging"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"
	corev1 "k8s.io/api/core/v1"
	apixv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// core-provider#597: does Observe write to an existing release when the composition's
// krateo.io/management-policy withholds update? A differential over the real Observe on a real
// apiserver with real helm storage: two arms (a desired-state change, an out-of-band drift), each
// under `observe` and, as the CONTROL, under the default policy. The control must show the write,
// or a "no write" under `observe` would be an instrument failure.

const p597App = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: app\n  namespace: {{ .Release.Namespace }}\ndata:\n  k: {{ .Values.k | quote }}\n"

// p597Deployment never becomes ready on envtest, which runs no controllers: the unhealthy child the
// precedence arm needs.
const p597Deployment = "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\n  namespace: {{ .Release.Namespace }}\nspec:\n  replicas: 1\n" +
	"  selector:\n    matchLabels: {app: web}\n  template:\n    metadata:\n      labels: {app: web}\n" +
	"    spec:\n      containers:\n      - name: web\n        image: registry.invalid/web:1\n"

func serveP597Chart(t *testing.T) string { return serveP597ChartOf(t, "0.1.0", false) }

func serveP597ChartOf(t *testing.T, version string, withDeployment bool) string {
	t.Helper()
	templates := map[string]string{"app.yaml": p597App}
	if withDeployment {
		templates["web.yaml"] = p597Deployment
	}
	return serveP597Templates(t, version, templates)
}

// serveP597Templates serves chart p597 at version with the given templates.
func serveP597Templates(t *testing.T, version string, templates map[string]string) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	files := map[string]string{"Chart.yaml": "apiVersion: v2\nname: p597\nversion: " + version + "\n"}
	for name, body := range templates {
		files["templates/"+name] = body
	}
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: "p597/" + name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	tgz := buf.Bytes()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(tgz)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/p597-" + version + ".tgz"
}

// p597Getter serves one chart version; the definition's version is the same, so this controller is
// never retiring.
type p597Getter struct{ url, version string }

func (g p597Getter) info() *archive.Info {
	v := g.version
	if v == "" {
		v = "0.1.0"
	}
	return &archive.Info{URL: g.url, Version: v, DefinitionVersion: v,
		CompositionDefinitionInfo: &archive.CompositionDefinitionInfo{Name: "fireworksapp", Namespace: "krateo-system",
			GVR: schema.GroupVersionResource{Group: "core.krateo.io", Version: "v1alpha1", Resource: "compositiondefinitions"}}}
}
func (g p597Getter) Get(*unstructured.Unstructured) (*archive.Info, error) {
	i := g.info()
	i.Auth = &archive.Auth{}
	return i, nil
}
func (g p597Getter) Locate(*unstructured.Unstructured) (*archive.Info, error) { return g.info(), nil }
func (g p597Getter) WithLogger(logging.Logger) archive.Getter                 { return g }

// p597Pluralizer maps the composition kind to its CRD and any other kind to its lowercase plural.
type p597Pluralizer struct{}

func (p597Pluralizer) GVKtoGVR(gvk schema.GroupVersionKind) (schema.GroupVersionResource, error) {
	if gvk.Group == "composition.krateo.io" {
		return schema.GroupVersionResource{Group: gvk.Group, Version: gvk.Version, Resource: "fireworks"}, nil
	}
	return schema.GroupVersionResource{Group: gvk.Group, Version: gvk.Version, Resource: strings.ToLower(gvk.Kind) + "s"}, nil
}

func TestEnvtest_597_ObserveWritesUnderAWithheldUpdate(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via setup-envtest")
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	ctx := context.Background()
	kube := kubernetes.NewForConfigOrDie(cfg)
	dyn := dynamic.NewForConfigOrDie(cfg)

	gvr := schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v0-1-0", Resource: "fireworks"}
	preserve := true
	if _, err := apiextclient.NewForConfigOrDie(cfg).ApiextensionsV1().CustomResourceDefinitions().Create(ctx, &apixv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: gvr.Resource + "." + gvr.Group},
		Spec: apixv1.CustomResourceDefinitionSpec{
			Group: gvr.Group,
			Names: apixv1.CustomResourceDefinitionNames{Plural: gvr.Resource, Singular: "firework", Kind: "Firework", ListKind: "FireworkList"},
			Scope: apixv1.NamespaceScoped,
			Versions: []apixv1.CustomResourceDefinitionVersion{{
				Name: gvr.Version, Served: true, Storage: true,
				Schema:       &apixv1.CustomResourceValidation{OpenAPIV3Schema: &apixv1.JSONSchemaProps{Type: "object", XPreserveUnknownFields: &preserve}},
				Subresources: &apixv1.CustomResourceSubresources{Status: &apixv1.CustomResourceSubresourceStatus{}},
			}},
		},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := waitServed(cfg, gvr); err != nil {
		t.Fatal(err)
	}
	// The production shape: a discovery-backed REST mapper. Without one every child is taken for
	// cluster-scoped, its namespace is dropped from the inventory, and the rollup reads it as absent.
	groupResources, err := restmapper.GetAPIGroupResources(discovery.NewDiscoveryClientForConfigOrDie(cfg))
	if err != nil {
		t.Fatal(err)
	}
	mapper := restmapper.NewDiscoveryRESTMapper(groupResources)
	inspector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"group":"","version":"v1","resource":"configmaps","verbs":["get","list","watch","create","update","patch","delete"]},` +
			`{"group":"apps","version":"v1","resource":"deployments","verbs":["get","list","watch","create","update","patch","delete"]}]`))
	}))
	t.Cleanup(inspector.Close)
	url := serveP597Chart(t)
	url020 := serveP597ChartOf(t, "0.2.0", false)
	urlDep := serveP597ChartOf(t, "0.1.0", true)

	type outcome struct {
		revBefore, revAfter int
		cmBefore, cmAfter   string
		observeErr          error
		obs                 controller.ExternalObservation
		ready               map[string]any // the composition's Ready condition after Observe
		chartVersion        string         // status.helmChartVersion after Observe
		rolesBefore         int            // derived ClusterRoles/ClusterRoleBindings present just before Observe
		rolesAfter          int
	}
	// clusterRBAC is the set of ClusterRole and ClusterRoleBinding names; the derived RBAC for this
	// chart is cluster-scoped (the inspector stub names no namespace).
	clusterRBAC := func(t *testing.T) map[string]bool {
		out := map[string]bool{}
		cr, err := kube.RbacV1().ClusterRoles().List(ctx, metav1.ListOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range cr.Items {
			out["ClusterRole/"+r.Name] = true
		}
		crb, err := kube.RbacV1().ClusterRoleBindings().List(ctx, metav1.ListOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range crb.Items {
			out["ClusterRoleBinding/"+b.Name] = true
		}
		return out
	}
	readyOf := func(u *unstructured.Unstructured) map[string]any {
		conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
		for _, c := range conds {
			if m, ok := c.(map[string]any); ok && m["type"] == "Ready" {
				return m
			}
		}
		return nil
	}
	// run installs the release under the default policy, then sets policy and makes the arm's change:
	// "change" (values), "version" (chart 0.1.0 -> 0.2.0), "drift" (a child edited out of band), or
	// "dropRoles" (the derived Roles deleted, for the RBAC refresh). chartURL picks the chart.
	run := func(t *testing.T, ns, policy, arm string, chartURL ...string) outcome {
		installURL := url
		if len(chartURL) > 0 {
			installURL = chartURL[0]
		}
		if _, err := kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		h := &handler{
			kubeconfig:        cfg,
			pluralizer:        p597Pluralizer{},
			packageInfoGetter: p597Getter{url: installURL},
			chartInspectorUrl: inspector.URL,
			saName:            "cdc",
			saNamespace:       ns,
			eventRecorder:     *event.NewAPIRecorder(events.NewFakeRecorder(100)),
			mapper:            mapper,
		}
		mg := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": gvr.GroupVersion().String(), "kind": "Firework",
			"metadata": map[string]any{"name": "fw", "namespace": ns},
			"spec":     map[string]any{"k": "v1"},
		}}
		created, err := dyn.Resource(gvr).Namespace(ns).Create(ctx, mg, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		// Installed under the default policy, as a live composition would have been.
		if err := h.Create(ctx, created); err != nil {
			t.Fatalf("premise: Create must install the release: %v", err)
		}
		cur, err := dyn.Resource(gvr).Namespace(ns).Get(ctx, "fw", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		relName := cur.GetLabels()["krateo.io/release-name"]
		if relName == "" {
			t.Fatalf("premise: Create stamped no krateo.io/release-name label: %v", cur.GetLabels())
		}
		st := storage.Init(driver.NewSecrets(kube.CoreV1().Secrets(ns)))
		rev := func() int {
			r, err := st.Last(relName)
			if err != nil {
				t.Fatalf("reading release %s: %v", relName, err)
			}
			return r.Version
		}
		cm := func() string {
			c, err := kube.CoreV1().ConfigMaps(ns).Get(ctx, "app", metav1.GetOptions{})
			if err != nil {
				t.Fatalf("reading child: %v", err)
			}
			return c.Data["k"]
		}

		// Then the policy is set (unstructured-runtime D6 reacts at once to this edit).
		if policy != "" {
			ann := cur.GetAnnotations()
			if ann == nil {
				ann = map[string]string{}
			}
			ann["krateo.io/management-policy"] = policy
			cur.SetAnnotations(ann)
		}
		switch arm {
		case "change":
			cur.Object["spec"] = map[string]any{"k": "v2"}
		case "version":
			h.packageInfoGetter = p597Getter{url: url020, version: "0.2.0"}
		case "drift", "dropRoles":
		}
		if cur, err = dyn.Resource(gvr).Namespace(ns).Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		if arm == "drift" {
			c, err := kube.CoreV1().ConfigMaps(ns).Get(ctx, "app", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			c.Data["k"] = "tampered"
			if _, err := kube.CoreV1().ConfigMaps(ns).Update(ctx, c, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
		}
		// The derived RBAC here is cluster-scoped and named after the composition ("fw-..."); every arm's
		// composition is "fw", so these are the objects any arm's Observe would refresh.
		var derived []string
		for k := range clusterRBAC(t) {
			if _, name, _ := strings.Cut(k, "/"); strings.HasPrefix(name, "fw-") {
				derived = append(derived, k)
			}
		}
		present := func() int {
			now, n := clusterRBAC(t), 0
			for _, k := range derived {
				if now[k] {
					n++
				}
			}
			return n
		}
		if arm == "dropRoles" {
			if len(derived) == 0 {
				t.Fatal("premise: Create applied no derived RBAC, so a refresh could not be seen")
			}
			for _, k := range derived {
				kind, name, _ := strings.Cut(k, "/")
				var derr error
				if kind == "ClusterRole" {
					derr = kube.RbacV1().ClusterRoles().Delete(ctx, name, metav1.DeleteOptions{})
				} else {
					derr = kube.RbacV1().ClusterRoleBindings().Delete(ctx, name, metav1.DeleteOptions{})
				}
				if derr != nil {
					t.Fatal(derr)
				}
			}
		}
		var o outcome
		o.revBefore, o.cmBefore = rev(), cm()
		o.rolesBefore = present()
		if got := cur.GetAnnotations()["krateo.io/management-policy"]; got != policy {
			t.Fatalf("premise: the composition must carry policy %q, has %q", policy, got)
		}
		o.obs, o.observeErr = h.Observe(ctx, cur)
		o.revAfter, o.cmAfter = rev(), cm()
		o.rolesAfter = present()
		after, err := dyn.Resource(gvr).Namespace(ns).Get(ctx, "fw", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		o.ready = readyOf(after)
		o.chartVersion, _, _ = unstructured.NestedString(after.Object, "status", "helmChartVersion")
		t.Logf("policy=%q arm=%s: upToDate=%v Ready=%v/%v %q helmChartVersion=%q derived RBAC %d of %d -> %d",
			policy, arm, o.obs.ResourceUpToDate, o.ready["status"], o.ready["reason"], o.ready["message"], o.chartVersion, o.rolesBefore, len(derived), o.rolesAfter)
		t.Logf("policy=%q arm=%s: revision %d -> %d, child k %q -> %q, Observe err=%v",
			policy, arm, o.revBefore, o.revAfter, o.cmBefore, o.cmAfter, o.observeErr)
		return o
	}

	t.Run("control: default policy, desired-state change is applied", func(t *testing.T) {
		o := run(t, "d-change", "", "change")
		if o.revAfter <= o.revBefore || o.cmAfter != "v2" {
			t.Errorf("control failed: revision %d -> %d, child %q; the instrument does not see Observe's write", o.revBefore, o.revAfter, o.cmAfter)
		}
	})
	t.Run("control: default policy, drift is healed", func(t *testing.T) {
		o := run(t, "d-drift", "", "drift")
		if o.cmBefore != "tampered" || o.cmAfter != "v1" {
			t.Errorf("control failed: child %q -> %q, want tampered -> v1", o.cmBefore, o.cmAfter)
		}
	})
	t.Run("observe: a desired-state change must NOT be applied", func(t *testing.T) {
		o := run(t, "o-change", "observe", "change")
		if o.revAfter != o.revBefore || o.cmAfter != "v1" {
			t.Errorf("#597: under management-policy observe, Observe wrote the change: revision %d -> %d, child %q -> %q", o.revBefore, o.revAfter, o.cmBefore, o.cmAfter)
		}
	})
	t.Run("observe: drift must NOT be healed", func(t *testing.T) {
		o := run(t, "o-drift", "observe", "drift")
		if o.cmAfter != "tampered" || o.revAfter != o.revBefore {
			t.Errorf("#597: under management-policy observe, Observe healed the drift: revision %d -> %d, child %q -> %q", o.revBefore, o.revAfter, o.cmBefore, o.cmAfter)
		}
		// D4: reported, healthy, and not up to date, so the runtime takes its withheld-update branch.
		msg, _ := o.ready["message"].(string)
		if o.observeErr != nil || o.obs.ResourceUpToDate || o.ready["status"] != "True" || o.ready["reason"] != "UpdateWithheldByPolicy" ||
			!strings.Contains(msg, "would patch") || !strings.Contains(msg, "app") || !strings.Contains(msg, `"observe"`) {
			t.Errorf("drift under observe: err=%v upToDate=%v Ready=%v/%v %q; want Ready=True/UpdateWithheldByPolicy naming the policy and the child it would patch, upToDate=false",
				o.observeErr, o.obs.ResourceUpToDate, o.ready["status"], o.ready["reason"], msg)
		}
	})
	t.Run("observe: a values change is reported as withheld", func(t *testing.T) {
		o := run(t, "o-change-ready", "observe", "change")
		msg, _ := o.ready["message"].(string)
		if o.obs.ResourceUpToDate || o.ready["reason"] != "UpdateWithheldByPolicy" || !strings.Contains(msg, "is not applied") {
			t.Errorf("values change under observe: upToDate=%v Ready=%v %q; want UpdateWithheldByPolicy saying the change is not applied", o.obs.ResourceUpToDate, o.ready["reason"], msg)
		}
	})

	// A chart VERSION change, and what status.helmChartVersion records. CONTROL: the default policy
	// upgrades to 0.2.0 and records it; under observe the release stays at 0.1.0, and so does the
	// status, while Ready names both versions (installer: healthy must not read as arrived).
	t.Run("control: default policy, a chart version change is applied and recorded", func(t *testing.T) {
		o := run(t, "d-version", "", "version")
		if o.revAfter <= o.revBefore || o.chartVersion != "0.2.0" {
			t.Errorf("control: revision %d -> %d, helmChartVersion %q; want an upgrade recorded as 0.2.0", o.revBefore, o.revAfter, o.chartVersion)
		}
	})
	t.Run("observe: a chart version change is not applied, and the installed version is recorded", func(t *testing.T) {
		o := run(t, "o-version", "observe", "version")
		msg, _ := o.ready["message"].(string)
		if o.revAfter != o.revBefore || o.chartVersion != "0.1.0" || o.ready["reason"] != "UpdateWithheldByPolicy" ||
			!strings.Contains(msg, "0.1.0") || !strings.Contains(msg, "0.2.0") {
			t.Errorf("version change under observe: revision %d -> %d, helmChartVersion %q, Ready %v %q; want no upgrade, 0.1.0 recorded, both versions named",
				o.revBefore, o.revAfter, o.chartVersion, o.ready["reason"], msg)
		}
	})

	// The derived-RBAC refresh still runs under a withheld update (Diego, 2026-11-21).
	t.Run("observe: the derived RBAC is still refreshed", func(t *testing.T) {
		o := run(t, "o-rbac", "observe", "dropRoles")
		if o.rolesBefore != 0 || o.rolesAfter == 0 || o.revAfter != o.revBefore {
			t.Errorf("RBAC under observe: derived RBAC %d -> %d, revision %d -> %d; want the deleted derived RBAC re-applied and the release untouched", o.rolesBefore, o.rolesAfter, o.revBefore, o.revAfter)
		}
	})

	// PRECEDENCE (Diego, 2026-11-21): a withheld update replaces only the healthy verdict. The chart
	// has a Deployment, which never becomes ready on envtest, so the rollup is not healthy: Ready keeps
	// the rollup's reason, and its message notes the withheld update. The healthy control is the drift
	// arm above, which reads UpdateWithheldByPolicy.
	t.Run("observe: an unhealthy rollup keeps its reason, and says updates are withheld", func(t *testing.T) {
		o := run(t, "o-precedence", "observe", "drift", urlDep)
		msg, _ := o.ready["message"].(string)
		if o.ready["status"] != "False" || o.ready["reason"] == "UpdateWithheldByPolicy" || o.ready["reason"] == "Available" ||
			!strings.Contains(msg, "withholds update") || o.cmAfter != "tampered" {
			t.Errorf("precedence: Ready=%v/%v %q, child %q; want the rollup's not-ready reason kept, the message noting the withheld update, nothing healed",
				o.ready["status"], o.ready["reason"], msg, o.cmAfter)
		}
	})
}

// D4 (#597): a composition whose release is not installed, under a policy that withholds create, is
// not installed, and Ready says why as a refusal. CONTROL: the default policy reports the same absence
// with no condition, so the runtime queues the Create.
func TestEnvtest_597_CreateWithheld(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via setup-envtest")
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	ctx := context.Background()
	kube := kubernetes.NewForConfigOrDie(cfg)
	url := serveP597Chart(t)
	for _, tc := range []struct {
		policy   string
		withheld bool
	}{{"", false}, {"observe", true}, {"observe-delete", true}, {"observe-create", true}} {
		t.Run("policy="+tc.policy, func(t *testing.T) {
			ns := "cw-" + strings.ReplaceAll(strings.ReplaceAll(tc.policy, ",", "-"), "observe", "o")
			if tc.policy == "" {
				ns = "cw-default"
			}
			if _, err := kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			h := &handler{kubeconfig: cfg, pluralizer: p597Pluralizer{}, packageInfoGetter: p597Getter{url: url},
				saName: "cdc", saNamespace: ns, eventRecorder: *event.NewAPIRecorder(events.NewFakeRecorder(100))}
			mg := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "composition.krateo.io/v0-1-0", "kind": "Firework",
				"metadata": map[string]any{"name": "fw", "namespace": ns, "uid": "u-" + ns,
					"labels": map[string]any{"krateo.io/release-name": "fw"}},
				"spec": map[string]any{"k": "v1"},
			}}
			if tc.policy != "" {
				mg.SetAnnotations(map[string]string{"krateo.io/management-policy": tc.policy})
			}
			obs, err := h.Observe(ctx, mg)
			if err != nil {
				t.Fatalf("Observe: %v", err)
			}
			secrets, err := kube.CoreV1().Secrets(ns).List(ctx, metav1.ListOptions{LabelSelector: "owner=helm"})
			if err != nil {
				t.Fatal(err)
			}
			var ready *metav1.Condition
			for i := range obs.Conditions {
				if obs.Conditions[i].Type == "Ready" {
					ready = &obs.Conditions[i]
				}
			}
			t.Logf("policy=%q: exists=%v ready=%+v helm releases=%d", tc.policy, obs.ResourceExists, ready, len(secrets.Items))
			if obs.ResourceExists || len(secrets.Items) != 0 {
				t.Fatalf("premise: absent and nothing installed by Observe; exists=%v releases=%d", obs.ResourceExists, len(secrets.Items))
			}
			if !tc.withheld {
				if ready != nil {
					t.Errorf("control: the default policy returns no Ready (the runtime creates), got %+v", ready)
				}
				return
			}
			if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != "CreateWithheldByPolicy" ||
				!strings.Contains(ready.Message, `"`+tc.policy+`"`) || !strings.Contains(ready.Message, "add create") {
				t.Errorf("policy %q: Ready=%+v; want False/CreateWithheldByPolicy naming the policy and how to allow create", tc.policy, ready)
			}
			if tc.policy == "observe-create" && !strings.Contains(ready.Message, "does not parse") {
				t.Errorf("an invalid policy's message must say it does not parse: %q", ready.Message)
			}
		})
	}
}

// #597, the drift Observe cannot MEASURE under a withheld update (Diego, 2026-11-21: no new reason;
// Ready says why). The stored release names a custom kind whose CRD is then deleted, so helm's render
// needs the stored-manifest repair, which is a write: plumbing returns ErrStoredManifestRepairWithheld.
// Observe must report it, not fail on it, and write nothing.
func TestEnvtest_597_DriftUnassessable(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via setup-envtest")
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	ctx := context.Background()
	kube := kubernetes.NewForConfigOrDie(cfg)
	dyn := dynamic.NewForConfigOrDie(cfg)
	apix := apiextclient.NewForConfigOrDie(cfg)
	preserve := true
	crd := func(group, plural, kind, version string) *apixv1.CustomResourceDefinition {
		return &apixv1.CustomResourceDefinition{
			ObjectMeta: metav1.ObjectMeta{Name: plural + "." + group},
			Spec: apixv1.CustomResourceDefinitionSpec{
				Group: group, Scope: apixv1.NamespaceScoped,
				Names: apixv1.CustomResourceDefinitionNames{Plural: plural, Singular: strings.ToLower(kind), Kind: kind, ListKind: kind + "List"},
				Versions: []apixv1.CustomResourceDefinitionVersion{{
					Name: version, Served: true, Storage: true,
					Schema:       &apixv1.CustomResourceValidation{OpenAPIV3Schema: &apixv1.JSONSchemaProps{Type: "object", XPreserveUnknownFields: &preserve}},
					Subresources: &apixv1.CustomResourceSubresources{Status: &apixv1.CustomResourceSubresourceStatus{}},
				}},
			},
		}
	}
	gvr := schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v0-1-0", Resource: "fireworks"}
	widgets := schema.GroupVersionResource{Group: "p597.test", Version: "v1", Resource: "widgets"}
	for _, c := range []*apixv1.CustomResourceDefinition{crd(gvr.Group, gvr.Resource, "Firework", gvr.Version), crd(widgets.Group, widgets.Resource, "Widget", widgets.Version)} {
		if _, err := apix.ApiextensionsV1().CustomResourceDefinitions().Create(ctx, c, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	for _, g := range []schema.GroupVersionResource{gvr, widgets} {
		if err := waitServed(cfg, g); err != nil {
			t.Fatal(err)
		}
	}
	groupResources, err := restmapper.GetAPIGroupResources(discovery.NewDiscoveryClientForConfigOrDie(cfg))
	if err != nil {
		t.Fatal(err)
	}
	inspector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"group":"","version":"v1","resource":"configmaps","verbs":["get","list","watch","create","update","patch","delete"]}]`))
	}))
	t.Cleanup(inspector.Close)
	url := serveP597Templates(t, "0.1.0", map[string]string{
		"app.yaml":    p597App,
		"widget.yaml": "apiVersion: p597.test/v1\nkind: Widget\nmetadata:\n  name: w\n  namespace: {{ .Release.Namespace }}\nspec: {}\n",
	})

	const ns = "unassessable"
	if _, err := kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	h := &handler{kubeconfig: cfg, pluralizer: p597Pluralizer{}, packageInfoGetter: p597Getter{url: url},
		chartInspectorUrl: inspector.URL, saName: "cdc", saNamespace: ns,
		eventRecorder: *event.NewAPIRecorder(events.NewFakeRecorder(100)), mapper: restmapper.NewDiscoveryRESTMapper(groupResources)}
	created, err := dyn.Resource(gvr).Namespace(ns).Create(ctx, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": gvr.GroupVersion().String(), "kind": "Firework",
		"metadata": map[string]any{"name": "fw", "namespace": ns},
		"spec":     map[string]any{"k": "v1"},
	}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Create(ctx, created); err != nil {
		t.Fatalf("premise: Create must install the release with its Widget: %v", err)
	}
	cur, err := dyn.Resource(gvr).Namespace(ns).Get(ctx, "fw", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	relName := cur.GetLabels()["krateo.io/release-name"]
	st := storage.Init(driver.NewSecrets(kube.CoreV1().Secrets(ns)))
	before, err := st.Last(relName)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(before.Manifest, "kind: Widget") {
		t.Fatalf("premise: the stored manifest must name the Widget: %s", before.Manifest)
	}

	// The cluster stops serving the Widget kind; the stored release still names it.
	if err := apix.ApiextensionsV1().CustomResourceDefinitions().Delete(ctx, widgets.Resource+"."+widgets.Group, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(60 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		if _, err := dyn.Resource(widgets).Namespace(ns).List(ctx, metav1.ListOptions{Limit: 1}); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("premise: the Widget kind is still served after its CRD was deleted")
		}
	}
	cur.SetAnnotations(map[string]string{"krateo.io/management-policy": "observe"})
	if cur, err = dyn.Resource(gvr).Namespace(ns).Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	obs, oerr := h.Observe(ctx, cur)
	after, err := st.Last(relName)
	if err != nil {
		t.Fatal(err)
	}
	fw, err := dyn.Resource(gvr).Namespace(ns).Get(ctx, "fw", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var ready map[string]any
	conds, _, _ := unstructured.NestedSlice(fw.Object, "status", "conditions")
	for _, c := range conds {
		if m, ok := c.(map[string]any); ok && m["type"] == "Ready" {
			ready = m
		}
	}
	msg, _ := ready["message"].(string)
	t.Logf("observe + unserved stored kind: err=%v exists=%v revision %d -> %d, manifest still names Widget=%v, Ready=%v/%v %q",
		oerr, obs.ResourceExists, before.Version, after.Version, strings.Contains(after.Manifest, "kind: Widget"), ready["status"], ready["reason"], msg)
	if oerr != nil {
		t.Fatalf("drift that cannot be assessed is not a reconcile failure under a withheld update: %v", oerr)
	}
	if after.Version != before.Version || !strings.Contains(after.Manifest, "kind: Widget") {
		t.Errorf("the stored release was written (revision %d -> %d, Widget kept=%v): the repair must be withheld", before.Version, after.Version, strings.Contains(after.Manifest, "kind: Widget"))
	}
	if !strings.Contains(msg, "drift could not be assessed") || !strings.Contains(msg, "withholds update") {
		t.Errorf("Ready must say drift could not be assessed and why: %v/%v %q", ready["status"], ready["reason"], msg)
	}
}

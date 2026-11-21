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

	"github.com/krateo-platformops/composition-dynamic-controller/internal/tools/archive"
	"github.com/krateo-platformops/plumbing/kubeutil/event"
	"github.com/krateo-platformops/unstructured-runtime/pkg/logging"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"
	corev1 "k8s.io/api/core/v1"
	apixv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// core-provider#597: does Observe write to an existing release when the composition's
// krateo.io/management-policy withholds update? A differential over the real Observe on a real
// apiserver with real helm storage: two arms (a desired-state change, an out-of-band drift), each
// under `observe` and, as the CONTROL, under the default policy. The control must show the write,
// or a "no write" under `observe` would be an instrument failure.

const p597Chart = "apiVersion: v2\nname: p597\nversion: 0.1.0\n"
const p597App = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: app\ndata:\n  k: {{ .Values.k | quote }}\n"

func serveP597Chart(t *testing.T) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range map[string]string{"Chart.yaml": p597Chart, "templates/app.yaml": p597App} {
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
	return srv.URL + "/p597-0.1.0.tgz"
}

type p597Getter struct{ url string }

func (g p597Getter) info() *archive.Info {
	return &archive.Info{URL: g.url, Version: "0.1.0", DefinitionVersion: "0.1.0",
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
	inspector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"group":"","version":"v1","resource":"configmaps","verbs":["get","list","watch","create","update","patch","delete"]}]`))
	}))
	t.Cleanup(inspector.Close)
	url := serveP597Chart(t)

	type outcome struct {
		revBefore, revAfter int
		cmBefore, cmAfter   string
		observeErr          error
	}
	run := func(t *testing.T, ns, policy, arm string) outcome {
		if _, err := kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		h := &handler{
			kubeconfig:        cfg,
			pluralizer:        p597Pluralizer{},
			packageInfoGetter: p597Getter{url: url},
			chartInspectorUrl: inspector.URL,
			saName:            "cdc",
			saNamespace:       ns,
			eventRecorder:     *event.NewAPIRecorder(events.NewFakeRecorder(100)),
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
		case "drift":
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
		var o outcome
		o.revBefore, o.cmBefore = rev(), cm()
		if got := cur.GetAnnotations()["krateo.io/management-policy"]; got != policy {
			t.Fatalf("premise: the composition must carry policy %q, has %q", policy, got)
		}
		_, o.observeErr = h.Observe(ctx, cur)
		o.revAfter, o.cmAfter = rev(), cm()
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
	})
}

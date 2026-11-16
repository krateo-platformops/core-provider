//go:build envtest

package composition

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/krateo-platformops/composition-dynamic-controller/internal/tools/archive"
	"github.com/krateo-platformops/unstructured-runtime/pkg/controller"
	"github.com/krateo-platformops/unstructured-runtime/pkg/logging"
	corev1 "k8s.io/api/core/v1"
	apixv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// unreachableChartGetter names a chart nobody serves, so Create gets past every step before helm
// and the INSTALL itself fails.
type unreachableChartGetter struct{}

func (unreachableChartGetter) info() *archive.Info {
	return &archive.Info{URL: "oci://127.0.0.1:9/charts/nothing-here", Version: "0.1.0",
		CompositionDefinitionInfo: &archive.CompositionDefinitionInfo{Name: "fireworksapp", Namespace: "krateo-system",
			GVR: schema.GroupVersionResource{Group: "core.krateo.io", Version: "v1alpha1", Resource: "compositiondefinitions"}}}
}

// Get carries credentials (empty here), as the real getter's Get always does; Locate carries none.
func (g unreachableChartGetter) Get(*unstructured.Unstructured) (*archive.Info, error) {
	i := g.info()
	i.Auth = &archive.Auth{}
	return i, nil
}
func (g unreachableChartGetter) Locate(*unstructured.Unstructured) (*archive.Info, error) {
	return g.info(), nil
}
func (g unreachableChartGetter) WithLogger(logging.Logger) archive.Getter { return g }

// The other half of TestACreateThatFailsBeforeHelmIsNotCommitted: once the helm call has started a
// release may exist, so the failure must stay UNCLASSIFIED and unstructured-runtime confirms the
// release is absent before it installs again. Driven through the real Create against a real
// apiserver, with only the chart unreachable; the error must come from the install, so this test
// cannot pass by failing earlier.
func TestEnvtest_ACreateThatFailsInHelmIsLeftUnclassified(t *testing.T) {
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest apiserver: %v", err)
	}
	defer func() { _ = env.Stop() }()
	ctx := context.Background()

	gvr := schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1-0-0", Resource: "fireworks"}
	const ns = "krateo-system"
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
	if _, err := kubernetes.NewForConfigOrDie(cfg).CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := waitServed(cfg, gvr); err != nil {
		t.Fatal(err)
	}

	mg := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": gvr.GroupVersion().String(), "kind": "Firework",
		"metadata": map[string]any{"name": "fw", "namespace": ns},
		"spec":     map[string]any{"replicas": int64(1)},
	}}
	dyn := dynamic.NewForConfigOrDie(cfg)
	created, err := dyn.Resource(gvr).Namespace(ns).Create(ctx, mg, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	inspector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"group":"","version":"v1","resource":"configmaps","verbs":["get"]}]`))
	}))
	defer inspector.Close()

	h := &handler{
		kubeconfig:        cfg,
		pluralizer:        stubPluralizerForRBAC{},
		packageInfoGetter: unreachableChartGetter{},
		chartInspectorUrl: inspector.URL,
		saName:            "cdc",
		saNamespace:       ns,
	}
	err = h.Create(ctx, created)
	if err == nil {
		t.Fatal("Create installed a chart nobody serves")
	}
	if !strings.Contains(err.Error(), "installing helm chart") {
		t.Fatalf("Create failed before the install, so this test does not reach the helm call: %v", err)
	}
	if errors.Is(err, controller.ErrCreateNotCommitted) {
		t.Fatalf("a failure from the helm call was classified as not committed, so the runtime would re-install without confirming the release is absent: %v", err)
	}
}

// waitServed polls until the apiserver serves gvr: a CRD is accepted before it is served. It fails
// on timeout rather than proceeding, or the test would fail for that reason instead of its own.
func waitServed(cfg *rest.Config, gvr schema.GroupVersionResource) error {
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return err
	}
	var last error
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if _, last = dyn.Resource(gvr).List(context.Background(), metav1.ListOptions{Limit: 1}); last == nil {
			return nil
		}
	}
	return fmt.Errorf("%s not served after 60s: %w", gvr, last)
}

//go:build envtest

package crd

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/krateo-platformops/core-provider/internal/tools/crd/generation"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// core-provider#534 through the real write path. A served version generated before plumbing#186
// (its untyped items typed `object`, as v1.80.0 emitted them) takes today's looser render on the
// next apply, the object stored under it survives, and a list of strings, refused before, is
// admitted. The reverse direction (a tighter render) leaves the served schema alone.
func TestEnvtest_ApplyRefreshesALooserServedSpecAndNeverATighterOne(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Fatal("KUBEBUILDER_ASSETS is not set: this test exists to run against a real apiserver, and a skip would read as a pass")
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	sc := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(sc)
	_ = apiextensionsv1.AddToScheme(sc)
	cl, err := client.New(cfg, client.Options{Scheme: sc})
	if err != nil {
		t.Fatal(err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	const spec = `{"type":"object","properties":{"targetNamespaces":{"type":"array","items":{}}}}`
	gvk := schema.GroupVersionKind{Group: "composition.krateo.io", Version: "v0-20-0", Kind: "TrustManager"}
	looser, err := generation.GenerateCRD([]byte(spec), gvk)
	if err != nil {
		t.Fatal(err)
	}
	// The pre-fix render, measured: v1.80.0 typed the untyped items `object`.
	preserve := true
	narrowed := looser.DeepCopy()
	sp := narrowed.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	tn := sp.Properties["targetNamespaces"]
	tn.Items = &apiextensionsv1.JSONSchemaPropsOrArray{Schema: &apiextensionsv1.JSONSchemaProps{Type: "object", XPreserveUnknownFields: &preserve}}
	sp.Properties["targetNamespaces"] = tn
	narrowed.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"] = sp
	if reflect.DeepEqual(narrowed.Spec, looser.Spec) {
		t.Fatal("the narrowed and looser CRDs are identical: the refresh below would test nothing")
	}

	gvr, err := ApplyOrUpdateCRDDropping(ctx, cl, dyn, narrowed.DeepCopy(), nil)
	if err != nil {
		t.Fatalf("create the narrowed CRD: %v", err)
	}
	res := dyn.Resource(gvr).Namespace("default")
	obj := func(name string, items ...any) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": gvk.GroupVersion().String(), "kind": gvk.Kind,
			"metadata": map[string]any{"name": name, "namespace": "default"},
			"spec":     map[string]any{"targetNamespaces": items},
		}}
	}
	stored := obj("stored", map[string]any{"a": "b"})
	if err := eventuallyNil(func() error { _, err := res.Create(ctx, stored, metav1.CreateOptions{}); return err }); err != nil {
		t.Fatalf("store an object under the narrowed CRD: %v", err)
	}
	if _, err := res.Create(ctx, obj("strings-before", "cert-manager"), metav1.CreateOptions{}); err == nil {
		t.Fatal("control: the narrowed CRD admitted a list of strings, so the pre-state is not the defect")
	}

	// The refresh: the served version exists, the render is looser, so the apply writes it.
	if _, err := ApplyOrUpdateCRDDropping(ctx, cl, dyn, looser.DeepCopy(), nil); err != nil {
		t.Fatalf("apply the looser render: %v", err)
	}
	served := servedItemsType(t, cl, looser.Name, gvk.Version)
	if served != "" {
		t.Fatalf("the served version still types the items %q: the looser render was not written", served)
	}
	if err := eventuallyNil(func() error {
		_, err := res.Create(ctx, obj("strings-after", "cert-manager", "krateo-system"), metav1.CreateOptions{})
		return err
	}); err != nil {
		t.Fatalf("a list of strings is still refused after the refresh: %v", err)
	}
	got, err := res.Get(ctx, "stored", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Object["spec"], stored.Object["spec"]) {
		t.Errorf("the stored object changed across the refresh: got %v", got.Object["spec"])
	}
	got.SetLabels(map[string]string{"touched": "true"})
	if _, err := res.Update(ctx, got, metav1.UpdateOptions{}); err != nil {
		t.Errorf("the object stored under the narrowed schema is no longer writable: %v", err)
	}

	// The other direction: re-applying the narrowed render must NOT tighten the served schema back.
	if _, err := ApplyOrUpdateCRDDropping(ctx, cl, dyn, narrowed.DeepCopy(), nil); err != nil {
		t.Fatalf("apply the narrowed render: %v", err)
	}
	if served := servedItemsType(t, cl, looser.Name, gvk.Version); served != "" {
		t.Fatalf("a tighter render was written over the served schema (items type %q): stored strings would be refused", served)
	}
}

func servedItemsType(t *testing.T, cl client.Client, name, version string) string {
	t.Helper()
	var crd apiextensionsv1.CustomResourceDefinition
	if err := cl.Get(context.Background(), types.NamespacedName{Name: name}, &crd); err != nil {
		t.Fatal(err)
	}
	for _, v := range crd.Spec.Versions {
		if v.Name == version {
			return v.Schema.OpenAPIV3Schema.Properties["spec"].Properties["targetNamespaces"].Items.Schema.Type
		}
	}
	t.Fatalf("version %s is not served", version)
	return ""
}

func eventuallyNil(f func() error) error {
	var err error
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
		if err = f(); err == nil {
			return nil
		}
	}
	return err
}

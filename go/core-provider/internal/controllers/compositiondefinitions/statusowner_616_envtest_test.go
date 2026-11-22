//go:build envtest

package compositiondefinitions

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// core-provider#616 end to end through the REAL generated CRD: status.owner written as Observe
// writes it (the typed DefinitionReference) is admitted, survives structural pruning and reads back
// equal; its two fields are required; and clearing it (Observe on the owner) leaves it absent.
//
// CONTROL: an unknown sibling field under status.owner IS pruned, so the read-back comparison would
// see a field the schema does not carry.
func TestEnvtest_616_StatusOwnerRoundTripsTheCRD(t *testing.T) {
	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "..", "crds")}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	defer func() { _ = env.Stop() }()
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ri := dyn.Resource(schema.GroupVersionResource{Group: "core.krateo.io", Version: "v1alpha1", Resource: "compositiondefinitions"}).Namespace("default")
	ctx := context.Background()

	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "core.krateo.io/v1alpha1", "kind": "CompositionDefinition",
		"metadata": map[string]any{"name": "follower", "namespace": "default"},
		"spec":     map[string]any{"chart": map[string]any{"url": "oci://ghcr.io/example/charts/demo", "version": "1.0.0"}}}}
	cur, err := ri.Create(ctx, u, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	writeOwner := func(owner any) (*unstructured.Unstructured, error) {
		obj := cur.DeepCopy()
		if owner == nil {
			unstructured.RemoveNestedField(obj.Object, "status", "owner")
		} else if err := unstructured.SetNestedField(obj.Object, owner, "status", "owner"); err != nil {
			t.Fatal(err)
		}
		return ri.UpdateStatus(ctx, obj, metav1.UpdateOptions{})
	}
	asTyped := func(o *unstructured.Unstructured) *compositiondefinitionsv1alpha1.CompositionDefinition {
		var cd compositiondefinitionsv1alpha1.CompositionDefinition
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(o.Object, &cd); err != nil {
			t.Fatal(err)
		}
		return &cd
	}

	// The value Observe writes on a follower, through the typed field.
	want := compositiondefinitionsv1alpha1.DefinitionReference{Namespace: "krateo-system", Name: "owner-cd"}
	typed := asTyped(cur)
	typed.Status.Owner = &want
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(typed)
	if err != nil {
		t.Fatal(err)
	}
	owner, _, _ := unstructured.NestedMap(m, "status", "owner")
	owner["unexpected"] = "pruned" // CONTROL: not in the schema
	if cur, err = writeOwner(owner); err != nil {
		t.Fatalf("status.owner refused: %v", err)
	}
	got, err := ri.Get(ctx, "follower", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if g := asTyped(got).Status.Owner; g == nil || *g != want {
		t.Fatalf("status.owner read back %+v, want %+v", g, want)
	}
	if _, found, _ := unstructured.NestedString(got.Object, "status", "owner", "unexpected"); found {
		t.Fatalf("control: an unknown field under status.owner survived; the schema does not constrain it")
	}

	for _, missing := range []string{"namespace", "name"} {
		partial := map[string]any{"namespace": "krateo-system", "name": "owner-cd"}
		delete(partial, missing)
		_, err := writeOwner(partial)
		if err == nil || !strings.Contains(err.Error(), missing) {
			t.Fatalf("status.owner without %q: err = %v, want a refusal naming it", missing, err)
		}
	}

	// Observe on the owner clears it (nil): absent after the write.
	cur = got
	if cur, err = writeOwner(nil); err != nil {
		t.Fatalf("clearing status.owner: %v", err)
	}
	got, err = ri.Get(ctx, "follower", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, found, _ := unstructured.NestedMap(got.Object, "status", "owner"); found {
		t.Fatalf("status.owner still present after clearing: %v", got.Object["status"])
	}
}

//go:build e2e

// #235 apiserver-level proof. Runs only with -tags e2e against a real cluster (KUBECONFIG).
// Reuses e2eClients/waitCRDEstablished/compositionGroup from e2e_test.go (same package).
//
// It proves the webhookless nested-defaulting fix end-to-end: a chart values schema where an
// OPTIONAL parent object (spec.image) has a child (spec.image.tag) with a `default`. With the fix,
// crdgen synthesizes `default: {}` on the parent, so when a composition is created WITHOUT
// spec.image the apiserver materializes the parent and applies the nested default — no mutating
// webhook. Without the fix, spec.image.tag would be silently absent.
package crd

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	crdutils "github.com/krateo-platformops/core-provider/internal/tools/crd/generation"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// nestedDefaultSchema: spec.image is optional; spec.image.tag defaults to "latest"; spec.replicas
// defaults at top level. spec.guarded is optional with a REQUIRED non-defaulted child, to prove the
// guard leaves it un-materialized (no default: {}), so omitting it stays valid.
func nestedDefaultSchema() []byte {
	return []byte(`{
	  "type":"object",
	  "properties":{
	    "image":{"type":"object","properties":{"tag":{"type":"string","default":"latest"}}},
	    "guarded":{"type":"object","required":["mandatory"],"properties":{"mandatory":{"type":"string"}}},
	    "replicas":{"type":"integer","default":1}
	  }
	}`)
}

func TestE2E_NestedDefault235(t *testing.T) {
	if os.Getenv("KUBECONFIG") == "" {
		t.Skip("KUBECONFIG must be set")
	}
	ctx := context.Background()
	cl, dyn := e2eClients(t)

	kind := fmt.Sprintf("Nd%d", time.Now().UnixNano()%1000000)
	gvk := schema.GroupVersionKind{Group: compositionGroup, Version: "v1-0-0", Kind: kind}

	crd, err := crdutils.GenerateCRD(nestedDefaultSchema(), gvk)
	if err != nil {
		t.Fatalf("GenerateCRD: %v", err)
	}
	crdName := crd.Name
	t.Cleanup(func() {
		c := &apiextensionsv1.CustomResourceDefinition{}
		c.Name = crdName
		_ = cl.Delete(context.Background(), c)
	})

	gvr, err := ApplyOrUpdateCRD(ctx, cl, dyn, crd)
	if err != nil {
		t.Fatalf("ApplyOrUpdateCRD: %v", err)
	}
	waitCRDEstablished(t, cl, crdName)

	// Sanity: the generated CRD schema must carry default:{} on the optional parent, and NOT on the
	// guarded object (required non-defaulted child).
	got := &apiextensionsv1.CustomResourceDefinition{}
	if err := cl.Get(ctx, types.NamespacedName{Name: crdName}, got); err != nil {
		t.Fatalf("get CRD: %v", err)
	}
	spec := got.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	if img, ok := spec.Properties["image"]; !ok || img.Default == nil {
		t.Errorf("expected spec.image to carry a synthesized default:{}, got default=%v", img.Default)
	}
	if g, ok := spec.Properties["guarded"]; ok && g.Default != nil {
		t.Errorf("guard failed: spec.guarded (required non-defaulted child) must NOT get default:{}, got %v", g.Default)
	}

	// Create an instance OMITTING spec.image entirely.
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	obj.SetName("nd1")
	obj.SetNamespace("default")
	_ = unstructured.SetNestedField(obj.Object, map[string]any{}, "spec")
	created := createEventually(t, ctx, dyn.Resource(gvr).Namespace("default"), obj)

	// The apiserver must have materialized spec.image and applied the nested default.
	tag, found, _ := unstructured.NestedString(created.Object, "spec", "image", "tag")
	if !found || tag != "latest" {
		t.Fatalf("#235 NOT fixed: spec.image.tag = %q found=%v (want \"latest\") — nested default under omitted parent was dropped", tag, found)
	}
	// Top-level default must also apply.
	repl, found, _ := unstructured.NestedInt64(created.Object, "spec", "replicas")
	if !found || repl != 1 {
		t.Errorf("top-level default spec.replicas = %d found=%v, want 1", repl, found)
	}
	t.Logf("OK #235 fixed: omitted spec.image was materialized with tag=%q; spec.replicas=%d", tag, repl)
}

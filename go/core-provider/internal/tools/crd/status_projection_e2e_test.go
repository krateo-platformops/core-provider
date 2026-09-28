//go:build e2e

// Multi-version status-projection e2e. Runs only with -tags e2e against a real cluster
// (KUBECONFIG). It validates what the unit tests cannot: that a real apiserver ACCEPTS the
// status schema produced by generation.InjectStatusFields (typed scalars, a nested object,
// and an x-kubernetes-preserve-unknown-fields node) across MULTIPLE served versions, and
// that the declared status fields round-trip (survive subresource pruning) on every served
// version — including after a version bump (AppendVersion) and after the declared set
// changes (the ApplyOrUpdateCRD !statusEqual -> UpdateStatus propagation).
package crd

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	crdutils "github.com/krateo-platformops/core-provider/internal/tools/crd/generation"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

// declaredFields mirrors a CompositionDefinition's statusDataTemplate-derived schema: a
// string, an integer, a nested object path, and a free-form (preserve-unknown) node.
func declaredFields() []crdutils.StatusField {
	return []crdutils.StatusField{
		{ForPath: "endpoint", Type: "string"},
		{ForPath: "replicas", Type: "integer"},
		{ForPath: "network.host", Type: "string"},
		{ForPath: "raw", PreserveUnknownFields: true},
	}
}

// writeAndVerifyStatus creates an instance under gvk, writes the declared status fields via
// the status subresource, and asserts the apiserver accepted and retained them (i.e. the
// injected schema is valid and the fields are not pruned).
func writeAndVerifyStatus(t *testing.T, ctx context.Context, dyn dynamic.Interface, gvk schema.GroupVersionKind, gvr schema.GroupVersionResource, name string) {
	t.Helper()
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	obj.SetName(name)
	obj.SetNamespace("default")
	createEventually(t, ctx, dyn.Resource(gvr).Namespace("default"), obj)

	got, err := dyn.Resource(gvr).Namespace("default").Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get %s/%s: %v", gvk.Version, name, err)
	}
	_ = unstructured.SetNestedField(got.Object, "1.2.3.4", "status", "endpoint")
	_ = unstructured.SetNestedField(got.Object, int64(5), "status", "replicas")
	_ = unstructured.SetNestedField(got.Object, "demo.example.com", "status", "network", "host")
	_ = unstructured.SetNestedMap(got.Object, map[string]any{"anything": "goes", "n": int64(7)}, "status", "raw")

	// The apiserver validates status against the (injected) schema; a bad schema or a typed
	// field that prunes would fail here.
	updated, err := dyn.Resource(gvr).Namespace("default").UpdateStatus(ctx, got, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("UpdateStatus %s/%s (injected schema rejected?): %v", gvk.Version, name, err)
	}

	if v, _, _ := unstructured.NestedString(updated.Object, "status", "endpoint"); v != "1.2.3.4" {
		t.Errorf("%s status.endpoint = %q, want 1.2.3.4 (pruned? schema missing the field)", gvk.Version, v)
	}
	if v, _, _ := unstructured.NestedInt64(updated.Object, "status", "replicas"); v != 5 {
		t.Errorf("%s status.replicas = %d, want 5", gvk.Version, v)
	}
	if v, _, _ := unstructured.NestedString(updated.Object, "status", "network", "host"); v != "demo.example.com" {
		t.Errorf("%s status.network.host = %q, want demo.example.com", gvk.Version, v)
	}
	if m, found, _ := unstructured.NestedMap(updated.Object, "status", "raw"); !found || m["anything"] != "goes" {
		t.Errorf("%s status.raw = %v found=%v, want preserved free-form map", gvk.Version, m, found)
	}
}

// TestE2E_StatusProjectionMultiVersion injects the same declared fields into a two-version
// CRD (v1 + v2, with the vacuum storage version) and proves the apiserver accepts the
// injected schema and the declared status round-trips on BOTH served versions.
func TestE2E_StatusProjectionMultiVersion(t *testing.T) {
	if os.Getenv("KUBECONFIG") == "" {
		t.Skip("KUBECONFIG must be set")
	}
	ctx := context.Background()
	cl, dyn := e2eClients(t)

	kind := fmt.Sprintf("Sp%d", time.Now().UnixNano()%100000)
	gvkV1 := schema.GroupVersionKind{Group: compositionGroup, Version: "v1-0-0", Kind: kind}
	gvkV2 := schema.GroupVersionKind{Group: compositionGroup, Version: "v2-0-0", Kind: kind}

	crd1, err := crdutils.GenerateCRD(specSchema("foo"), gvkV1)
	if err != nil {
		t.Fatalf("GenerateCRD v1: %v", err)
	}
	crd2, err := crdutils.GenerateCRD(specSchema("bar"), gvkV2)
	if err != nil {
		t.Fatalf("GenerateCRD v2: %v", err)
	}
	// Inject the declared status fields exactly as the controller does (both versions).
	if err := crdutils.InjectStatusFields(crd1, declaredFields()); err != nil {
		t.Fatalf("inject v1: %v", err)
	}
	if err := crdutils.InjectStatusFields(crd2, declaredFields()); err != nil {
		t.Fatalf("inject v2: %v", err)
	}

	crdName := crd1.Name
	t.Cleanup(func() {
		c := &apiextensionsv1.CustomResourceDefinition{}
		c.Name = crdName
		_ = cl.Delete(ctx, c)
	})

	gvr1, err := ApplyOrUpdateCRD(ctx, cl, dyn, crd1)
	if err != nil {
		t.Fatalf("ApplyOrUpdateCRD v1 (apiserver rejected injected schema?): %v", err)
	}
	gvr2, err := ApplyOrUpdateCRD(ctx, cl, dyn, crd2)
	if err != nil {
		t.Fatalf("ApplyOrUpdateCRD v2: %v", err)
	}
	waitCRDEstablished(t, cl, crdName)

	// The declared status fields must be present on EVERY served version's schema.
	got := &apiextensionsv1.CustomResourceDefinition{}
	if err := cl.Get(ctx, types.NamespacedName{Name: crdName}, got); err != nil {
		t.Fatalf("get CRD: %v", err)
	}
	for _, v := range got.Spec.Versions {
		if v.Name == "vacuum" {
			continue
		}
		props := v.Schema.OpenAPIV3Schema.Properties["status"].Properties
		for _, f := range []string{"endpoint", "replicas", "network", "raw"} {
			if _, ok := props[f]; !ok {
				t.Errorf("served version %s status schema missing declared field %q", v.Name, f)
			}
		}
	}

	// Status round-trips on both served versions.
	writeAndVerifyStatus(t, ctx, dyn, gvkV1, gvr1, "sp-v1")
	writeAndVerifyStatus(t, ctx, dyn, gvkV2, gvr2, "sp-v2")
	t.Logf("OK: injected status schema accepted + round-trips on v1 and v2; CRD %s", crdName)
}

// TestE2E_StatusProjectionFieldsChange proves that adding a declared field to an existing
// CRD propagates through ApplyOrUpdateCRD's !statusEqual -> UpdateStatus path: the new
// field becomes writable on the live (already-served) version.
func TestE2E_StatusProjectionFieldsChange(t *testing.T) {
	if os.Getenv("KUBECONFIG") == "" {
		t.Skip("KUBECONFIG must be set")
	}
	ctx := context.Background()
	cl, dyn := e2eClients(t)

	kind := fmt.Sprintf("Spc%d", time.Now().UnixNano()%100000)
	gvk := schema.GroupVersionKind{Group: compositionGroup, Version: "v1-0-0", Kind: kind}

	mk := func(fields []crdutils.StatusField) *apiextensionsv1.CustomResourceDefinition {
		c, err := crdutils.GenerateCRD(specSchema("foo"), gvk)
		if err != nil {
			t.Fatalf("GenerateCRD: %v", err)
		}
		if err := crdutils.InjectStatusFields(c, fields); err != nil {
			t.Fatalf("inject: %v", err)
		}
		return c
	}

	crd1 := mk([]crdutils.StatusField{{ForPath: "endpoint", Type: "string"}})
	crdName := crd1.Name
	t.Cleanup(func() {
		c := &apiextensionsv1.CustomResourceDefinition{}
		c.Name = crdName
		_ = cl.Delete(ctx, c)
	})

	gvr, err := ApplyOrUpdateCRD(ctx, cl, dyn, crd1)
	if err != nil {
		t.Fatalf("ApplyOrUpdateCRD initial: %v", err)
	}
	waitCRDEstablished(t, cl, crdName)

	// Now declare an additional integer field. StatusEqual is false -> UpdateStatus
	// propagates the new schema to the live CRD's versions.
	crd2 := mk([]crdutils.StatusField{{ForPath: "endpoint", Type: "string"}, {ForPath: "count", Type: "integer"}})
	if _, err := ApplyOrUpdateCRD(ctx, cl, dyn, crd2); err != nil {
		t.Fatalf("ApplyOrUpdateCRD with added field: %v", err)
	}
	waitCRDEstablished(t, cl, crdName)

	// The new field must now be writable on the served version.
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	obj.SetName("spc1")
	obj.SetNamespace("default")
	if _, err := dyn.Resource(gvr).Namespace("default").Create(ctx, obj, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := dyn.Resource(gvr).Namespace("default").Get(ctx, "spc1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	_ = unstructured.SetNestedField(got.Object, int64(9), "status", "count")
	updated, err := dyn.Resource(gvr).Namespace("default").UpdateStatus(ctx, got, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("UpdateStatus with newly-declared field (schema not propagated?): %v", err)
	}
	if v, _, _ := unstructured.NestedInt64(updated.Object, "status", "count"); v != 9 {
		t.Errorf("status.count = %d, want 9 (added field not propagated to the live CRD)", v)
	}
	t.Logf("OK: added declared field propagated and writable; CRD %s", crdName)
}

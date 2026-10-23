//go:build e2e

// #189 apiserver-level characterization. Runs only with -tags e2e against a real cluster
// (KUBECONFIG). Reuses e2eClients/waitCRDEstablished/createEventually/compositionGroup.
//
// This does not test core-provider code. It pins the APISERVER behaviour that the served-version
// prune's safety guard rests on, because the guard is only worth its cost if the thing it
// prevents is real — and the cheapest way for that guard to rot is for someone to conclude the
// danger was hypothetical.
//
// Two facts are established:
//
//  1. status.storedVersions is a real interlock. Removing a version from spec.versions while it
//     is still listed there is REJECTED, with the apiserver naming storage migration as the
//     precondition. This is what protects "vacuum".
//
//  2. Trimming storedVersions FIRST defeats that interlock. The removal then succeeds, and every
//     read of the Kind — LIST and GET, for every instance, not only the affected one — fails with
//     "request to convert CR from an invalid group/version". That is the error on
//     docs.krateo.io's troubleshooting page.
//
// It also confirms the documented workaround ("change the chart version and change it back")
// works and why: re-appending the version to spec.versions restores readability with no data
// loss, so the blast radius is an availability outage for the Kind, not destruction of objects.
package crd

import (
	"context"
	"strings"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

func TestE2E_StoredVersionInterlockAndHowThePruneDefeatsIt(t *testing.T) {
	cl, dyn := e2eClients(t)
	ctx := context.Background()

	bp := func(v bool) *bool { return &v }
	root := func(props map[string]apiextensionsv1.JSONSchemaProps, preserve bool) *apiextensionsv1.CustomResourceValidation {
		spec := apiextensionsv1.JSONSchemaProps{Type: "object", Properties: props}
		if preserve {
			spec = apiextensionsv1.JSONSchemaProps{Type: "object", XPreserveUnknownFields: bp(true)}
		}
		return &apiextensionsv1.CustomResourceValidation{
			OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
				Type: "object",
				Properties: map[string]apiextensionsv1.JSONSchemaProps{
					"apiVersion": {Type: "string"},
					"kind":       {Type: "string"},
					"metadata":   {Type: "object"},
					"spec":       spec,
					"status":     {Type: "object", XPreserveUnknownFields: bp(true)},
				},
			},
		}
	}
	v1Schema := root(map[string]apiextensionsv1.JSONSchemaProps{"replicas": {Type: "integer"}}, false)

	name := "interlocks." + compositionGroup
	crd := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: compositionGroup,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural: "interlocks", Singular: "interlock", Kind: "Interlock", ListKind: "InterlockList",
			},
			Scope:      apiextensionsv1.NamespaceScoped,
			Conversion: &apiextensionsv1.CustomResourceConversion{Strategy: apiextensionsv1.NoneConverter},
			// As generateCRD emits the FIRST version: sole version, storage=true, no vacuum yet.
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
				{Name: "v1-0-0", Served: true, Storage: true, Schema: v1Schema},
			},
		},
	}
	if err := cl.Create(ctx, crd); err != nil {
		t.Fatalf("create CRD: %v", err)
	}
	t.Cleanup(func() { _ = cl.Delete(ctx, crd) })
	waitCRDEstablished(t, cl, name)

	v1 := schema.GroupVersionResource{Group: compositionGroup, Version: "v1-0-0", Resource: "interlocks"}

	// An instance written while v1-0-0 is the STORAGE version, and deliberately carrying no
	// composition-version label — the policy-absence gap that makes it invisible to both the
	// migration and the prune's eligibility test.
	createEventually(t, ctx, dyn.Resource(v1).Namespace("default"), &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": compositionGroup + "/v1-0-0",
			"kind":       "Interlock",
			"metadata":   map[string]any{"name": "i1", "namespace": "default"},
			"spec":       map[string]any{"replicas": int64(2)},
		},
	})

	live := &apiextensionsv1.CustomResourceDefinition{}
	if err := cl.Get(ctx, types.NamespacedName{Name: name}, live); err != nil {
		t.Fatalf("get CRD: %v", err)
	}
	if !contains(live.Status.StoredVersions, "v1-0-0") {
		t.Fatalf("precondition failed: v1-0-0 is not in storedVersions (%v) — nothing is encoded "+
			"at it, so this test proves nothing", live.Status.StoredVersions)
	}

	// The version bump, as AppendVersion performs it: add v2-0-0 and the vacuum storage version.
	live.Spec.Versions = []apiextensionsv1.CustomResourceDefinitionVersion{
		{Name: "v1-0-0", Served: true, Storage: false, Schema: v1Schema},
		{Name: "v2-0-0", Served: true, Storage: false, Schema: v1Schema},
		{Name: "vacuum", Served: false, Storage: true, Schema: root(nil, true)},
	}
	if err := cl.Update(ctx, live); err != nil {
		t.Fatalf("append version: %v", err)
	}
	// The instance was never rewritten (no label -> the migration selects nothing), so it is
	// still encoded at v1-0-0 while vacuum is now the storage version.

	// --- Fact 1: the interlock refuses the removal on its own.
	if err := cl.Get(ctx, types.NamespacedName{Name: name}, live); err != nil {
		t.Fatalf("get CRD: %v", err)
	}
	withoutV1 := []apiextensionsv1.CustomResourceDefinitionVersion{}
	for _, v := range live.Spec.Versions {
		if v.Name != "v1-0-0" {
			withoutV1 = append(withoutV1, v)
		}
	}
	attempt := live.DeepCopy()
	attempt.Spec.Versions = withoutV1
	err := cl.Update(ctx, attempt)
	if err == nil {
		t.Fatalf("the apiserver ACCEPTED removing v1-0-0 while it was still in storedVersions "+
			"(%v) — the interlock this guard relies on does not exist on this cluster, and the "+
			"#189 reasoning needs revisiting rather than the test being deleted",
			live.Status.StoredVersions)
	}
	if !strings.Contains(err.Error(), "storedVersions") {
		t.Errorf("rejection did not mention storedVersions, so it may be unrelated: %v", err)
	}
	t.Logf("interlock holds: %v", err)

	// --- Fact 2: trimming storedVersions first defeats it. This is exactly what
	// pruneStaleServedVersions does before calling RemoveStaleVersions.
	if err := cl.Get(ctx, types.NamespacedName{Name: name}, live); err != nil {
		t.Fatalf("get CRD: %v", err)
	}
	var kept []string
	for _, sv := range live.Status.StoredVersions {
		if sv != "v1-0-0" {
			kept = append(kept, sv)
		}
	}
	live.Status.StoredVersions = kept
	if err := cl.Status().Update(ctx, live); err != nil {
		t.Fatalf("trim storedVersions: %v", err)
	}
	if err := cl.Get(ctx, types.NamespacedName{Name: name}, live); err != nil {
		t.Fatalf("get CRD: %v", err)
	}
	live.Spec.Versions = withoutV1
	if err := cl.Update(ctx, live); err != nil {
		t.Fatalf("prune v1-0-0 after trimming storedVersions: %v", err)
	}

	v2 := schema.GroupVersionResource{Group: compositionGroup, Version: "v2-0-0", Resource: "interlocks"}
	var listErr, getErr error
	_ = wait(10*time.Second, func() bool {
		_, listErr = dyn.Resource(v2).Namespace("default").List(ctx, metav1.ListOptions{})
		_, getErr = dyn.Resource(v2).Namespace("default").Get(ctx, "i1", metav1.GetOptions{})
		return listErr != nil || getErr != nil
	})

	if listErr == nil && getErr == nil {
		t.Fatalf("pruning a version with objects still encoded at it did NOT break reads — the " +
			"premise of #189 does not hold on this cluster; investigate before removing the guard")
	}
	t.Logf("Kind is now unreadable, as #189 describes:\n  LIST: %v\n  GET : %v", listErr, getErr)
	for _, e := range []error{listErr, getErr} {
		if e != nil && !strings.Contains(e.Error(), "invalid group/version") {
			t.Errorf("expected the documented \"invalid group/version\" failure, got: %v", e)
		}
	}

	// --- The documented workaround, and the bound on the damage: re-appending the version
	// restores readability with no data loss.
	if err := cl.Get(ctx, types.NamespacedName{Name: name}, live); err != nil {
		t.Fatalf("get CRD: %v", err)
	}
	live.Spec.Versions = append([]apiextensionsv1.CustomResourceDefinitionVersion{
		{Name: "v1-0-0", Served: true, Storage: false, Schema: v1Schema},
	}, live.Spec.Versions...)
	if err := cl.Update(ctx, live); err != nil {
		t.Fatalf("re-add v1-0-0: %v", err)
	}

	var recovered *unstructured.Unstructured
	ok := wait(30*time.Second, func() bool {
		var e error
		recovered, e = dyn.Resource(v2).Namespace("default").Get(ctx, "i1", metav1.GetOptions{})
		return e == nil
	})
	if !ok {
		t.Fatal("re-adding the pruned version did NOT restore readability — the outage is not " +
			"self-clearing, which makes #189 considerably worse than filed")
	}
	if replicas, found, _ := unstructured.NestedInt64(recovered.Object, "spec", "replicas"); !found || replicas != 2 {
		t.Errorf("object survived but lost data: spec.replicas=%d found=%v, want 2", replicas, found)
	}
	t.Log("recovery confirmed: re-appending the version restores the Kind with no data loss — " +
		"which is why the documented workaround (change the chart version and change it back) clears it")
}

func contains(h []string, n string) bool {
	for _, s := range h {
		if s == n {
			return true
		}
	}
	return false
}

func wait(d time.Duration, f func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if f() {
			return true
		}
		time.Sleep(300 * time.Millisecond)
	}
	return false
}

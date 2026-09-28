//go:build e2e

// Package crd e2e validation. Runs only with -tags e2e against a real cluster (KUBECONFIG
// env). The composition-version-policy test additionally requires the GA
// MutatingAdmissionPolicy API (admissionregistration.k8s.io/v1) - Kubernetes >= 1.36.
//
// It validates two things the unit tests cannot:
//   - None conversion + the permissive "vacuum" storage version round-trip losslessly
//     across heterogeneous per-version schemas, using the REAL generator + ApplyOrUpdateCRD;
//   - the shipped composition-version MutatingAdmissionPolicy stamps the request's served
//     version onto a composition instance, in-apiserver, with no webhook.
package crd

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	crdutils "github.com/krateo-platformops/core-provider/internal/tools/crd/generation"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const compositionGroup = "composition.krateo.io"

func e2eClients(t *testing.T) (client.Client, dynamic.Interface) {
	t.Helper()
	rc, err := clientcmd.BuildConfigFromFlags("", os.Getenv("KUBECONFIG"))
	if err != nil {
		t.Fatalf("rest config: %v", err)
	}
	sc := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(sc)
	_ = apiextensionsv1.AddToScheme(sc)
	cl, err := client.New(rc, client.Options{Scheme: sc})
	if err != nil {
		t.Fatalf("kube client: %v", err)
	}
	dyn, err := dynamic.NewForConfig(rc)
	if err != nil {
		t.Fatalf("dynamic client: %v", err)
	}
	return cl, dyn
}

// specSchema returns the values schema GenerateCRD expects (it describes the spec fields
// directly, like a chart's values.schema.json), with one string field.
func specSchema(field string) []byte {
	return []byte(fmt.Sprintf(`{"type":"object","properties":{%q:{"type":"string"}}}`, field))
}

func waitCRDEstablished(t *testing.T, cl client.Client, name string) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := cl.Get(context.Background(), types.NamespacedName{Name: name}, crd); err == nil {
			for _, c := range crd.Status.Conditions {
				if c.Type == apiextensionsv1.Established && c.Status == apiextensionsv1.ConditionTrue {
					return
				}
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("CRD %s not established", name)
}

// createEventually creates an object, tolerating the window between a CRD reporting Established
// and its resource actually being servable.
//
// Established=True means the apiserver accepted the definition, NOT that every discovery path has
// caught up — so a create issued immediately after can fail with "Resource kind ... not found.
// There can be a delay between when CustomResourceDefinitions are created and when they are
// available". It is a race, and the loser is whoever is running on a slower machine: this suite
// passed on a local kind and failed on a CI runner the first time it was ever run there
// (krateo-platformops/core-provider#138).
//
// Retrying the create is the honest fix. Sleeping longer only moves the threshold, and a fixed
// sleep that is long enough for CI is wasted on every other run.
func createEventually(t *testing.T, ctx context.Context, ri dynamic.ResourceInterface, obj *unstructured.Unstructured) *unstructured.Unstructured {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		created, err := ri.Create(ctx, obj, metav1.CreateOptions{})
		if err == nil {
			return created
		}
		// Only discovery lag is retried. A schema rejection, a policy denial or anything else is a
		// real result and must fail the test immediately rather than after a minute of retries.
		if !meta.IsNoMatchError(err) && !apierrors.IsNotFound(err) {
			t.Fatalf("create object: %v", err)
		}
		lastErr = err
		time.Sleep(time.Second)
	}
	t.Fatalf("create object: resource never became servable within 60s: %v", lastErr)
	return nil
}

// TestE2E_NoneVacuumRoundTrip drives the real generator + ApplyOrUpdateCRD to build a
// two-version CRD (v1 has spec.foo, v2 has spec.bar) with a vacuum storage version and
// None conversion, then proves a v1 object round-trips losslessly through storage.
func TestE2E_NoneVacuumRoundTrip(t *testing.T) {
	if os.Getenv("KUBECONFIG") == "" {
		t.Skip("KUBECONFIG must be set")
	}
	ctx := context.Background()
	cl, dyn := e2eClients(t)

	kind := fmt.Sprintf("Rt%d", time.Now().UnixNano()%100000)
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
	crdName := crd1.Name
	t.Cleanup(func() {
		c := &apiextensionsv1.CustomResourceDefinition{}
		c.Name = crdName
		_ = cl.Delete(ctx, c)
	})

	gvr1, err := ApplyOrUpdateCRD(ctx, cl, dyn, crd1)
	if err != nil {
		t.Fatalf("ApplyOrUpdateCRD v1: %v", err)
	}
	gvr2, err := ApplyOrUpdateCRD(ctx, cl, dyn, crd2)
	if err != nil {
		t.Fatalf("ApplyOrUpdateCRD v2: %v", err)
	}
	waitCRDEstablished(t, cl, crdName)

	// The applied CRD must use None conversion and carry the vacuum storage version.
	got := &apiextensionsv1.CustomResourceDefinition{}
	if err := cl.Get(ctx, types.NamespacedName{Name: crdName}, got); err != nil {
		t.Fatalf("get CRD: %v", err)
	}
	if got.Spec.Conversion == nil || got.Spec.Conversion.Strategy != apiextensionsv1.NoneConverter {
		t.Fatalf("expected None conversion, got %+v", got.Spec.Conversion)
	}
	var hasVacuum, vacuumIsStorage bool
	for _, v := range got.Spec.Versions {
		if v.Name == "vacuum" {
			hasVacuum, vacuumIsStorage = true, v.Storage
		}
	}
	if !hasVacuum || !vacuumIsStorage {
		t.Fatalf("expected a vacuum storage version, versions=%+v", got.Spec.Versions)
	}

	// Create an instance through v1 with spec.foo, then read it back through both versions.
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvkV1)
	obj.SetName("rt1")
	obj.SetNamespace("default")
	_ = unstructured.SetNestedField(obj.Object, "hello", "spec", "foo")
	createEventually(t, ctx, dyn.Resource(gvr1).Namespace("default"), obj)

	readFoo := func(gvr schema.GroupVersionResource) (string, bool) {
		o, err := dyn.Resource(gvr).Namespace("default").Get(ctx, "rt1", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get via %s: %v", gvr.Version, err)
		}
		v, found, _ := unstructured.NestedString(o.Object, "spec", "foo")
		return v, found
	}

	if v, found := readFoo(gvr1); !found || v != "hello" {
		t.Fatalf("read as v1 after create: foo=%q found=%v, want hello", v, found)
	}
	if _, found := readFoo(gvr2); found {
		t.Fatalf("read as v2: foo should be pruned from the v2 view")
	}
	if v, found := readFoo(gvr1); !found || v != "hello" {
		t.Fatalf("read as v1 again: foo=%q found=%v - vacuum did not preserve it", v, found)
	}
	t.Logf("OK: None+vacuum round-trip lossless (v1 foo survived a v2 read); CRD %s", crdName)
}

// TestE2E_CompositionVersionPolicy applies the shipped composition-version
// MutatingAdmissionPolicy (GA v1) and proves it stamps the request's served version onto
// a composition instance, in-apiserver, with no webhook. Requires Kubernetes >= 1.36.
func TestE2E_CompositionVersionPolicy(t *testing.T) {
	if os.Getenv("KUBECONFIG") == "" {
		t.Skip("KUBECONFIG must be set")
	}
	ctx := context.Background()
	cl, dyn := e2eClients(t)

	if err := ensurePolicy(ctx, cl); err != nil {
		t.Skipf("MutatingAdmissionPolicy not available (needs k8s >= 1.36): %v", err)
	}
	t.Cleanup(func() { deletePolicy(ctx, cl) })

	kind := fmt.Sprintf("Pol%d", time.Now().UnixNano()%100000)
	gvk := schema.GroupVersionKind{Group: compositionGroup, Version: "v1-0-0", Kind: kind}
	crd, err := crdutils.GenerateCRD(specSchema("foo"), gvk)
	if err != nil {
		t.Fatalf("GenerateCRD: %v", err)
	}
	gvr, err := ApplyOrUpdateCRD(ctx, cl, dyn, crd)
	if err != nil {
		t.Fatalf("ApplyOrUpdateCRD: %v", err)
	}
	t.Cleanup(func() {
		c := &apiextensionsv1.CustomResourceDefinition{}
		c.Name = crd.Name
		_ = cl.Delete(ctx, c)
	})
	waitCRDEstablished(t, cl, crd.Name)
	time.Sleep(2 * time.Second) // let the policy become active

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	obj.SetName("pol1")
	obj.SetNamespace("default")
	_ = unstructured.SetNestedField(obj.Object, "hello", "spec", "foo")
	created := createEventually(t, ctx, dyn.Resource(gvr).Namespace("default"), obj)
	if got := created.GetLabels()["krateo.io/composition-version"]; got != "v1-0-0" {
		t.Fatalf("composition-version label = %q, want v1-0-0 (policy did not stamp it)", got)
	}
	t.Logf("OK: MutatingAdmissionPolicy stamped composition-version=v1-0-0 in-apiserver (no webhook)")
}

func policyObjects() (*unstructured.Unstructured, *unstructured.Unstructured) {
	p := &unstructured.Unstructured{}
	p.SetAPIVersion("admissionregistration.k8s.io/v1")
	p.SetKind("MutatingAdmissionPolicy")
	p.SetName("e2e-compositions-version-policy")
	p.Object["spec"] = map[string]any{
		"matchConstraints": map[string]any{
			"matchPolicy": "Exact",
			"resourceRules": []any{map[string]any{
				"apiGroups": []any{compositionGroup}, "apiVersions": []any{"*"},
				"operations": []any{"CREATE", "UPDATE"}, "resources": []any{"*"},
			}},
		},
		"failurePolicy": "Fail", "reinvocationPolicy": "Never",
		"mutations": []any{map[string]any{
			"patchType": "ApplyConfiguration",
			"applyConfiguration": map[string]any{
				"expression": `Object{ metadata: Object.metadata{ labels: {"krateo.io/composition-version": request.requestKind.version} } }`,
			},
		}},
	}
	b := &unstructured.Unstructured{}
	b.SetAPIVersion("admissionregistration.k8s.io/v1")
	b.SetKind("MutatingAdmissionPolicyBinding")
	b.SetName("e2e-compositions-version-binding")
	b.Object["spec"] = map[string]any{"policyName": "e2e-compositions-version-policy"}
	return p, b
}

func ensurePolicy(ctx context.Context, cl client.Client) error {
	p, b := policyObjects()
	for _, o := range []*unstructured.Unstructured{p, b} {
		if err := cl.Create(ctx, o); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	}
	return nil
}

func deletePolicy(ctx context.Context, cl client.Client) {
	p, b := policyObjects()
	_ = cl.Delete(ctx, b)
	_ = cl.Delete(ctx, p)
}

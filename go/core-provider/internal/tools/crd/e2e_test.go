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
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	crdutils "github.com/krateo-platformops/core-provider/internal/tools/crd/generation"
	"github.com/krateo-platformops/core-provider/internal/tools/policy"
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

// isDiscoveryLag reports whether an error is the CRD-not-yet-servable window rather than a real
// rejection. Deliberately narrow: it must never swallow a policy denial or a schema error, because
// those are what several of these tests exist to observe.
func isDiscoveryLag(err error) bool {
	var nkme *meta.NoKindMatchError
	if errors.As(err, &nkme) || meta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
		return true
	}
	// The RESTMapper's wrapped form, which the typed checks above miss.
	return strings.Contains(err.Error(), "There can be a delay between when CustomResourceDefinitions are created")
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
		//
		// The predicate has to be generous about HOW the lag is reported. meta.IsNoMatchError uses a
		// bare type assertion with no unwrapping, so a wrapped NoKindMatchError slips past it — which
		// is exactly what happened the first time this helper was used: it bailed instantly on the
		// very error it was written to absorb. Match the RESTMapper's own sentence as a fallback.
		if !isDiscoveryLag(err) {
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

	// Assert the apiserver accepted the JSON Patch form specifically. Without this the test
	// passes against either mutation shape, which is how the pre-#66 expression survived here
	// unnoticed — a green run that could not tell the two apart.
	live := &unstructured.Unstructured{}
	live.SetAPIVersion("admissionregistration.k8s.io/v1")
	live.SetKind("MutatingAdmissionPolicy")
	if err := cl.Get(ctx, client.ObjectKey{Name: policy.PolicyName}, live); err != nil {
		t.Fatalf("reading back the policy we just applied: %v", err)
	}
	liveMuts, _, _ := unstructured.NestedSlice(live.Object, "spec", "mutations")
	if len(liveMuts) != 1 {
		t.Fatalf("expected exactly 1 mutation on the live policy, got %d", len(liveMuts))
	}
	if pt, _, _ := unstructured.NestedString(liveMuts[0].(map[string]any), "patchType"); pt != "JSONPatch" {
		t.Fatalf("live policy patchType = %q, want JSONPatch — the apiserver is running the "+
			"pre-#66 ApplyConfiguration mutation (core-provider#66, #173)", pt)
	}

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

// ensurePolicy installs the policy core-provider ACTUALLY SHIPS, by calling the production
// entry point rather than rebuilding one here.
//
// It used to hand-write its own copy — and that copy still carried the pre-#66
// ApplyConfiguration mutation long after the shipped policy had moved to JSON Patch. So the one
// job this e2e exists to do, proving the shipped policy works against a real apiserver, was not
// being done: the test would have passed just the same with policy.objects() returning anything
// at all. That is the same defect as core-provider#173 (a fix that reached the Go code and
// neither chart) in a third location, and it is why this now calls into the package under test.
func ensurePolicy(ctx context.Context, cl client.Client) error {
	return policy.EnsureCompositionVersionPolicy(ctx, cl)
}

func deletePolicy(ctx context.Context, cl client.Client) {
	for _, kind := range []struct{ kind, name string }{
		{"MutatingAdmissionPolicyBinding", policy.BindingName},
		{"MutatingAdmissionPolicy", policy.PolicyName},
	} {
		o := &unstructured.Unstructured{}
		o.SetAPIVersion("admissionregistration.k8s.io/v1")
		o.SetKind(kind.kind)
		o.SetName(kind.name)
		_ = cl.Delete(ctx, o)
	}
}

// conversionFailure matches the admission error family #66 is about. ApplyConfiguration performs
// a structured merge, which requires converting to a TYPED object first — and with
// failurePolicy: Fail, any reason that conversion cannot be completed denies the write, for an
// object the mutation barely touches.
//
// Matching the family rather than one message is deliberate, and the two variants observed so far
// are why. On krateo-057 it was a value/schema mismatch:
//
//	failed to convert original object to typed object: errors:
//	  .spec.resources.requests.cpu: expected numeric (int or float), got string
//
// In this test it is the apiserver having no registered type for a CRD created moments earlier:
//
//	failed to convert patch object to typed object:
//	  no corresponding type for composition.krateo.io/v1-0-0, Kind=Qty...
//
// Different triggers, same defect: the mutation cannot be applied without typing the object, so
// the object's typeability becomes a precondition for stamping a label on it. Pinning the exact
// 057 wording made this test fail against a correct control arm, which is the wrong failure.
const conversionFailure = "failed to convert"

// TestE2E_PolicyDoesNotDenyQuantityStrings is the test that decides whether the JSON Patch form
// actually AVOIDS the #66 conversion or merely relocates it. Everything else about #66 is
// shape-checking — this submits the offending object to a real apiserver under each policy in
// turn and compares the two outcomes.
//
// It is written as a differential on purpose. "The shipped policy admits it" alone would be
// vacuous: if the apiserver never performed the conversion in the first place, a passing run
// would prove nothing about the fix. So the legacy arm must FAIL in the specific #66 way, and if
// it does not, this test says so loudly rather than quietly reporting success — the premise of
// the fix would then be the thing in question.
//
// Both arms expect the create to be REJECTED; what differs is why. Mutating admission runs before
// schema validation, so with a numeric-typed field holding "200m":
//
//	legacy  -> denied by the mutation's typed conversion (the #66 bug)
//	shipped -> mutation succeeds, then ordinary CRD schema validation rejects the value
//
// The second is the correct behaviour: a bad value is rejected for being a bad value, not for
// making an unrelated label stamp impossible.
func TestE2E_PolicyDoesNotDenyQuantityStrings(t *testing.T) {
	if os.Getenv("KUBECONFIG") == "" {
		t.Skip("KUBECONFIG must be set")
	}
	ctx := context.Background()
	cl, dyn := e2eClients(t)

	// A CRD whose schema types the field numerically, as a chart's values.schema.json did when
	// it declared `cpu: {type: number}`, while the real value is a Quantity string.
	kind := fmt.Sprintf("Qty%d", time.Now().UnixNano()%100000)
	gvk := schema.GroupVersionKind{Group: compositionGroup, Version: "v1-0-0", Kind: kind}
	numeric := []byte(`{"type":"object","properties":{"cpu":{"type":"number"}}}`)
	crd, err := crdutils.GenerateCRD(numeric, gvk)
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

	offending := func(name string) *unstructured.Unstructured {
		o := &unstructured.Unstructured{}
		o.SetGroupVersionKind(gvk)
		o.SetName(name)
		o.SetNamespace("default")
		// A Quantity string in the numeric-typed field. Kubernetes considers "200m" a perfectly
		// valid CPU quantity; the chart's schema is what calls it a number.
		_ = unstructured.SetNestedField(o.Object, "200m", "spec", "cpu")
		return o
	}

	// --- control arm: the pre-#66 policy must reproduce the conversion denial -----------------
	lp, lb := legacyPolicyObjects()
	for _, o := range []*unstructured.Unstructured{lp, lb} {
		if err := cl.Create(ctx, o); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Skipf("MutatingAdmissionPolicy not available (needs k8s >= 1.36): %v", err)
		}
	}
	time.Sleep(3 * time.Second) // let the policy become active

	_, legacyErr := dyn.Resource(gvr).Namespace("default").
		Create(ctx, offending("qty-legacy"), metav1.CreateOptions{})
	_ = cl.Delete(ctx, lb)
	_ = cl.Delete(ctx, lp)

	if legacyErr == nil || !strings.Contains(legacyErr.Error(), conversionFailure) {
		t.Fatalf("the #66 premise did not reproduce: under the pre-fix ApplyConfiguration policy "+
			"this write should be denied by a typed-conversion failure (%q), got: %v\n"+
			"Until this arm fails in the expected way, the other arm proves nothing about the fix.",
			conversionFailure, legacyErr)
	}
	t.Logf("control: legacy ApplyConfiguration denied the write with a typed-conversion failure: %v", legacyErr)

	// --- the shipped policy must NOT deny it for that reason ----------------------------------
	if err := ensurePolicy(ctx, cl); err != nil {
		t.Fatalf("installing the shipped policy: %v", err)
	}
	t.Cleanup(func() { deletePolicy(ctx, cl) })
	time.Sleep(3 * time.Second)

	_, shippedErr := dyn.Resource(gvr).Namespace("default").
		Create(ctx, offending("qty-shipped"), metav1.CreateOptions{})

	if shippedErr != nil && strings.Contains(shippedErr.Error(), conversionFailure) {
		t.Fatalf("the JSON Patch form RELOCATED the #66 conversion rather than avoiding it: %v", shippedErr)
	}
	t.Logf("shipped: no conversion failure (error, if any, is ordinary validation): %v", shippedErr)
}

// legacyPolicyObjects rebuilds the pre-#66 ApplyConfiguration policy under a DISTINCT name, used
// only as the control arm of TestE2E_PolicyDoesNotDenyQuantityStrings. This is the one place a
// hand-written copy is correct: it is the thing we are proving we moved away from.
func legacyPolicyObjects() (*unstructured.Unstructured, *unstructured.Unstructured) {
	const name = "e2e-legacy-composition-version"
	p := &unstructured.Unstructured{}
	p.SetAPIVersion("admissionregistration.k8s.io/v1")
	p.SetKind("MutatingAdmissionPolicy")
	p.SetName(name)
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
	b.SetName(name)
	b.Object["spec"] = map[string]any{"policyName": name}
	return p, b
}

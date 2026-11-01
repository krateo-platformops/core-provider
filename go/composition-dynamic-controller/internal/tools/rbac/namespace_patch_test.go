package rbac

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

var nsGVR = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}

// liveNamespaceWithForeignLabels is a tenant namespace as Kyverno generates one: our label is
// absent, and the generate.kyverno.io/* set plus managed-by belong to somebody else.
func liveNamespaceWithForeignLabels() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata": map[string]any{
			"name": "tenant-acmpe2e",
			"labels": map[string]any{
				"app.kubernetes.io/managed-by":          "kyverno",
				"generate.kyverno.io/policy-name":       "tenant-namespace-on-login",
				"generate.kyverno.io/rule-name":         "create-ns",
				"generate.kyverno.io/trigger-group":     "",
				"generate.kyverno.io/trigger-version":   "v1",
				"generate.kyverno.io/trigger-kind":      "Secret",
				"generate.kyverno.io/trigger-namespace": "krateo-system",
				"generate.kyverno.io/trigger-name":      "acmpe2e-login",
			},
		},
	}}
}

// capturePatch returns a fake dynamic client holding the live namespace, and a pointer to the raw
// body of the first patch it receives.
func capturePatch(t *testing.T, live *unstructured.Unstructured) (*dynamicfake.FakeDynamicClient, *[]byte) {
	t.Helper()
	scheme := runtime.NewScheme()
	cli := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{nsGVR: "NamespaceList"}, live)

	var body []byte
	cli.PrependReactor("patch", "namespaces", func(a k8stesting.Action) (bool, runtime.Object, error) {
		pa, ok := a.(k8stesting.PatchAction)
		if !ok {
			return false, nil, nil
		}
		body = append([]byte(nil), pa.GetPatch()...)
		return false, nil, nil // fall through so the fake still applies it
	})
	return cli, &body
}

// THE CASE THE FIX EXISTS FOR (#294). The request must not carry labels we do not own.
//
// The merge in ApplyNamespace already preserved foreign labels, so nothing was being STRIPPED. But a
// full PUT carries every field, so our ServiceAccount was submitting an object bearing Kyverno's
// generate labels — and Kyverno 1.19.2's protect-generation-labels webhook (failurePolicy Fail,
// CREATE/UPDATE on */*) denies that outright: "Kyverno generate labels can only be set by Kyverno".
// A PUT cannot express "I am preserving this", so no amount of merging helps; the field simply must
// not be in the request.
//
// Consequence measured on a live cluster: every tenant namespace generated after that Kyverno
// upgrade rejected the RBAC install, so no newly onboarded tenant could get a child at all — 180
// identical attempts in 30 minutes on the first one.
func TestApplyNamespace_PatchOmitsForeignLabels(t *testing.T) {
	live := liveNamespaceWithForeignLabels()
	cli, body := capturePatch(t, live)
	inst := &RBACInstaller{DynamicClient: cli}

	desired := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   "tenant-acmpe2e",
		Labels: map[string]string{"krateo.io/composition-id": "abc123"},
	}}

	if _, err := inst.ApplyNamespace(context.Background(), desired); err != nil {
		t.Fatalf("ApplyNamespace: %v", err)
	}

	// CALIBRATION. If no patch was issued this test proves nothing — it would pass against code
	// that silently did nothing at all.
	if len(*body) == 0 {
		t.Fatal("no patch was captured, so this test could not distinguish the fix from no write")
	}

	var got map[string]any
	if err := json.Unmarshal(*body, &got); err != nil {
		t.Fatalf("patch body is not JSON: %v\n%s", err, *body)
	}

	raw := string(*body)
	for _, foreign := range []string{"generate.kyverno.io", "managed-by"} {
		if strings.Contains(raw, foreign) {
			t.Errorf("the patch carries the foreign key %q, which is what the webhook denies:\n%s",
				foreign, raw)
		}
	}
	if !strings.Contains(raw, "krateo.io/composition-id") {
		t.Errorf("the patch does not carry OUR label, so it would not do its job:\n%s", raw)
	}
}

// THE DIFFERENTIAL, and it is what makes the test above mean something rather than assert a string
// absence. A full PUT of the merged object — the pre-fix behaviour — DOES carry the foreign labels.
// Without this arm, "the request omits them" could be true of a request that omits everything.
func TestApplyNamespace_AFullPutWouldHaveCarriedThem(t *testing.T) {
	live := liveNamespaceWithForeignLabels()

	// Reproduce exactly what the pre-fix code sent: merge our labels into the live object, convert
	// the whole thing, and serialise it as a PUT body would.
	existing := &corev1.Namespace{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(live.Object, existing); err != nil {
		t.Fatalf("convert: %v", err)
	}
	existing.Labels["krateo.io/composition-id"] = "abc123"
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(existing)
	if err != nil {
		t.Fatalf("convert back: %v", err)
	}
	putBody, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if !strings.Contains(string(putBody), "generate.kyverno.io") {
		t.Fatal("a full PUT of the merged object did NOT carry the Kyverno labels — then the PUT " +
			"was never the problem and the fix is addressing the wrong thing")
	}
	if !strings.Contains(string(putBody), "krateo.io/composition-id") {
		t.Error("the reproduction did not even carry our own label; it is not reproducing the old path")
	}
}

// A namespace that already has our labels must produce NO write at all — the early return. Without
// this, the fix could patch on every reconcile and the only symptom would be write load.
func TestApplyNamespace_NoPatchWhenNothingIsMissing(t *testing.T) {
	live := liveNamespaceWithForeignLabels()
	lbl, _, _ := unstructured.NestedMap(live.Object, "metadata", "labels")
	lbl["krateo.io/composition-id"] = "abc123"
	_ = unstructured.SetNestedMap(live.Object, lbl, "metadata", "labels")

	cli, body := capturePatch(t, live)
	inst := &RBACInstaller{DynamicClient: cli}

	desired := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   "tenant-acmpe2e",
		Labels: map[string]string{"krateo.io/composition-id": "abc123"},
	}}
	if _, err := inst.ApplyNamespace(context.Background(), desired); err != nil {
		t.Fatalf("ApplyNamespace: %v", err)
	}
	if len(*body) != 0 {
		t.Errorf("nothing was missing, so nothing should have been written; got patch:\n%s", *body)
	}
}

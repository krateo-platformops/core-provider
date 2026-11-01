package rbac

// #294, second attempt.
//
// The request-shape tests that used to live here are GONE on purpose. They asserted that the patch
// body omits foreign labels, which defended a rationale that turned out to be FALSE — admission
// webhooks receive the RESULTING object, not the patch body, so the request shape was never the
// variable. A test defending a refuted theory is worse than no test, because it makes the wrong
// mechanism look verified.
//
// The patch is kept because a narrower write is better practice, not because it fixes anything.

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

var ownerNsGVR = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}

// kyvernoGeneratedNamespace is a tenant namespace as Kyverno generates one: `managed-by: kyverno`
// plus the generate.kyverno.io/* set, and none of our markers.
func kyvernoGeneratedNamespace() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata": map[string]any{
			"name": "tenant-acmpe2e",
			"labels": map[string]any{
				"app.kubernetes.io/managed-by":          "kyverno",
				"generate.kyverno.io/policy-name":       "tenant-namespace-on-login",
				"generate.kyverno.io/rule-name":         "create-ns",
				"generate.kyverno.io/trigger-kind":      "Secret",
				"generate.kyverno.io/trigger-namespace": "krateo-system",
			},
		},
	}}
}

func namespaceOwnedBy(owner string) *unstructured.Unstructured {
	lbl := map[string]any{}
	if owner != "" {
		lbl["app.kubernetes.io/managed-by"] = owner
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Namespace",
		"metadata": map[string]any{"name": "tenant-acmpe2e", "labels": lbl},
	}}
}

// captureWrites returns a fake client holding `live`, plus a pointer to a record of any write it
// receives. An empty record means NO WRITE HAPPENED, which is what this fix is about — and it
// catches a regression to a full PUT as well, so "no write" means no write of any shape.
func captureWrites(t *testing.T, objs ...runtime.Object) (*dynamicfake.FakeDynamicClient, *[]byte) {
	t.Helper()
	cli := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{ownerNsGVR: "NamespaceList"}, objs...)

	var writes []byte
	cli.PrependReactor("patch", "namespaces", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if pa, ok := a.(k8stesting.PatchAction); ok {
			writes = append(writes, pa.GetPatch()...)
		}
		return false, nil, nil
	})
	cli.PrependReactor("update", "namespaces", func(a k8stesting.Action) (bool, runtime.Object, error) {
		writes = append(writes, []byte("UNEXPECTED-FULL-UPDATE")...)
		return false, nil, nil
	})
	return cli, &writes
}

// THE CASE THIS FIX EXISTS FOR, after the first attempt shipped and failed on a live cluster.
//
// CreateNamespace stamps `app.kubernetes.io/managed-by: Helm` so HELM WILL ADOPT a namespace we are
// about to have it manage. On a namespace somebody else already owns that is a claim, not a merge:
// the loop writes our value wherever it differs, so it changed `kyverno` -> `Helm`.
//
// Kyverno 1.19.2's protect-generation-labels webhook denies that, and denies it WHATEVER THE REQUEST
// SHAPE, because admission sees the resulting object. 136 identical failures in 22 minutes on
// 2.13.53 — the version carrying the first fix.
//
// So the assertion is NO WRITE AT ALL, not a patch with particular contents.
func TestApplyNamespace_LeavesAForeignManagerAlone(t *testing.T) {
	cli, writes := captureWrites(t, kyvernoGeneratedNamespace())
	inst := &RBACInstaller{DynamicClient: cli}

	// THE PRODUCTION CONSTRUCTOR, not a fixture. The first version of this test hand-built the
	// desired namespace with one harmless label, so it asserted a property of the fixture — the
	// real CreateNamespace carries `managed-by: Helm`, which IS the defect. A test that constructs
	// its own input cannot catch a defect that lives in the real input.
	got, err := inst.ApplyNamespace(context.Background(),
		CreateNamespace("tenant-acmpe2e", "rel", "krateo-system"))
	if err != nil {
		t.Fatalf("a foreign-owned namespace must be skipped, not error: %v", err)
	}
	if len(*writes) != 0 {
		t.Errorf("a foreign-owned namespace must not be written at all; got: %s", *writes)
	}
	if got.Labels["app.kubernetes.io/managed-by"] != "kyverno" {
		t.Errorf("managed-by = %q, want kyverno — we must not report having taken ownership",
			got.Labels["app.kubernetes.io/managed-by"])
	}
}

// THE CONTROL, and it is what stops the fix from being "never write a namespace again".
//
// The case the merge was written for — a namespace Helm already manages — must still be merged into,
// and so must one with NO declared manager, because claiming an unowned namespace takes it from
// nobody. Without this arm, an ApplyNamespace that returned early unconditionally would pass the
// test above.
func TestApplyNamespace_StillWritesWhenNobodyElseOwnsIt(t *testing.T) {
	for _, c := range []struct{ name, owner string }{
		{"already Helm-managed (the demo-system case)", "Helm"},
		{"no manager declared at all", ""},
	} {
		cli, writes := captureWrites(t, namespaceOwnedBy(c.owner))
		inst := &RBACInstaller{DynamicClient: cli}

		if _, err := inst.ApplyNamespace(context.Background(),
			CreateNamespace("tenant-acmpe2e", "rel", "krateo-system")); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		// The meta.helm.sh/* annotations are absent on both, so there IS something to add.
		if len(*writes) == 0 {
			t.Errorf("%s: expected a write — the adoption annotations are absent, which is the "+
				"case the merge exists for", c.name)
		}
	}
}

// Idempotency: a namespace already carrying everything we would add must produce no write, so this
// does not churn on every reconcile.
func TestApplyNamespace_NoWriteWhenNothingIsMissing(t *testing.T) {
	desired := CreateNamespace("tenant-acmpe2e", "rel", "krateo-system")
	live := namespaceOwnedBy("")
	lbl := map[string]any{}
	for k, v := range desired.Labels {
		lbl[k] = v
	}
	ann := map[string]any{}
	for k, v := range desired.Annotations {
		ann[k] = v
	}
	_ = unstructured.SetNestedMap(live.Object, lbl, "metadata", "labels")
	_ = unstructured.SetNestedMap(live.Object, ann, "metadata", "annotations")

	cli, writes := captureWrites(t, live)
	inst := &RBACInstaller{DynamicClient: cli}
	if _, err := inst.ApplyNamespace(context.Background(), desired); err != nil {
		t.Fatalf("ApplyNamespace: %v", err)
	}
	if len(*writes) != 0 {
		t.Errorf("nothing was missing, so nothing should have been written; got: %s", *writes)
	}
}

// An absent namespace must still be CREATED with our markers — the fix must not turn an install
// into a no-op on a fresh tenant, which is the way a guard like this goes wrong.
func TestApplyNamespace_StillCreatesWhenAbsent(t *testing.T) {
	cli, _ := captureWrites(t)
	created := false
	cli.PrependReactor("create", "namespaces", func(a k8stesting.Action) (bool, runtime.Object, error) {
		created = true
		return false, nil, nil
	})
	inst := &RBACInstaller{DynamicClient: cli}

	got, err := inst.ApplyNamespace(context.Background(),
		CreateNamespace("tenant-fresh", "rel", "krateo-system"))
	if err != nil {
		t.Fatalf("ApplyNamespace on an absent namespace: %v", err)
	}
	if !created {
		t.Fatal("an absent namespace must be created; the fix must not no-op a fresh install")
	}
	if got.Labels["app.kubernetes.io/managed-by"] != "Helm" {
		t.Errorf("a namespace WE create must carry our adoption marker, got %q",
			got.Labels["app.kubernetes.io/managed-by"])
	}
}

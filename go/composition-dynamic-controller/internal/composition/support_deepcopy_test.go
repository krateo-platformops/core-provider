package composition

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// The #74 child-health rollup writes status.managed. It MUST hold JSON-native maps, not typed
// ManagedResource structs: mg.Object is deep-copied by runtime.DeepCopyJSONValue on the
// observe/converter reconcile path, which panics on any non-JSON type
// ("cannot deep copy composition.ManagedResource") and crash-loops the umbrella controller.
func TestManagedResourcesStatusIsDeepCopyable(t *testing.T) {
	// The shape populateManagedResources now produces (via ToUnstructured).
	u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&ManagedResource{
		APIVersion: "v1", Resource: "configmaps", Name: "x", Namespace: "ns",
		Path: "/api/v1/namespaces/ns/configmaps/x",
	})
	if err != nil {
		t.Fatalf("ToUnstructured: %v", err)
	}
	mg := &unstructured.Unstructured{Object: map[string]interface{}{}}
	setManagedResources(mg, []any{u})

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("DeepCopyJSONValue panicked on the managed status (the production bug): %v", r)
		}
	}()
	// Exactly the call that crash-looped the installers-controller in production.
	_ = runtime.DeepCopyJSONValue(mg.Object)
}

// Guard: a raw typed struct in the status IS what panics — keeps the test above meaningful and
// documents the regression this fix prevents from reappearing.
func TestRawManagedResourceStructPanicsOnDeepCopy(t *testing.T) {
	mg := &unstructured.Unstructured{Object: map[string]interface{}{}}
	setManagedResources(mg, []any{ManagedResource{Name: "x"}})

	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected DeepCopyJSONValue to panic on a raw ManagedResource struct, but it did not")
		}
	}()
	_ = runtime.DeepCopyJSONValue(mg.Object)
}

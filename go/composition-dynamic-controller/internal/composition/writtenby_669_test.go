package composition

import (
	"context"
	"reflect"
	"testing"

	"github.com/krateo-platformops/composition-dynamic-controller/internal/writtenby"
	"github.com/krateo-platformops/unstructured-runtime/pkg/tools"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

type pluralizer669 struct{ gvr schema.GroupVersionResource }

func (p pluralizer669) GVKtoGVR(schema.GroupVersionKind) (schema.GroupVersionResource, error) {
	return p.gvr, nil
}

// core-provider#669: every status the handler writes goes out through updateStatus, which stamps the
// stamp the entry point put on ctx. The handler is built the way the manager builds it (NewHandler
// with HandlerOptions.WrittenBy), and its ctx the way every entry point builds it (withPassContext).
// What is asserted is the body the apiserver was SENT.
func TestUpdateStatusSendsTheHandlersStamp(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1-0-0", Resource: "fireworks"}
	stored := func() *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "composition.krateo.io/v1-0-0", "kind": "Firework",
			"metadata": map[string]any{"name": "fw", "namespace": "demo"},
			"status":   map[string]any{"digest": "d"},
		}}
	}
	send := func(ctx context.Context) map[string]any {
		t.Helper()
		dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{gvr: "FireworkList"}, stored())
		var sent map[string]any
		dyn.PrependReactor("update", gvr.Resource, func(a clienttesting.Action) (bool, runtime.Object, error) {
			if a.GetSubresource() == "status" {
				sent, _, _ = unstructured.NestedMap(a.(clienttesting.UpdateAction).GetObject().(*unstructured.Unstructured).Object, "status")
			}
			return false, nil, nil
		})
		if _, err := updateStatus(ctx, stored(), tools.UpdateOptions{Pluralizer: pluralizer669{gvr}, DynamicClient: dyn}); err != nil {
			t.Fatal(err)
		}
		if sent == nil {
			t.Fatal("control: no status update reached the client")
		}
		return sent
	}

	stamp := writtenby.Stamp{Version: "2.13.135", Commit: "c0ffee135"}
	h := NewHandler(&HandlerOptions{WrittenBy: stamp}).(*handler)

	got := send(h.withPassContext(context.Background()))
	want := map[string]any{"name": "composition-dynamic-controller", "version": "2.13.135", "commit": "c0ffee135"}
	if !reflect.DeepEqual(got["writtenBy"], want) {
		t.Errorf("status.writtenBy sent = %v, want %v", got["writtenBy"], want)
	}
	if got["digest"] != "d" {
		t.Errorf("the stamp changed the rest of the status: %v", got)
	}

	// CONTROL: a context no entry point built carries no stamp, and the body goes out as it was.
	if got := send(context.Background()); got["writtenBy"] != nil {
		t.Errorf("without the entry point's context the status carried writtenBy %v", got["writtenBy"])
	}
}

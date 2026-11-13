package composition

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func crdObj(conds ...map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1",
		"kind":       "CustomResourceDefinition",
		"metadata":   map[string]any{"name": "users.basic.authn.krateo.io"},
	}}
	if conds != nil {
		list := make([]any, 0, len(conds))
		for _, c := range conds {
			list = append(list, c)
		}
		_ = unstructured.SetNestedSlice(u.Object, list, "status", "conditions")
	}
	return u
}

func crdCond(t, s string) map[string]any { return map[string]any{"type": t, "status": s} }

// #473 (Diego, 2026-11-13): a CRD the composition deploys is converging until Established, and a
// name conflict (NamesAccepted=False) is terminal. Before, every apiextensions.k8s.io child was
// healthy by existence, so the composition read Ready=True while its CRD was not yet served.
func TestClassifyChild_CRD(t *testing.T) {
	for _, tc := range []struct {
		name string
		obj  *unstructured.Unstructured
		want childState
	}{
		{"established", crdObj(crdCond("NamesAccepted", "True"), crdCond("Established", "True")), childHealthy},
		{"names accepted, not yet established", crdObj(crdCond("NamesAccepted", "True"), crdCond("Established", "False")), childConverging},
		{"just created, no conditions yet", crdObj(), childConverging},
		{"established but terminating", crdObj(crdCond("NamesAccepted", "True"), crdCond("Established", "True"), crdCond("Terminating", "True")), childConverging},
		{"name conflict is terminal", crdObj(crdCond("NamesAccepted", "False"), crdCond("Established", "False")), childFailed},
	} {
		if got := classifyChild("apiextensions.k8s.io", tc.obj); got != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, got, tc.want)
		}
	}
	// Control: the native fallthrough is untouched; a config kind with no readiness is still healthy
	// by existence, so the CRD case did not widen into the whole native domain.
	cm := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "c"}}}
	if got := classifyChild("", cm); got != childHealthy {
		t.Errorf("ConfigMap: got %d, want childHealthy (existence is health)", got)
	}
	// Another kind in the same group is not a CRD and stays existence-is-health.
	other := crdObj()
	other.SetKind("SomethingElse")
	if got := classifyChild("apiextensions.k8s.io", other); got != childHealthy {
		t.Errorf("apiextensions.k8s.io/SomethingElse: got %d, want childHealthy", got)
	}
}

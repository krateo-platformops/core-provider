//go:build envtest

package crd

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// docs/design/cdc-pooling.md §5c: core-provider runs no admission webhook, so the generated CRD's CEL
// rules are the only gate. A real kube-apiserver (envtest) evaluates them over the CRD as GENERATED,
// so a lost marker or a broken rule fails here, not on a cluster.
func TestPoolAdmissionEnvtest(t *testing.T) {
	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "..", "crds")}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest apiserver: %v", err)
	}
	defer func() { _ = env.Stop() }()
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ri := dyn.Resource(schema.GroupVersionResource{Group: "core.krateo.io", Version: "v1alpha1", Resource: "compositiondefinitions"}).Namespace("default")

	cd := func(name string, controller map[string]any, remote bool) *unstructured.Unstructured {
		spec := map[string]any{"chart": map[string]any{"url": "oci://ghcr.io/example/charts/demo", "version": "1.0.0"}}
		if controller != nil {
			spec["controller"] = controller
		}
		if remote {
			spec["deploy"] = map[string]any{"targetRef": map[string]any{"name": "spoke-1"}}
		}
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "core.krateo.io/v1alpha1", "kind": "CompositionDefinition",
			"metadata": map[string]any{"name": name, "namespace": "default"}, "spec": spec}}
	}
	cases := []struct {
		name   string
		obj    *unstructured.Unstructured
		reject string // "" accepted; otherwise a substring of the refusal
	}{
		{"local pool is accepted", cd("local-pool", map[string]any{"pool": "pages"}, false), ""},
		{"remote pool is refused", cd("remote-pool", map[string]any{"pool": "pages"}, true), "supported for local definitions only"},
		{"pool with scaleToZero true is refused", cd("pool-stz", map[string]any{"pool": "pages", "scaleToZero": true}, false), "mutually exclusive"},
		{"pool with scaleToZero false is accepted", cd("pool-stz-off", map[string]any{"pool": "pages", "scaleToZero": false}, false), ""},
		{"a 54-character pool name is accepted", cd("pool-54", map[string]any{"pool": strings.Repeat("a", 54)}, false), ""},
		{"a 55-character pool name is refused", cd("pool-55", map[string]any{"pool": strings.Repeat("a", 55)}, false), "spec.controller.pool"},
		{"remote without a pool is accepted (control)", cd("remote-plain", nil, true), ""},
		{"local scaleToZero without a pool is accepted (control)", cd("local-stz", map[string]any{"scaleToZero": true}, false), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ri.Create(context.Background(), c.obj, metav1.CreateOptions{})
			switch {
			case c.reject == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case c.reject != "" && err == nil:
				t.Fatalf("accepted; want a refusal naming %q", c.reject)
			case c.reject != "" && !strings.Contains(err.Error(), c.reject):
				t.Fatalf("refused for another reason: %v (want %q)", err, c.reject)
			}
		})
	}
}

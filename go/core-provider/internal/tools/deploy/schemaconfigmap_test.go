package deploy

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"

	kubecli "github.com/krateo-platformops/core-provider/internal/tools/kube"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
)

func cdObj(ns, name, resource string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "core.krateo.io/v1alpha1", "kind": "CompositionDefinition",
		"metadata": map[string]any{"name": name, "namespace": ns},
		"status":   map[string]any{"resource": resource},
	}}
	return u
}

func cmObj(ns, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": name, "namespace": ns},
	}}
}

func hubWith(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		compositionDefinitionGVR: "CompositionDefinitionList",
		configMapGVR:             "ConfigMapList",
	}, objs...)
}

var portalsGVR = schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1-8-127", Resource: "portals"}

// Diego, 2026-11-16: the CD's schema ConfigMap moves to the CD's own namespace, and the copy an
// earlier core-provider wrote into the controller namespace is removed ONLY when provably a leftover.
// frontend's edge: on krateo-057 every CD lives in krateo-system, where the legacy location IS the
// current object, so an unconditional removal would have emptied every form there.
func TestLegacySchemaConfigMapRemovable(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name      string
		hub       *dynamicfake.FakeDynamicClient
		cdNS      string
		listErr   bool
		removable bool
	}{
		{"(i) the CD lives in the controller namespace: the legacy location is the current ConfigMap",
			hubWith(cdObj("krateo-system", "portal", "portals")), "krateo-system", false, false},
		{"(i') the same namespace with no CD listed yet (status not written): still the current ConfigMap",
			hubWith(), "krateo-system", false, false},
		{"(ii) a leader of this resource lives in the controller namespace, this CD elsewhere: that copy is the leader's",
			hubWith(cdObj("krateo-system", "portal", "portals")), "tenant-a", false, false},
		{"the CD list failed: could not determine is never absent",
			hubWith(), "tenant-a", true, false},
		{"no CD of this resource in the controller namespace: a leftover",
			hubWith(cdObj("krateo-system", "frontend", "frontends")), "tenant-a", false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.listErr {
				c.hub.PrependReactor("list", "compositiondefinitions", func(clienttesting.Action) (bool, runtime.Object, error) {
					return true, nil, errors.New("apiserver unavailable")
				})
			}
			got, why := legacySchemaConfigMapRemovable(ctx, c.hub, "krateo-system", c.cdNS, portalsGVR)
			if got != c.removable {
				t.Fatalf("removable = %v (%s), want %v", got, why, c.removable)
			}
		})
	}
}

// The removal takes exactly one object by its derived name. The chart's own template ConfigMap shares
// the -jsonschema-configmap suffix (installer-core-provider-jsonschema-configmap on krateo-057), so a
// suffix match would delete it; it must survive.
func TestRemoveLegacySchemaConfigMapTakesOnlyItsOwnName(t *testing.T) {
	ctx := context.Background()
	legacy := getJsonSchemaConfigmapNN(types.NamespacedName{Namespace: "krateo-system", Name: "portals-v1-8-127"})
	template := "installer-core-provider-jsonschema-configmap"
	hub := hubWith(cmObj("krateo-system", legacy.Name), cmObj("krateo-system", template))

	if err := removeLegacySchemaConfigMap(ctx, hub, legacy); err != nil {
		t.Fatal(err)
	}
	if _, err := hub.Resource(configMapGVR).Namespace("krateo-system").Get(ctx, legacy.Name, metav1.GetOptions{}); err == nil {
		t.Fatalf("the legacy ConfigMap %s was not removed", legacy)
	}
	if _, err := hub.Resource(configMapGVR).Namespace("krateo-system").Get(ctx, template, metav1.GetOptions{}); err != nil {
		t.Fatalf("the chart's template ConfigMap %s was removed with it: %v", template, err)
	}
	// Already gone is success, so a second pass is harmless.
	if err := removeLegacySchemaConfigMap(ctx, hub, legacy); err != nil {
		t.Fatalf("removing an absent legacy ConfigMap failed: %v", err)
	}
}

// The new location: the CD's namespace, under the same derived name.
func TestSchemaConfigMapLivesInTheDefinitionsNamespace(t *testing.T) {
	got := schemaConfigMapNN("tenant-a", types.NamespacedName{Namespace: "krateo-system", Name: "portals-v1-8-127"})
	if got.Namespace != "tenant-a" || got.Name != "portals-v1-8-127-jsonschema-configmap" {
		t.Fatalf("schema ConfigMap at %s, want tenant-a/portals-v1-8-127-jsonschema-configmap", got)
	}
}

// Deploy's step, through applySchemaConfigMap: the ConfigMap lands in the CD's namespace ON THE HUB
// (not on a remote target), the leftover in the controller namespace is removed, the chart's template
// ConfigMap survives, and a dry run removes nothing.
func TestApplySchemaConfigMap(t *testing.T) {
	ctx := context.Background()
	gvr := schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v0-2-61", Resource: "selfservicekrateoes"}
	controllerObject := types.NamespacedName{Namespace: "krateo-system", Name: "selfservicekrateoes-v0-2-61"}
	legacyName := controllerObject.Name + "-jsonschema-configmap"
	template := "installer-core-provider-jsonschema-configmap"
	opts := func(hub, target *dynamicfake.FakeDynamicClient, dry bool) DeployOptions {
		return DeployOptions{GVR: gvr, Namespace: "krateo-bp-selfservice-krateo", ControllerNamespace: "krateo-system",
			HubDynClient: hub, DynClient: target, DryRunServer: dry,
			JsonSchemaTemplatePath: "testdata/configmap_jsonschema_production.yaml",
			JsonSchemaBytes:        []byte(`{"type":"object","properties":{"key":{"type":"string"}}}`)}
	}
	exists := func(c *dynamicfake.FakeDynamicClient, ns, name string) bool {
		_, err := c.Resource(configMapGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
		return err == nil
	}

	t.Run("written in the CD namespace on the hub; the leftover removed; the template kept", func(t *testing.T) {
		hub := hubWith(cmObj("krateo-system", legacyName), cmObj("krateo-system", template))
		target := hubWith()
		cm, err := applySchemaConfigMap(ctx, opts(hub, target, false), controllerObject, kubecli.ApplyOptions{}, logging.NewNopLogger())
		if err != nil {
			t.Fatal(err)
		}
		if cm.Namespace != "krateo-bp-selfservice-krateo" || cm.Name != legacyName || !exists(hub, cm.Namespace, legacyName) {
			t.Fatalf("schema ConfigMap not on the hub in the CD's namespace (got %s/%s)", cm.Namespace, cm.Name)
		}
		if exists(target, cm.Namespace, legacyName) {
			t.Error("the schema ConfigMap was written to the target cluster, where neither the CD nor the portal is")
		}
		if exists(hub, "krateo-system", legacyName) {
			t.Error("the leftover in the controller namespace was not removed")
		}
		if !exists(hub, "krateo-system", template) {
			t.Error("the chart's template ConfigMap was removed")
		}
	})
	t.Run("a dry run removes nothing", func(t *testing.T) {
		hub := hubWith(cmObj("krateo-system", legacyName))
		if _, err := applySchemaConfigMap(ctx, opts(hub, hubWith(), true), controllerObject, kubecli.ApplyOptions{DryRun: []string{"All"}}, logging.NewNopLogger()); err != nil {
			t.Fatal(err)
		}
		if !exists(hub, "krateo-system", legacyName) {
			t.Error("a dry-run pass removed the legacy ConfigMap")
		}
	})
}

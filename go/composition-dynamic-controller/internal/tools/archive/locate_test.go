package archive

import (
	"testing"

	compositionMeta "github.com/krateo-platformops/composition-dynamic-controller/pkg/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/krateo-platformops/unstructured-runtime/pkg/logging"
)

type locatePluralizer struct{}

func (locatePluralizer) GVKtoGVR(gvk schema.GroupVersionKind) (schema.GroupVersionResource, error) {
	switch gvk.Kind {
	case "CompositionDefinition":
		return schema.GroupVersionResource{Group: gvk.Group, Version: gvk.Version, Resource: "compositiondefinitions"}, nil
	}
	return schema.GroupVersionResource{Group: gvk.Group, Version: gvk.Version, Resource: "fireworksapps"}, nil
}

var cdGVR = schema.GroupVersionResource{Group: "core.krateo.io", Version: "v1alpha1", Resource: "compositiondefinitions"}

// #402's case: the definition's chart has a passwordRef, and the Secret it names is GONE. Get
// must still fail (it is what Create/Update use to fetch the chart, and a missing credential there
// is a real error). Locate, which Delete uses, must succeed without reading it and carry everything
// Delete needs: the chart version (isVersionMigrationHandover) and the definition's identity
// (the RBAC fallback). Get's failure on the same fixture is the control: it proves the fixture
// really points at an absent Secret, so Locate's success is not vacuous.
func TestLocateNeedsNoCredentials(t *testing.T) {
	cd := &unstructured.Unstructured{}
	cd.SetAPIVersion("core.krateo.io/v1alpha1")
	cd.SetKind("CompositionDefinition")
	cd.SetName("fireworks-app")
	cd.SetNamespace("demo-system")
	_ = unstructured.SetNestedField(cd.Object, "oci://registry.example/charts/fireworks-app", "spec", "chart", "url")
	_ = unstructured.SetNestedField(cd.Object, "2.0.4", "spec", "chart", "version")
	_ = unstructured.SetNestedField(cd.Object, "deploy-bot", "spec", "chart", "credentials", "username")
	_ = unstructured.SetNestedStringMap(cd.Object, map[string]string{"name": "registry-token", "namespace": "demo-system", "key": "token"},
		"spec", "chart", "credentials", "passwordRef")
	_ = unstructured.SetNestedField(cd.Object, "composition.krateo.io/v2-0-4", "status", "apiVersion")
	_ = unstructured.SetNestedField(cd.Object, "FireworksApp", "status", "kind")

	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		cdGVR: "CompositionDefinitionList",
		{Group: "", Version: "v1", Resource: "secrets"}: "SecretList",
	}, cd)
	g := &dynamicGetter{dynamicClient: dyn, logger: logging.NewNopLogger(), pluralizer: locatePluralizer{}}

	comp := &unstructured.Unstructured{}
	comp.SetAPIVersion("composition.krateo.io/v2-0-4")
	comp.SetKind("FireworksApp")
	comp.SetName("probe")
	comp.SetNamespace("demo-system")
	comp.SetLabels(map[string]string{
		compositionMeta.CompositionDefinitionGroupLabel:     cdGVR.Group,
		compositionMeta.CompositionDefinitionVersionLabel:   cdGVR.Version,
		compositionMeta.CompositionDefinitionResourceLabel:  cdGVR.Resource,
		compositionMeta.CompositionDefinitionNameLabel:      "fireworks-app",
		compositionMeta.CompositionDefinitionNamespaceLabel: "demo-system",
		compositionMeta.CompositionVersionLabel:             "v2-0-4",
	})

	// Control: Get reads the absent Secret and fails.
	if _, err := g.Get(comp); err == nil {
		t.Fatal("control: Get succeeded although the passwordRef Secret does not exist, so the fixture does not exercise #402")
	}

	info, err := g.Locate(comp)
	if err != nil {
		t.Fatalf("Locate failed although it must read no Secret: %v", err)
	}
	if info.Auth != nil {
		t.Errorf("Locate returned credentials %+v; it must return none", info.Auth)
	}
	if info.Version != "2.0.4" {
		t.Errorf("Version = %q, want 2.0.4 (isVersionMigrationHandover reads it)", info.Version)
	}
	if info.CompositionDefinitionInfo == nil || info.CompositionDefinitionInfo.Name != "fireworks-app" || info.CompositionDefinitionInfo.Namespace != "demo-system" {
		t.Errorf("CompositionDefinitionInfo = %+v, want demo-system/fireworks-app", info.CompositionDefinitionInfo)
	}
	// The witness that Locate reads no Secret, not merely that it tolerated a missing one: record
	// only Locate's own calls.
	dyn.ClearActions()
	if _, err := g.Locate(comp); err != nil {
		t.Fatal(err)
	}
	for _, a := range dyn.Actions() {
		if a.GetResource().Resource == "secrets" {
			t.Errorf("Locate touched secrets (%s): it must read no Secret", a.GetVerb())
		}
	}
}

// #419: a definition deleted before core-provider's first status write has no status.apiVersion or
// status.kind. A composition's Delete reads it through Locate and must still find it, from the
// labels that name it exactly, or the namespace deadlocks (the definition waits for its compositions,
// the composition's Delete waits for this status). Get (Create/Update) still requires the status: the
// control. And a status that is PRESENT but malformed stays a hard error for Locate as well, so the
// relaxation is "unset", not "anything wrong with status".
func TestLocateFindsADefinitionWithNoStatusYet(t *testing.T) {
	newCD := func(status map[string]interface{}) *unstructured.Unstructured {
		cd := &unstructured.Unstructured{}
		cd.SetAPIVersion("core.krateo.io/v1alpha1")
		cd.SetKind("CompositionDefinition")
		cd.SetName("fireworks-app")
		cd.SetNamespace("demo-system")
		_ = unstructured.SetNestedField(cd.Object, "oci://registry.example/charts/fireworks-app", "spec", "chart", "url")
		_ = unstructured.SetNestedField(cd.Object, "2.0.4", "spec", "chart", "version")
		if status != nil {
			_ = unstructured.SetNestedMap(cd.Object, status, "status")
		}
		return cd
	}
	comp := &unstructured.Unstructured{}
	comp.SetAPIVersion("composition.krateo.io/v2-0-4")
	comp.SetKind("FireworksApp")
	comp.SetName("probe")
	comp.SetNamespace("demo-system")
	comp.SetLabels(map[string]string{
		compositionMeta.CompositionDefinitionGroupLabel:     cdGVR.Group,
		compositionMeta.CompositionDefinitionVersionLabel:   cdGVR.Version,
		compositionMeta.CompositionDefinitionResourceLabel:  cdGVR.Resource,
		compositionMeta.CompositionDefinitionNameLabel:      "fireworks-app",
		compositionMeta.CompositionDefinitionNamespaceLabel: "demo-system",
		compositionMeta.CompositionVersionLabel:             "v2-0-4",
	})
	getter := func(cd *unstructured.Unstructured) *dynamicGetter {
		dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
			cdGVR: "CompositionDefinitionList",
			{Group: "", Version: "v1", Resource: "secrets"}: "SecretList",
		}, cd)
		return &dynamicGetter{dynamicClient: dyn, logger: logging.NewNopLogger(), pluralizer: locatePluralizer{}}
	}

	// No status at all.
	g := getter(newCD(nil))
	if _, err := g.Get(comp); err == nil {
		t.Fatal("control: Get succeeded on a definition with no status; Create/Update must still require it")
	}
	info, err := g.Locate(comp)
	if err != nil {
		t.Fatalf("Locate failed on a definition with no status yet, which deadlocks its compositions' deletion: %v", err)
	}
	if info.Version != "2.0.4" || info.CompositionDefinitionInfo == nil || info.CompositionDefinitionInfo.Name != "fireworks-app" {
		t.Errorf("Locate = version %q, definition %+v; want 2.0.4 and demo-system/fireworks-app", info.Version, info.CompositionDefinitionInfo)
	}

	// Present but malformed: still a hard error, for Locate too.
	if _, err := getter(newCD(map[string]interface{}{"apiVersion": "not-a-group-version", "kind": "FireworksApp"})).Locate(comp); err == nil {
		t.Error("Locate accepted a definition whose status.apiVersion is malformed; only an UNSET status may be trusted")
	}
}

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

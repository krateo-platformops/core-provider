package compositiondefinitions

import (
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// #633: syncHubCompositionCRDVersions's spec change is a pure function of the hub CRD it replaces, so
// ApplyProjected can recompute it on every write attempt. It must touch only spec.versions and
// spec.conversion: anything another writer put on the hub CRD (an annotation, a label) survives.
func TestMirrorSpokeVersions_633_TouchesOnlyTheVersionSet(t *testing.T) {
	ver := func(name string, served, storage bool) apiextensionsv1.CustomResourceDefinitionVersion {
		return apiextensionsv1.CustomResourceDefinitionVersion{Name: name, Served: served, Storage: storage}
	}
	hub := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "widgets.composition.krateo.io",
			Annotations: map[string]string{"example.krateo.io/third-party": "x"},
			Labels:      map[string]string{"example.krateo.io/owner": "someone-else"}},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
			ver("v1-0-0", true, false), ver("v1-1-0", true, false), ver("vacuum", false, true)}},
	}
	spoke := &apiextensionsv1.CustomResourceDefinition{Spec: apiextensionsv1.CustomResourceDefinitionSpec{
		Versions: []apiextensionsv1.CustomResourceDefinitionVersion{ver("v1-1-0", true, true)}}}

	out, retained := mirrorSpokeVersions(hub, spoke)
	if out.Annotations["example.krateo.io/third-party"] != "x" || out.Labels["example.krateo.io/owner"] != "someone-else" {
		t.Fatalf("another writer's metadata must survive the mirror: annotations %v labels %v", out.Annotations, out.Labels)
	}
	var names []string
	storage := ""
	for _, v := range out.Spec.Versions {
		names = append(names, v.Name)
		if v.Storage {
			storage = v.Name
		}
	}
	if len(names) != 2 || names[0] != "v1-1-0" || names[1] != "vacuum" || storage != "vacuum" || !retained {
		t.Fatalf("want [v1-1-0 vacuum] with vacuum the storage version and retained=true, got %v storage=%q retained=%v", names, storage, retained)
	}
	if len(hub.Spec.Versions) != 3 {
		t.Fatalf("the input hub must not be modified (it is the attempt's copy of the live object): %d versions", len(hub.Spec.Versions))
	}
}

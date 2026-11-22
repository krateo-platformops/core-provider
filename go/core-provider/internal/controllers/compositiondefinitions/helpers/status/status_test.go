package status

import (
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
)

func TestUpdateVersionInfo(t *testing.T) {
	cr := &compositiondefinitionsv1alpha1.CompositionDefinition{
		Status: compositiondefinitionsv1alpha1.CompositionDefinitionStatus{
			Managed: compositiondefinitionsv1alpha1.Managed{
				VersionInfo: []compositiondefinitionsv1alpha1.VersionDetail{},
			},
		},
		Spec: compositiondefinitionsv1alpha1.CompositionDefinitionSpec{
			Chart: &compositiondefinitionsv1alpha1.ChartInfo{
				Repo:    "test-repo",
				Url:     "test-url",
				Version: "test-version",
			},
		},
	}

	crd := &apiextensionsv1.CustomResourceDefinition{
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
				{
					Name:    "v1",
					Served:  true,
					Storage: true,
				},
			},
		},
	}

	gvr := schema.GroupVersionResource{
		Group:    "test-group",
		Version:  "v1",
		Resource: "test-resource",
	}

	UpdateVersionInfo(cr, crd, gvr)

	if len(cr.Status.Managed.VersionInfo) != 1 {
		t.Fatalf("expected 1 version info, got %d", len(cr.Status.Managed.VersionInfo))
	}

	vi := cr.Status.Managed.VersionInfo[0]
	if vi.Version != "v1" {
		t.Errorf("expected version 'v1', got '%s'", vi.Version)
	}
	if vi.Chart == nil {
		t.Fatal("expected chart info to be set")
	}
}

// TestUpdateVersionInfo_ProjectsLiveCRD verifies VersionInfo is a projection of the live CRD:
// a version that is no longer in the CRD (pruned) is dropped, while a surviving NON-current version
// keeps its per-version Chart (needed by the undeploy path). The current version's chart is refreshed
// from spec on every call since #609 ("the last in effect"), so the preserved entry here is an older
// version (v1-0-0) while v2-0-0 is current.
func TestUpdateVersionInfo_ProjectsLiveCRD(t *testing.T) {
	cr := &compositiondefinitionsv1alpha1.CompositionDefinition{
		Status: compositiondefinitionsv1alpha1.CompositionDefinitionStatus{
			Managed: compositiondefinitionsv1alpha1.Managed{
				VersionInfo: []compositiondefinitionsv1alpha1.VersionDetail{
					{Version: "v1-0-0", Served: true, Chart: &compositiondefinitionsv1alpha1.ChartInfoProps{Version: "keep-me"}},
					{Version: "v0-9-0", Served: true}, // pruned from the CRD below -> must be dropped
				},
			},
		},
		Spec: compositiondefinitionsv1alpha1.CompositionDefinitionSpec{
			Chart: &compositiondefinitionsv1alpha1.ChartInfo{Repo: "r", Url: "u", Version: "v"},
		},
	}
	crd := &apiextensionsv1.CustomResourceDefinition{
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
				{Name: "v1-0-0", Served: true},
				{Name: "v2-0-0", Served: true},
				{Name: "vacuum", Served: false, Storage: true},
			},
		},
	}
	gvr := schema.GroupVersionResource{Group: "g", Version: "v2-0-0", Resource: "r"}

	UpdateVersionInfo(cr, crd, gvr)

	got := map[string]*compositiondefinitionsv1alpha1.ChartInfoProps{}
	for _, vi := range cr.Status.Managed.VersionInfo {
		v := vi
		got[v.Version] = v.Chart
	}
	if _, stale := got["v0-9-0"]; stale {
		t.Errorf("pruned version v0-9-0 should have been dropped from VersionInfo, got %v", cr.Status.Managed.VersionInfo)
	}
	if _, ok := got["v1-0-0"]; !ok {
		t.Fatalf("live version v1-0-0 missing from VersionInfo")
	}
	if got["v1-0-0"] == nil || got["v1-0-0"].Version != "keep-me" {
		t.Errorf("surviving non-current version's Chart must be preserved, got %v", got["v1-0-0"])
	}
	if got["v2-0-0"] == nil || got["v2-0-0"].Url != "u" {
		t.Errorf("the current version's Chart is spec.chart, got %v", got["v2-0-0"])
	}
	// vacuum is in the CRD, so it is tracked too (it is never served/listed but is a real version).
	if _, ok := got["vacuum"]; !ok {
		t.Errorf("vacuum version should be tracked (present in CRD)")
	}
}

func TestCurrentGVKAndGVR(t *testing.T) {
	st := compositiondefinitionsv1alpha1.CompositionDefinitionStatus{
		ApiVersion: "composition.krateo.io/v1-0-0",
		Kind:       "Fireworksapp",
		Resource:   "fireworksapps",
	}
	gvk := st.CurrentGVK()
	if gvk.Group != "composition.krateo.io" || gvk.Version != "v1-0-0" || gvk.Kind != "Fireworksapp" {
		t.Errorf("CurrentGVK = %+v", gvk)
	}
	gvr := st.CurrentGVR()
	if gvr.Group != "composition.krateo.io" || gvr.Version != "v1-0-0" || gvr.Resource != "fireworksapps" {
		t.Errorf("CurrentGVR = %+v", gvr)
	}
	// Zero status -> zero refs (no panic).
	var zero compositiondefinitionsv1alpha1.CompositionDefinitionStatus
	if g := zero.CurrentGVK(); g.Empty() == false {
		t.Errorf("zero status CurrentGVK should be empty, got %+v", g)
	}
}

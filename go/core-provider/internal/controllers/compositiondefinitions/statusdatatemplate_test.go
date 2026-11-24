package compositiondefinitions

import (
	"strings"
	"testing"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/tools/objects"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

func TestEncodeStatusDataTemplate(t *testing.T) {
	// empty when nothing declared
	if got := encodeStatusDataTemplate(&compositiondefinitionsv1alpha1.CompositionDefinition{}); got != "" {
		t.Errorf("empty = %q, want \"\"", got)
	}

	cr := &compositiondefinitionsv1alpha1.CompositionDefinition{}
	cr.Spec.StatusDataTemplate = []compositiondefinitionsv1alpha1.StatusFieldMapping{
		{ForPath: "url", Expression: `${ "https://\(.self.spec.host)" }`},
		{ForPath: "ready", Expression: `${ .helm.status == "deployed" }`, Type: "boolean"},
	}
	got := encodeStatusDataTemplate(cr)
	// wire format: lowercase forPath/expression keys, type omitted (only forPath+expression travel).
	want := `[{"forPath":"url","expression":"${ \"https://\\(.self.spec.host)\" }"},{"forPath":"ready","expression":"${ .helm.status == \"deployed\" }"}]`
	if got != want {
		t.Errorf("encoded =\n  %s\nwant\n  %s", got, want)
	}
}

func TestApiRefAccessors(t *testing.T) {
	// no apiRef → all empty (disables .api resolution in the CDC)
	empty := &compositiondefinitionsv1alpha1.CompositionDefinition{}
	if n, ns, ex := apiRefName(empty), apiRefNamespace(empty), encodeApiRefExtras(empty); n != "" || ns != "" || ex != "" {
		t.Errorf("empty apiRef = (%q,%q,%q), want all empty", n, ns, ex)
	}

	cr := &compositiondefinitionsv1alpha1.CompositionDefinition{}
	cr.Spec.ApiRef = &compositiondefinitionsv1alpha1.ApiReference{
		Name:      "status-sources",
		Namespace: "demo",
		Extras:    &apiextensionsv1.JSON{Raw: []byte(`{"region":"eu"}`)},
	}
	if got := apiRefName(cr); got != "status-sources" {
		t.Errorf("name = %q", got)
	}
	if got := apiRefNamespace(cr); got != "demo" {
		t.Errorf("namespace = %q", got)
	}
	if got := encodeApiRefExtras(cr); got != `{"region":"eu"}` {
		t.Errorf("extras = %q", got)
	}

	// apiRef without extras → empty extras, coordinates still present
	cr.Spec.ApiRef.Extras = nil
	if got := encodeApiRefExtras(cr); got != "" {
		t.Errorf("nil extras = %q, want \"\"", got)
	}
}

// The CDC ConfigMap template renders the serialized template into
// COMPOSITION_CONTROLLER_STATUS_DATA_TEMPLATE as a YAML-safe quoted string, so the CDC
// receives the raw JSON. The apiRef coordinates and extras travel alongside.
func TestConfigMapRendersStatusDataTemplate(t *testing.T) {
	cm := corev1.ConfigMap{}
	json := `[{"forPath":"url","expression":"${ .self.spec.host }"}]`
	extras := `{"region":"eu"}`
	err := objects.CreateK8sObject(&cm,
		schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1-0-0", Resource: "fireworksapps"},
		types.NamespacedName{Name: "fireworksapps-v1-0-0-configmap", Namespace: "demo"},
		"testdata/manifests/configmap.yaml",
		envConfigMapTemplateValues(
			"composition_controller_sa_name", "sa",
			"composition_controller_sa_namespace", "demo",
			"status_data_template", json,
			"api_ref_name", "status-sources",
			"api_ref_namespace", "demo",
			"api_ref_extras", extras,
		)...,
	)
	if err != nil {
		t.Fatalf("rendering configmap: %v", err)
	}
	got := cm.Data["COMPOSITION_CONTROLLER_STATUS_DATA_TEMPLATE"]
	if got != json {
		t.Errorf("env value = %q, want raw json %q", got, json)
	}
	if !strings.Contains(got, `"forPath"`) {
		t.Errorf("env value lost JSON structure: %q", got)
	}
	if v := cm.Data["COMPOSITION_CONTROLLER_API_REF_NAME"]; v != "status-sources" {
		t.Errorf("api ref name env = %q", v)
	}
	if v := cm.Data["COMPOSITION_CONTROLLER_API_REF_NAMESPACE"]; v != "demo" {
		t.Errorf("api ref namespace env = %q", v)
	}
	if v := cm.Data["COMPOSITION_CONTROLLER_API_REF_EXTRAS"]; v != extras {
		t.Errorf("api ref extras env = %q, want raw json %q", v, extras)
	}
}

package deploy

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// shippedControllerAuthnURL is what renderControllerObjects passes as authn_url: a value no default
// produces, so the assertion below can only pass if the ConfigMap carries the value it was given.
const shippedControllerAuthnURL = "https://authn.spoke-7.example:9443"

// cdcBuiltinAuthnURL is composition-dynamic-controller's own URL_AUTHN default (main.go, the
// authn-url flag). Kept literal here: the cdc is another module, and this is the value a cluster that
// sets nothing has been running with.
const cdcBuiltinAuthnURL = "http://authn.krateo-system.svc.cluster.local:8082"

// #251 item 4. The controller's authn URL reaches its ConfigMap as URL_AUTHN, the key it reads; and
// with the chart's defaults core-provider's own authn URL (the local controller's source) is exactly
// the URL every controller already used, so the change moves no cluster that sets nothing.
func TestShippedControllerConfigMap_CarriesURLAuthn(t *testing.T) {
	_, cm := renderControllerObjects(t, nil)
	if got, ok := cm.Data["URL_AUTHN"]; !ok || got != shippedControllerAuthnURL {
		t.Fatalf("URL_AUTHN = %q (present %v), want the authn_url passed, %q", got, ok, shippedControllerAuthnURL)
	}

	manifests := renderShippedChart(t, nil)
	var own corev1.ConfigMap
	if err := yaml.Unmarshal([]byte(manifests["core-provider/templates/configmap.yaml"]), &own); err != nil || own.Name == "" {
		t.Fatalf("decoding core-provider's env ConfigMap: %v", err)
	}
	if got := own.Data["CORE_PROVIDER_AUTHN_URL"]; got != cdcBuiltinAuthnURL {
		t.Fatalf("with defaults CORE_PROVIDER_AUTHN_URL = %q, want the controllers' built-in %q: a local controller "+
			"would otherwise move to a different authn on upgrade", got, cdcBuiltinAuthnURL)
	}

	// The control: the chart value moves it, so the assertion above reads the value, not a constant.
	manifests = renderShippedChart(t, map[string]any{"apiRefRBAC": map[string]any{"authnUrl": "http://authn.platform.svc.cluster.local:8082"}})
	if err := yaml.Unmarshal([]byte(manifests["core-provider/templates/configmap.yaml"]), &own); err != nil {
		t.Fatal(err)
	}
	if got := own.Data["CORE_PROVIDER_AUTHN_URL"]; got != "http://authn.platform.svc.cluster.local:8082" {
		t.Fatalf("apiRefRBAC.authnUrl set: CORE_PROVIDER_AUTHN_URL = %q", got)
	}
}

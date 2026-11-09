package deploy

import (
	"testing"

	hasher "github.com/krateo-platformops/plumbing/kubeutil/hasher"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestAuthnNamespaceOrDefault(t *testing.T) {
	if got := authnNamespaceOrDefault(""); got != "krateo-system" {
		t.Errorf("empty -> %q, want krateo-system", got)
	}
	if got := authnNamespaceOrDefault("authn-ns"); got != "authn-ns" {
		t.Errorf("explicit -> %q, want authn-ns", got)
	}
}

func TestAuthnMappingNameAndGroup(t *testing.T) {
	if got := authnMappingName("apps", "fireworksapps-v1-0-0"); got != "cdc-apps-fireworksapps-v1-0-0" {
		t.Errorf("mapping name = %q", got)
	}
	if got := cdcGroup("fireworksapps-v1-0-0"); got != "krateo:cdc:fireworksapps-v1-0-0" {
		t.Errorf("group = %q", got)
	}
}

func TestRenderAuthnServiceAccountMapping(t *testing.T) {
	opts := DeployOptions{
		GVR:                 schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1-0-0", Resource: "fireworksapps"},
		Namespace:           "apps",
		ControllerNamespace: "apps",
		RBACFolderPath:      "testdata",
		ApiRefName:          "status-sources",
		AuthnNamespace:      "krateo-system",
	}
	obj, err := renderAuthnServiceAccountMapping(opts, "fireworksapps-v1-0-0", "apps")
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	if gvk := obj.GroupVersionKind(); gvk.Group != "serviceaccount.authn.krateo.io" || gvk.Kind != "ServiceAccount" || gvk.Version != "v1alpha1" {
		t.Errorf("gvk = %v", gvk)
	}
	if obj.GetName() != "cdc-apps-fireworksapps-v1-0-0" {
		t.Errorf("name = %q", obj.GetName())
	}
	// the mapping lives in the authn operator namespace, not the composition namespace
	if obj.GetNamespace() != "krateo-system" {
		t.Errorf("namespace = %q, want krateo-system", obj.GetNamespace())
	}

	ref, _, _ := unstructured.NestedMap(obj.Object, "spec", "serviceAccountRef")
	if ref["namespace"] != "apps" || ref["name"] != "fireworksapps-v1-0-0" {
		t.Errorf("serviceAccountRef = %v, want {apps, fireworksapps-v1-0-0}", ref)
	}
	groups, _, _ := unstructured.NestedStringSlice(obj.Object, "spec", "groups")
	if len(groups) != 1 || groups[0] != "krateo:cdc:fireworksapps-v1-0-0" {
		t.Errorf("groups = %v, want [krateo:cdc:fireworksapps-v1-0-0]", groups)
	}
}

// The mapping must contribute to the digest only when an apiRef is declared, so existing
// (non-apiRef) compositions keep their digests and toggling apiRef triggers a redeploy.
func TestHashAuthnServiceAccountMapping_GatedByApiRef(t *testing.T) {
	base := DeployOptions{
		ControllerNamespace: "test-ns", Namespace: "apps", AuthnNamespace: "krateo-system"}

	h1 := hasher.NewFNVObjectHash()
	if err := hashAuthnServiceAccountMapping(base, "sa", &h1); err != nil {
		t.Fatalf("hash (no apiRef): %v", err)
	}

	withRef := base
	withRef.ApiRefName = "status-sources"
	h2 := hasher.NewFNVObjectHash()
	if err := hashAuthnServiceAccountMapping(withRef, "sa", &h2); err != nil {
		t.Fatalf("hash (apiRef): %v", err)
	}

	if h1.GetHash() == h2.GetHash() {
		t.Errorf("apiRef toggle did not change the hash contribution (%v == %v)", h1.GetHash(), h2.GetHash())
	}
}

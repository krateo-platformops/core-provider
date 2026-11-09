package deploy

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	definitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	hasher "github.com/krateo-platformops/plumbing/kubeutil/hasher"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

// authnMappingCRD is authn's ServiceAccount mapping CRD serving version v1alpha1, with or without
// spec.principal in its schema (authn-crds 0.39.0 vs 0.37.0).
func authnMappingCRD(served, withPrincipal bool) *unstructured.Unstructured {
	specProps := map[string]interface{}{
		"serviceAccountRef": map[string]interface{}{"type": "object"},
		"groups":            map[string]interface{}{"type": "array"},
	}
	if withPrincipal {
		specProps["principal"] = map[string]interface{}{"type": "string", "enum": []interface{}{"agent", "controller", "service"}}
	}
	crd := &unstructured.Unstructured{}
	crd.SetAPIVersion("apiextensions.k8s.io/v1")
	crd.SetKind("CustomResourceDefinition")
	crd.SetName(authnMappingCRDName)
	_ = unstructured.SetNestedSlice(crd.Object, []interface{}{map[string]interface{}{
		"name": "v1alpha1", "served": served, "storage": true,
		"schema": map[string]interface{}{"openAPIV3Schema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{"spec": map[string]interface{}{
				"type": "object", "properties": specProps,
			}},
		}},
	}}, "spec", "versions")
	return crd
}

func crdClient(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{crdGVR: "CustomResourceDefinitionList"}, objs...)
}

// The gate follows the SERVED schema, on every state it can meet (#405). The two arms that must not
// render "controller" (an older CRD, a non-served version) are what keeps the apply from writing a
// field the CRD would prune or refuse; the failed read must be an error, because answering "" would
// re-render every mapping without principal and flap it on the next good read.
func TestAuthnPrincipalFollowsTheServedCRD(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		dyn     *dynamicfake.FakeDynamicClient
		want    string
		wantErr bool
	}{
		"authn-crds 0.39.0 serves spec.principal": {dyn: crdClient(authnMappingCRD(true, true)), want: "controller"},
		"authn-crds 0.37.0 has no spec.principal": {dyn: crdClient(authnMappingCRD(true, false)), want: ""},
		"the version is not served":               {dyn: crdClient(authnMappingCRD(false, true)), want: ""},
		"no authn mapping CRD at all":             {dyn: crdClient(), want: ""},
	} {
		got, err := authnPrincipal(ctx, tc.dyn)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("%s: authnPrincipal = %q, err %v; want %q, err %v", name, got, err, tc.want, tc.wantErr)
		}
	}

	failing := crdClient(authnMappingCRD(true, true))
	failing.PrependReactor("get", "customresourcedefinitions", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("connection refused")
	})
	if got, err := authnPrincipal(ctx, failing); err == nil {
		t.Errorf("a failed CRD read returned %q with no error; it must be an error, never \"unsupported\"", got)
	}
}

// The four arms authn reviews: delegation absent/present x principal supported/not. The ABSENT arm
// must render no delegation key and no principal key at all, so every existing mapping's full Update
// stays a no-op after #405.
func TestMappingRendersDelegationAndPrincipal(t *testing.T) {
	two := int32(2)
	for name, tc := range map[string]struct {
		delegation    *definitionsv1alpha1.ApiRefDelegation
		principal     string
		wantPrincipal string // "" = key absent
		wantDeleg     map[string]interface{}
	}{
		"no delegation, older CRD (exactly as before #405)": {},
		"no delegation, CRD serves principal":               {principal: "controller", wantPrincipal: "controller"},
		"delegation with maxDepth, older CRD": {
			delegation: &definitionsv1alpha1.ApiRefDelegation{Allowed: true, MaxDepth: &two},
			wantDeleg:  map[string]interface{}{"allowed": true, "maxDepth": int64(2)},
		},
		"delegation without maxDepth, CRD serves principal": {
			delegation: &definitionsv1alpha1.ApiRefDelegation{Allowed: true}, principal: "controller", wantPrincipal: "controller",
			wantDeleg: map[string]interface{}{"allowed": true},
		},
		"delegation declared but not allowed": {
			delegation: &definitionsv1alpha1.ApiRefDelegation{Allowed: false},
			wantDeleg:  map[string]interface{}{"allowed": false},
		},
	} {
		opts := DeployOptions{GVR: testGVR, RBACFolderPath: "testdata", ApiRefName: "status-sources", ApiRefDelegation: tc.delegation}
		obj, err := renderAuthnServiceAccountMapping(opts, "fireworksapps-v1-0-0", "krateo-system", tc.principal)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		p, hasP, _ := unstructured.NestedString(obj.Object, "spec", "principal")
		if hasP != (tc.wantPrincipal != "") || p != tc.wantPrincipal {
			t.Errorf("%s: spec.principal = %q (present %v), want %q", name, p, hasP, tc.wantPrincipal)
		}
		d, hasD, _ := unstructured.NestedMap(obj.Object, "spec", "delegation")
		if hasD != (tc.wantDeleg != nil) {
			t.Errorf("%s: spec.delegation present = %v, want %v (%v)", name, hasD, tc.wantDeleg != nil, d)
			continue
		}
		if tc.wantDeleg != nil {
			if len(d) != len(tc.wantDeleg) {
				t.Errorf("%s: spec.delegation = %v, want exactly %v (maxDepth is rendered only when set)", name, d, tc.wantDeleg)
			}
			for k, v := range tc.wantDeleg {
				if d[k] != v {
					t.Errorf("%s: spec.delegation.%s = %#v, want %#v", name, k, d[k], v)
				}
			}
		}
	}
}

// Declaring delegation, or the CRD starting to serve principal, must change the digest, or the
// definition would read as up to date and the mapping would never be re-rendered.
func TestMappingDigestFollowsDelegationAndPrincipal(t *testing.T) {
	sum := func(d *definitionsv1alpha1.ApiRefDelegation, principal string) string {
		h := hasher.NewFNVObjectHash()
		if err := hashAuthnServiceAccountMapping(DeployOptions{ApiRefName: "x", ApiRefDelegation: d}, "sa", principal, &h); err != nil {
			t.Fatal(err)
		}
		return h.GetHash()
	}
	base := sum(nil, "")
	if base == sum(nil, "controller") {
		t.Error("the digest ignores principal: a CRD that starts serving it would never re-render the mapping")
	}
	if base == sum(&definitionsv1alpha1.ApiRefDelegation{Allowed: true}, "") {
		t.Error("the digest ignores delegation: declaring it would never reach the mapping")
	}
	three := int32(3)
	if sum(&definitionsv1alpha1.ApiRefDelegation{Allowed: true}, "") == sum(&definitionsv1alpha1.ApiRefDelegation{Allowed: true, MaxDepth: &three}, "") {
		t.Error("the digest ignores maxDepth")
	}
	if base != sum(nil, "") {
		t.Error("the digest is not deterministic")
	}
}

// The unit tests render testdata/authn-serviceaccount.yaml, a copy of the shipped asset. A copy that
// drifts makes every test above describe a template that does not ship (#404 lost two CI rounds to
// that class). This fails as soon as the copy differs from the asset with its helm escaping removed.
func TestMappingTemplateCopyIsTheShippedAsset(t *testing.T) {
	asset, err := os.ReadFile("../../../../../helm/core-provider/assets/cdc/rbac/authn-serviceaccount.yaml")
	if err != nil {
		t.Fatal(err)
	}
	copyOf, err := os.ReadFile("testdata/authn-serviceaccount.yaml")
	if err != nil {
		t.Fatal(err)
	}
	unescaped := strings.NewReplacer("{{`", "", "`}}", "").Replace(string(asset))
	if unescaped != string(copyOf) {
		t.Fatalf("testdata/authn-serviceaccount.yaml differs from the shipped asset (helm escaping removed); update the copy:\n--- asset\n%s\n--- copy\n%s", unescaped, copyOf)
	}
	if !strings.Contains(unescaped, ".principal") || !strings.Contains(unescaped, ".delegation") {
		t.Fatal("control: the shipped asset does not carry the #405 keys, so this guard compares the wrong file")
	}
}

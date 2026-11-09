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

// authnMappingCRD is authn's ServiceAccount mapping CRD serving version v1alpha1, with the optional
// fields of a given authn-crds release: 0.36.0 has neither, 0.37.0 adds delegation, 0.39.0 adds
// principal.
func authnMappingCRD(served, withPrincipal, withDelegation bool) *unstructured.Unstructured {
	specProps := map[string]interface{}{
		"serviceAccountRef": map[string]interface{}{"type": "object"},
		"groups":            map[string]interface{}{"type": "array"},
	}
	if withDelegation {
		specProps["delegation"] = map[string]interface{}{"type": "object", "required": []interface{}{"allowed"}}
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

// The gate follows the SERVED schema, field by field, on every state it can meet (#405). A field
// the CRD doesn't serve must not be rendered (the CRD doesn't preserve unknown fields, so the full
// Update would PRUNE it and the mapping would silently differ from the definition). A failed read
// must be an error: answering "unsupported" would re-render every mapping without its fields and flap
// it on the next good read.
func TestAuthnMappingSupportFollowsTheServedCRD(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		dyn  *dynamicfake.FakeDynamicClient
		want AuthnSupport
	}{
		"authn-crds 0.39.0 serves both":       {dyn: crdClient(authnMappingCRD(true, true, true)), want: AuthnSupport{Principal: true, Delegation: true}},
		"authn-crds 0.37.0 serves delegation": {dyn: crdClient(authnMappingCRD(true, false, true)), want: AuthnSupport{Delegation: true}},
		"authn-crds 0.36.0 serves neither":    {dyn: crdClient(authnMappingCRD(true, false, false)), want: AuthnSupport{}},
		"the version is not served":           {dyn: crdClient(authnMappingCRD(false, true, true)), want: AuthnSupport{}},
		"no authn mapping CRD at all":         {dyn: crdClient(), want: AuthnSupport{}},
	} {
		got, err := AuthnMappingSupport(ctx, tc.dyn)
		if err != nil || got != tc.want {
			t.Errorf("%s: AuthnMappingSupport = %+v, err %v; want %+v", name, got, err, tc.want)
		}
	}

	failing := crdClient(authnMappingCRD(true, true, true))
	failing.PrependReactor("get", "customresourcedefinitions", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("connection refused")
	})
	if got, err := AuthnMappingSupport(ctx, failing); err == nil {
		t.Errorf("a failed CRD read returned %+v with no error; it must be an error, never \"unsupported\"", got)
	}
}

// The four arms authn reviews: delegation absent/present x principal supported/not. The ABSENT arm
// must render no delegation key and no principal key at all, so every existing mapping's full Update
// stays a no-op after #405.
func TestMappingRendersDelegationAndPrincipal(t *testing.T) {
	two := int32(2)
	v036 := AuthnSupport{}
	v037 := AuthnSupport{Delegation: true}
	v039 := AuthnSupport{Principal: true, Delegation: true}
	for name, tc := range map[string]struct {
		delegation    *definitionsv1alpha1.ApiRefDelegation
		support       AuthnSupport
		wantPrincipal string // "" = key absent
		wantDeleg     map[string]interface{}
	}{
		"A: no delegation, 0.37.0 (exactly as before #405)": {support: v037},
		"B: no delegation, 0.39.0":                          {support: v039, wantPrincipal: "controller"},
		"C: delegation with maxDepth, 0.37.0": {
			delegation: &definitionsv1alpha1.ApiRefDelegation{Allowed: true, MaxDepth: &two}, support: v037,
			wantDeleg: map[string]interface{}{"allowed": true, "maxDepth": int64(2)},
		},
		"D: delegation without maxDepth, 0.39.0": {
			delegation: &definitionsv1alpha1.ApiRefDelegation{Allowed: true}, support: v039, wantPrincipal: "controller",
			wantDeleg: map[string]interface{}{"allowed": true},
		},
		// authn's review: 0.36.0 has no spec.delegation and doesn't preserve unknown fields, so a
		// rendered delegation would be pruned on the full Update. It is not rendered at all.
		"C on 0.36.0: declared but not served -> not rendered": {
			delegation: &definitionsv1alpha1.ApiRefDelegation{Allowed: true, MaxDepth: &two}, support: v036,
		},
		"declared but not allowed, 0.37.0": {
			delegation: &definitionsv1alpha1.ApiRefDelegation{Allowed: false}, support: v037,
			wantDeleg: map[string]interface{}{"allowed": false},
		},
	} {
		opts := DeployOptions{GVR: testGVR, RBACFolderPath: "testdata", ApiRefName: "status-sources", ApiRefDelegation: tc.delegation, AuthnSupport: tc.support}
		obj, err := renderAuthnServiceAccountMapping(opts, "fireworksapps-v1-0-0", "krateo-system")
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
		support := AuthnSupport{Principal: principal != "", Delegation: true}
		if err := hashAuthnServiceAccountMapping(DeployOptions{ApiRefName: "x", ApiRefDelegation: d, AuthnSupport: support}, "sa", &h); err != nil {
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

// Upgrading authn-crds from 0.36.0 to >= 0.37.0 must re-render a declared delegation: the served-CRD
// change has to move the digest, or the definition would read as up to date and the mapping would
// keep lacking it until something unrelated triggered an Update.
func TestMappingDigestFollowsTheServedCRD(t *testing.T) {
	d := &definitionsv1alpha1.ApiRefDelegation{Allowed: true}
	sum := func(s AuthnSupport) string {
		h := hasher.NewFNVObjectHash()
		if err := hashAuthnServiceAccountMapping(DeployOptions{ApiRefName: "x", ApiRefDelegation: d, AuthnSupport: s}, "sa", &h); err != nil {
			t.Fatal(err)
		}
		return h.GetHash()
	}
	if sum(AuthnSupport{}) == sum(AuthnSupport{Delegation: true}) {
		t.Error("the digest ignores whether the CRD serves delegation: an authn-crds upgrade would never render a declared delegation")
	}
}

package deploy

import (
	"context"
	"errors"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

// selfHub is a hub serving the mapping kind (its CRD Established) unless told otherwise.
func selfHub(established bool, objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	if established {
		crd := authnMappingCRD(true, true, true)
		_ = unstructured.SetNestedSlice(crd.Object, []interface{}{
			map[string]interface{}{"type": "Established", "status": "True"}}, "status", "conditions")
		objs = append(objs, crd)
	}
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{authnServiceAccountGVR: "ServiceAccountList", crdGVR: "CustomResourceDefinitionList"}, objs...)
}

func selfMapping() SelfAuthnMapping {
	return SelfAuthnMapping{SAName: "core-provider", SANamespace: "krateo-system", AuthnNamespace: "krateo-system", RBACFolderPath: "testdata"}
}

// mappingWrites are the write requests a fake saw for the mapping kind.
func mappingWrites(c *dynamicfake.FakeDynamicClient) []string {
	var out []string
	for _, a := range c.Actions() {
		if a.GetResource() == authnServiceAccountGVR && a.GetVerb() != "get" && a.GetVerb() != "list" {
			out = append(out, a.GetVerb())
		}
	}
	return out
}

func liveSelf(t *testing.T, mutate func(*unstructured.Unstructured)) *unstructured.Unstructured {
	t.Helper()
	o, err := selfMapping().render()
	if err != nil {
		t.Fatal(err)
	}
	o.SetResourceVersion("7")
	if mutate != nil {
		mutate(o)
	}
	return o
}

func TestEnsureSelfAuthnMapping(t *testing.T) {
	ctx := context.Background()
	refuse := func(verb string, gvr schema.GroupVersionResource) func(*dynamicfake.FakeDynamicClient) {
		return func(c *dynamicfake.FakeDynamicClient) {
			c.PrependReactor(verb, gvr.Resource, func(clienttesting.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewForbidden(gvr.GroupResource(), "x", errors.New("refused by the test"))
			})
		}
	}
	otherSA := func(o *unstructured.Unstructured) {
		_ = unstructured.SetNestedField(o.Object, "someone-else", "spec", "serviceAccountRef", "name")
	}
	otherDisplay := func(o *unstructured.Unstructured) {
		_ = unstructured.SetNestedField(o.Object, "renamed", "spec", "displayName")
	}
	for name, tc := range map[string]struct {
		hub      *dynamicfake.FakeDynamicClient
		arrange  func(*dynamicfake.FakeDynamicClient)
		mapping  *SelfAuthnMapping
		readOnly bool
		want     MappingState
		wantErr  string
		writes   []string
	}{
		"no identity configured":                       {hub: selfHub(true), mapping: &SelfAuthnMapping{RBACFolderPath: "testdata"}, want: MappingNotNeeded},
		"absent: written":                              {hub: selfHub(true), want: MappingPresent, writes: []string{"create"}},
		"as wanted: no write":                          {hub: selfHub(true, liveSelf(t, nil)), want: MappingPresent},
		"names another ServiceAccount: corrected":      {hub: selfHub(true, liveSelf(t, otherSA)), want: MappingPresent, writes: []string{"update"}},
		"any spec difference: corrected":               {hub: selfHub(true, liveSelf(t, otherDisplay)), want: MappingPresent, writes: []string{"update"}},
		"no CRD: not installed, no error, no write":    {hub: selfHub(false), want: MappingNotInstalled},
		"read only, absent":                            {hub: selfHub(true), readOnly: true, want: MappingMissing},
		"read only, as wanted":                         {hub: selfHub(true, liveSelf(t, nil)), readOnly: true, want: MappingPresent},
		"read only, names another ServiceAccount":      {hub: selfHub(true, liveSelf(t, otherSA)), readOnly: true, want: MappingMissing},
		"read only, authorizes with another display":   {hub: selfHub(true, liveSelf(t, otherDisplay)), readOnly: true, want: MappingPresent},
		"unreadable mapping: an error, not missing":    {hub: selfHub(true), arrange: refuse("get", authnServiceAccountGVR), wantErr: "reading core-provider's own authn mapping"},
		"unreadable CRD: an error, not uninstalled":    {hub: selfHub(true), arrange: refuse("get", crdGVR), wantErr: "reading " + authnMappingCRDName},
		"a refused create is an error":                 {hub: selfHub(true), arrange: refuse("create", authnServiceAccountGVR), wantErr: "refused by the test", writes: []string{"create"}},
		"read only does not turn unreadable to absent": {hub: selfHub(true), arrange: refuse("get", authnServiceAccountGVR), readOnly: true, wantErr: "reading core-provider's own authn mapping"},
	} {
		t.Run(name, func(t *testing.T) {
			if tc.arrange != nil {
				tc.arrange(tc.hub)
			}
			m := selfMapping()
			if tc.mapping != nil {
				m = *tc.mapping
			}
			got, err := EnsureSelfAuthnMapping(ctx, tc.hub, m, !tc.readOnly)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || got != "" {
					t.Fatalf("state %q err %v, want no state and an error containing %q", got, err, tc.wantErr)
				}
			} else if err != nil || got != tc.want {
				t.Fatalf("state %q err %v, want %q", got, err, tc.want)
			}
			// Apply retries a failed write; the verb is what matters, not how often it was tried.
			seen := map[string]bool{}
			for _, w := range mappingWrites(tc.hub) {
				seen[w] = true
			}
			wantSeen := map[string]bool{}
			for _, w := range tc.writes {
				wantSeen[w] = true
			}
			if !equality.Semantic.DeepEqual(seen, wantSeen) {
				t.Errorf("writes = %v, want %v", mappingWrites(tc.hub), tc.writes)
			}
		})
	}
}

// A CRD that exists and does not serve yet is authn not being installed yet, and the very next call
// after it is Established writes the mapping: nothing is remembered.
func TestEnsureSelfAuthnMapping_LooksAgainAfterNotInstalled(t *testing.T) {
	ctx := context.Background()
	hub := selfHub(false, authnMappingCRD(true, true, true)) // no Established condition
	if got, err := EnsureSelfAuthnMapping(ctx, hub, selfMapping(), true); err != nil || got != MappingNotInstalled {
		t.Fatalf("CRD not Established: %q %v, want notInstalled", got, err)
	}
	crd := authnMappingCRD(true, true, true)
	_ = unstructured.SetNestedSlice(crd.Object, []interface{}{map[string]interface{}{"type": "Established", "status": "True"}}, "status", "conditions")
	if _, err := hub.Resource(crdGVR).Update(ctx, crd, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if got, err := EnsureSelfAuthnMapping(ctx, hub, selfMapping(), true); err != nil || got != MappingPresent {
		t.Fatalf("CRD Established: %q %v, want present", got, err)
	}
	if !mappingNames(t, hub)["core-provider"] {
		t.Error("the mapping is not there")
	}
	// Established, but not serving the version written.
	unserved := authnMappingCRD(false, true, true)
	_ = unstructured.SetNestedSlice(unserved.Object, []interface{}{map[string]interface{}{"type": "Established", "status": "True"}}, "status", "conditions")
	if got, err := EnsureSelfAuthnMapping(ctx, selfHub(false, unserved), selfMapping(), true); err != nil || got != MappingNotInstalled {
		t.Errorf("version not served: %q %v, want notInstalled", got, err)
	}
}

// Correcting the spec keeps what other writers put on the object.
func TestEnsureSelfAuthnMapping_KeepsMetadata(t *testing.T) {
	ctx := context.Background()
	hub := selfHub(true, liveSelf(t, func(o *unstructured.Unstructured) {
		o.SetLabels(map[string]string{"kept": "yes"})
		_ = unstructured.SetNestedField(o.Object, "someone-else", "spec", "serviceAccountRef", "name")
	}))
	if _, err := EnsureSelfAuthnMapping(ctx, hub, selfMapping(), true); err != nil {
		t.Fatal(err)
	}
	live, err := hub.Resource(authnServiceAccountGVR).Namespace("krateo-system").Get(ctx, "core-provider", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want, _ := selfMapping().render()
	if live.GetLabels()["kept"] != "yes" || !equality.Semantic.DeepEqual(live.Object["spec"], want.Object["spec"]) {
		t.Errorf("live = %v, want the wanted spec with the label kept", live.Object)
	}
}

func TestControllerAuthnMapping(t *testing.T) {
	ctx := context.Background()
	live := func(mutate func(*unstructured.Unstructured)) *unstructured.Unstructured {
		o, err := renderAuthnServiceAccountMapping(authnOpts(nil, nil, AuthnSupport{Principal: true}), "fireworksapps-v1-0-0", "apps")
		if err != nil {
			t.Fatal(err)
		}
		if mutate != nil {
			mutate(o)
		}
		return o
	}
	for name, tc := range map[string]struct {
		objs    []runtime.Object
		opts    func(*DeployOptions)
		refuse  bool
		want    MappingState
		wantErr bool
	}{
		"no apiRef":           {opts: func(o *DeployOptions) { o.ApiRefName = "" }, want: MappingNotNeeded},
		"no CRD on that side": {opts: func(o *DeployOptions) { o.AuthnSupport = AuthnSupport{NotInstalled: true} }, want: MappingNotInstalled},
		"absent":              {want: MappingMissing},
		"present":             {objs: []runtime.Object{live(nil)}, want: MappingPresent},
		"another pool member's": {objs: []runtime.Object{live(func(o *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(o.Object, true, "spec", "delegation", "allowed")
		})}, want: MappingPresent},
		"names another ServiceAccount": {objs: []runtime.Object{live(func(o *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(o.Object, "x", "spec", "serviceAccountRef", "name")
		})}, want: MappingMissing},
		"other groups": {objs: []runtime.Object{live(func(o *unstructured.Unstructured) {
			_ = unstructured.SetNestedStringSlice(o.Object, []string{"x"}, "spec", "groups")
		})}, want: MappingMissing},
		"unreadable": {refuse: true, wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			c := selfHub(true, tc.objs...)
			if tc.refuse {
				c.PrependReactor("get", "serviceaccounts", func(clienttesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewForbidden(authnServiceAccountGVR.GroupResource(), "x", errors.New("refused"))
				})
			}
			opts := authnOpts(c, nil, AuthnSupport{Principal: true})
			if tc.opts != nil {
				tc.opts(&opts)
			}
			got, err := ControllerAuthnMapping(ctx, opts)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Errorf("state %q err %v, want %q (error %v)", got, err, tc.want, tc.wantErr)
			}
			if w := mappingWrites(c); len(w) != 0 {
				t.Errorf("a read wrote: %v", w)
			}
		})
	}
}

package deploy

import (
	"context"
	"testing"

	kubecli "github.com/krateo-platformops/core-provider/internal/tools/kube"
	hasher "github.com/krateo-platformops/plumbing/kubeutil/hasher"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func authnFake() *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{authnServiceAccountGVR: "ServiceAccountList"})
}

func mappingNames(t *testing.T, c *dynamicfake.FakeDynamicClient) map[string]bool {
	t.Helper()
	l, err := c.Resource(authnServiceAccountGVR).Namespace("krateo-system").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, i := range l.Items {
		out[i.GetName()] = true
	}
	return out
}

func authnOpts(target, hub *dynamicfake.FakeDynamicClient, support AuthnSupport) DeployOptions {
	o := DeployOptions{
		GVR:                 schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1-0-0", Resource: "fireworksapps"},
		Namespace:           "apps",
		ControllerNamespace: "apps",
		RBACFolderPath:      "testdata",
		ApiRefName:          "status-sources",
		AuthnNamespace:      "krateo-system",
		AuthnSupport:        support,
		SelfSAName:          "core-provider",
		SelfSANamespace:     "krateo-system",
		DynClient:           target,
	}
	if hub != nil {
		o.HubDynClient = hub
	}
	return o
}

// #251 items 1 and 2, Diego 2026-11-14. The controller's mapping goes where the controller runs (the
// target), core-provider's OWN goes where core-provider and its authn run (the hub), and a target
// without authn skips the controller's mapping instead of failing the deploy.
func TestApplyAuthnMappings_EachMappingWhereItBelongs(t *testing.T) {
	ctx := context.Background()
	const composition, self = "cdc-apps-fireworksapps-v1-0-0", "core-provider"

	t.Run("remote target with authn: the controller's on the target, core-provider's on the hub", func(t *testing.T) {
		target, hub := authnFake(), authnFake()
		hsh := hasher.NewFNVObjectHash()
		if err := applyAuthnMappings(ctx, authnOpts(target, hub, AuthnSupport{}), "fireworksapps-v1-0-0", "apps", &hsh, kubecli.ApplyOptions{}); err != nil {
			t.Fatal(err)
		}
		if got := mappingNames(t, target); !got[composition] || got[self] {
			t.Errorf("target mappings = %v, want only %s", got, composition)
		}
		if got := mappingNames(t, hub); !got[self] || got[composition] {
			t.Errorf("hub mappings = %v, want only %s", got, self)
		}
	})

	t.Run("remote target WITHOUT authn: nothing written there, no error, the hub still gets core-provider's", func(t *testing.T) {
		target, hub := authnFake(), authnFake()
		hsh := hasher.NewFNVObjectHash()
		if err := applyAuthnMappings(ctx, authnOpts(target, hub, AuthnSupport{NotInstalled: true}), "fireworksapps-v1-0-0", "apps", &hsh, kubecli.ApplyOptions{}); err != nil {
			t.Fatalf("a target without authn failed the deploy: %v", err)
		}
		if got := mappingNames(t, target); len(got) != 0 {
			t.Errorf("target mappings = %v, want none (no authn there)", got)
		}
		if got := mappingNames(t, hub); !got[self] {
			t.Errorf("hub mappings = %v, want %s", got, self)
		}
	})

	t.Run("local definition (no hub client): both on the one cluster", func(t *testing.T) {
		local := authnFake()
		hsh := hasher.NewFNVObjectHash()
		if err := applyAuthnMappings(ctx, authnOpts(local, nil, AuthnSupport{}), "fireworksapps-v1-0-0", "apps", &hsh, kubecli.ApplyOptions{}); err != nil {
			t.Fatal(err)
		}
		if got := mappingNames(t, local); !got[composition] || !got[self] {
			t.Errorf("local mappings = %v, want both", got)
		}
	})
}

// The digest: authn arriving on a target later must change it (so the next Update writes the mapping),
// and the installed digest must be byte-identical to what it was before #251, so no definition
// re-deploys because of this change.
func TestHashAuthnServiceAccountMapping_NotInstalledIsItsOwnDigest(t *testing.T) {
	sum := func(support AuthnSupport) string {
		t.Helper()
		h := hasher.NewFNVObjectHash()
		if err := hashAuthnServiceAccountMapping(authnOpts(nil, nil, support), "fireworksapps-v1-0-0", &h); err != nil {
			t.Fatal(err)
		}
		return h.GetHash()
	}
	installed, missing := sum(AuthnSupport{}), sum(AuthnSupport{NotInstalled: true})
	if installed == missing {
		t.Fatal("not-installed and installed hash the same: authn arriving later would not trigger an Update")
	}
	// The pre-#251 formula, verbatim.
	opts := authnOpts(nil, nil, AuthnSupport{})
	ref := hasher.NewFNVObjectHash()
	delegation, allowed, maxDepth := delegationValues(opts.ApiRefDelegation, opts.AuthnSupport)
	if err := ref.SumHash(authnMappingName(opts.ControllerNamespace, "fireworksapps-v1-0-0"), authnNamespaceOrDefault(opts.AuthnNamespace),
		cdcGroup("fireworksapps-v1-0-0"), principalFor(opts.AuthnSupport), delegation, allowed, maxDepth); err != nil {
		t.Fatal(err)
	}
	if installed != ref.GetHash() {
		t.Errorf("the installed digest changed (%s, was %s): every definition with an apiRef would re-deploy", installed, ref.GetHash())
	}
}

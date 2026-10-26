package compositiondefinitions

import (
	"context"
	"testing"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/tools/deploy"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	contexttools "github.com/krateo-platformops/core-provider/internal/tools/context"
)

var followerGVR = schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v2-0-0", Resource: "fireworksapps"}

func followerInstance(name, versionLabel, cdName, cdNamespace string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(followerGVR.GroupVersion().String())
	u.SetKind("FireworksApp")
	u.SetNamespace("apps")
	u.SetName(name)
	u.SetLabels(map[string]string{
		"krateo.io/composition-version":            versionLabel,
		deploy.CompositionDefinitionNameLabel:      cdName,
		deploy.CompositionDefinitionNamespaceLabel: cdNamespace,
	})
	return u
}

// A follower defers the CRD and the controller to the owner, but not the migration of its OWN
// instances: no other definition migrates them, and an instance left on an old version label holds
// every served-version prune of the Kind back for good. Observe must therefore keep a follower
// not-up-to-date while one of its instances waits for migration, so Update migrates it.
func TestObserveFollower_WaitsForItsOwnInstancesToMigrate(t *testing.T) {
	follower := ownerCD("krateo-bp-fw", "fw", 1)
	follower.Status.ApiVersion = followerGVR.GroupVersion().String()
	follower.Status.Kind = "FireworksApp"
	follower.Status.Managed.VersionInfo = []compositiondefinitionsv1alpha1.VersionDetail{{Version: "v1-0-0"}, {Version: "v2-0-0"}}
	owner := ownerCD("krateo-system", "fw", 0)
	gvk := schema.GroupVersionKind{Group: followerGVR.Group, Version: followerGVR.Version, Kind: "FireworksApp"}

	crd := &apiextensionsv1.CustomResourceDefinition{}
	crd.Spec.Group = followerGVR.Group
	crd.Spec.Names = apiextensionsv1.CustomResourceDefinitionNames{Kind: "FireworksApp", Plural: followerGVR.Resource}
	crd.Spec.Versions = []apiextensionsv1.CustomResourceDefinitionVersion{
		{Name: "v1-0-0", Served: true},
		{Name: "v2-0-0", Served: true, Storage: true},
	}

	observe := func(objs ...runtime.Object) bool {
		t.Helper()
		e := &external{
			kube: clientfake.NewClientBuilder().Build(),
			dynamic: dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
				map[schema.GroupVersionResource]string{followerGVR: "FireworksAppList"}, objs...),
		}
		ctx := contexttools.CtxWithLogger(context.Background(), logging.NewNopLogger())
		obs, err := e.observeFollower(ctx, follower.DeepCopy(), crd, followerGVR, gvk, "", &owner)
		if err != nil {
			t.Fatalf("observeFollower: %v", err)
		}
		return obs.ResourceUpToDate
	}

	if observe(followerInstance("mine", "v1-0-0", "fw", "krateo-bp-fw")) {
		t.Fatal("follower reported up to date while one of its own instances is still on v1-0-0")
	}
	if !observe(followerInstance("theirs", "v1-0-0", "fw", "krateo-system")) {
		t.Fatal("another definition's instance on v1-0-0 must not hold the follower back (the owner migrates it)")
	}

	behind := follower.DeepCopy()
	behind.Status.ApiVersion = "composition.krateo.io/v1-0-0"
	e := &external{kube: clientfake.NewClientBuilder().Build()}
	ctx := contexttools.CtxWithLogger(context.Background(), logging.NewNopLogger())
	obs, err := e.observeFollower(ctx, behind, crd, followerGVR, gvk, "", &owner)
	if err != nil || obs.ResourceUpToDate {
		t.Fatalf("follower whose status is still on the old version must go through Update, got upToDate=%v err=%v", obs.ResourceUpToDate, err)
	}
}

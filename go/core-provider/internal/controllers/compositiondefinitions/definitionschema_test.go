package compositiondefinitions

import (
	"context"
	"errors"
	"testing"

	"github.com/krateo-platformops/core-provider/apis"
	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/tools/deploy"
	rtv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	appsv1 "k8s.io/api/apps/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	contexttools "github.com/krateo-platformops/core-provider/internal/tools/context"
)

var cmGVR = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}

const (
	followerSchema = `{"type":"object","properties":{"replicas":{"type":"integer"}}}`
	otherSchema    = `{"type":"object","properties":{"host":{"type":"string"}}}`
)

// useTestSchemaTemplate points the package at the template the controller extracts at startup.
func useTestSchemaTemplate(t *testing.T) {
	t.Helper()
	prev := JSONSchemaTemplateConfigmapPath
	JSONSchemaTemplateConfigmapPath = "testdata/manifests/json-schema-configmap.yaml"
	t.Cleanup(func() { JSONSchemaTemplateConfigmapPath = prev })
}

func hubDyn(objs ...runtime.Object) *dynfake.FakeDynamicClient {
	return dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{followerGVR: "FireworksAppList", cmGVR: "ConfigMapList"}, objs...)
}

func cmExists(t *testing.T, dyn *dynfake.FakeDynamicClient, ns string) bool {
	t.Helper()
	nn := deploy.DefinitionSchemaConfigMapNN(ns, followerGVR)
	_, err := dyn.Resource(cmGVR).Namespace(nn.Namespace).Get(context.Background(), nn.Name, metav1.GetOptions{})
	return err == nil
}

// A follower runs no Deploy, so before this its namespace had no schema ConfigMap and a tenant
// reading it found no form. publishOwnSchema writes it in the follower's namespace, on the hub and
// not on the target the definition deploys to.
func TestAFollowerPublishesItsSchemaInItsOwnNamespace(t *testing.T) {
	useTestSchemaTemplate(t)
	follower := ownerCD("krateo-bp-fw", "fw", 1)
	hub, target := hubDyn(), hubDyn()
	e := &external{mgmtDynamic: hub, dynamic: target}
	if err := e.publishOwnSchema(context.Background(), &follower, followerGVR, []byte(followerSchema)); err != nil {
		t.Fatal(err)
	}
	if !cmExists(t, hub, "krateo-bp-fw") {
		t.Fatalf("no %s on the hub", deploy.DefinitionSchemaConfigMapNN("krateo-bp-fw", followerGVR))
	}
	if cmExists(t, target, "krateo-bp-fw") {
		t.Error("the schema ConfigMap was written with the target's client")
	}
}

// Observe routes a follower to Update while its copy is missing or stale, and only then.
func TestObserveFollowerReportsItsSchemaConfigMap(t *testing.T) {
	useTestSchemaTemplate(t)
	follower := ownerCD("krateo-bp-fw", "fw", 1)
	follower.Status.ApiVersion = followerGVR.GroupVersion().String()
	follower.Status.Kind = "FireworksApp"
	follower.Spec.Chart = &compositiondefinitionsv1alpha1.ChartInfo{Url: "oci://example/fireworks", Version: "2.0.0"}
	owner := ownerCD("krateo-system", "fw", 0)
	gvk := schema.GroupVersionKind{Group: followerGVR.Group, Version: followerGVR.Version, Kind: "FireworksApp"}
	crd := &apiextensionsv1.CustomResourceDefinition{}
	crd.Spec.Group = followerGVR.Group
	crd.Spec.Names = apiextensionsv1.CustomResourceDefinitionNames{Kind: "FireworksApp", Plural: followerGVR.Resource}
	crd.Spec.Versions = []apiextensionsv1.CustomResourceDefinitionVersion{{Name: followerGVR.Version, Served: true, Storage: true}}
	ctx := contexttools.CtxWithLogger(context.Background(), logging.NewNopLogger())

	observe := func(t *testing.T, hub *dynfake.FakeDynamicClient) (bool, string) {
		t.Helper()
		e := &external{kube: clientfake.NewClientBuilder().Build(), dynamic: hubDyn(), mgmtDynamic: hub}
		cr := follower.DeepCopy()
		obs, err := e.observeFollower(ctx, cr, crd, followerGVR, gvk, "", &owner, []byte(followerSchema))
		if err != nil {
			t.Fatalf("observeFollower: %v", err)
		}
		return obs.ResourceUpToDate, cr.GetCondition(rtv1.TypeReady).Message
	}

	t.Run("missing: not up to date, so Update writes it", func(t *testing.T) {
		if up, why := observe(t, hubDyn()); up {
			t.Fatalf("up to date with no schema ConfigMap (%q)", why)
		}
	})
	t.Run("stale: not up to date", func(t *testing.T) {
		hub := hubDyn()
		if err := deploy.ApplyDefinitionSchemaConfigMap(ctx, hub, followerGVR, "krateo-bp-fw", JSONSchemaTemplateConfigmapPath, []byte(otherSchema)); err != nil {
			t.Fatal(err)
		}
		if up, why := observe(t, hub); up {
			t.Fatalf("up to date with another chart's schema in the ConfigMap (%q)", why)
		}
	})
	t.Run("current: up to date (the control)", func(t *testing.T) {
		hub := hubDyn()
		if err := deploy.ApplyDefinitionSchemaConfigMap(ctx, hub, followerGVR, "krateo-bp-fw", JSONSchemaTemplateConfigmapPath, []byte(followerSchema)); err != nil {
			t.Fatal(err)
		}
		if up, why := observe(t, hub); !up {
			t.Fatalf("not up to date with the current schema in place (%q)", why)
		}
	})
}

// schemaSharer: another live definition in the SAME namespace on the SAME Kind+version publishes the
// same object, judged by status or, before its status exists, by its chart (targetsVersion).
func TestSchemaSharer(t *testing.T) {
	gvk := schema.GroupVersionKind{Group: followerGVR.Group, Version: followerGVR.Version, Kind: "FireworksApp"}
	chart := &compositiondefinitionsv1alpha1.ChartInfo{Url: "oci://example/fireworks", Version: "2.0.0"}
	self := ownerCD("team-a", "leaving", 0)
	self.Spec.Chart = chart
	onStatus := func(ns, name, version string) compositiondefinitionsv1alpha1.CompositionDefinition {
		cd := ownerCD(ns, name, 1)
		cd.Status.ApiVersion = followerGVR.Group + "/" + version
		cd.Status.Kind = "FireworksApp"
		return cd
	}
	bySpec := ownerCD("team-a", "unreconciled", 2)
	bySpec.Spec.Chart = chart.DeepCopy()
	cases := []struct {
		name string
		cds  []compositiondefinitionsv1alpha1.CompositionDefinition
		want string
	}{
		{"alone: none", []compositiondefinitionsv1alpha1.CompositionDefinition{self}, ""},
		{"same namespace, same version by status", []compositiondefinitionsv1alpha1.CompositionDefinition{self, onStatus("team-a", "stays", followerGVR.Version)}, "stays"},
		{"same namespace, no status yet, same chart", []compositiondefinitionsv1alpha1.CompositionDefinition{self, bySpec}, "unreconciled"},
		{"same namespace, another version: another ConfigMap name", []compositiondefinitionsv1alpha1.CompositionDefinition{self, onStatus("team-a", "older", "v1-0-0")}, ""},
		{"another namespace: another ConfigMap", []compositiondefinitionsv1alpha1.CompositionDefinition{self, onStatus("team-b", "elsewhere", followerGVR.Version)}, ""},
		{"a definition also being deleted keeps nothing", []compositiondefinitionsv1alpha1.CompositionDefinition{self, deleting(onStatus("team-a", "going", followerGVR.Version))}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := schemaSharer(&self, c.cds, gvk, chartRefOf(self.Spec.Chart))
			name := ""
			if got != nil {
				name = got.Name
			}
			if name != c.want {
				t.Fatalf("sharer = %q, want %q", name, c.want)
			}
		})
	}
}

// removeOwnSchema on a hand-over delete: removes the leaving definition's copy unless a sharer
// keeps it, and a list that fails removes nothing.
func TestRemoveOwnSchema(t *testing.T) {
	useTestSchemaTemplate(t)
	sc := runtime.NewScheme()
	if err := apis.AddToScheme(sc); err != nil {
		t.Fatal(err)
	}
	gvk := schema.GroupVersionKind{Group: followerGVR.Group, Version: followerGVR.Version, Kind: "FireworksApp"}
	leaving := ownerCD("team-a", "leaving", 0)
	leaving.Spec.Chart = &compositiondefinitionsv1alpha1.ChartInfo{Url: "oci://example/fireworks", Version: "2.0.0"}
	stays := ownerCD("team-a", "stays", 1)
	stays.Status.ApiVersion = followerGVR.GroupVersion().String()
	stays.Status.Kind = "FireworksApp"
	ctx := context.Background()

	run := func(t *testing.T, mgmt client.Client) (*dynfake.FakeDynamicClient, error) {
		t.Helper()
		hub := hubDyn()
		if err := deploy.ApplyDefinitionSchemaConfigMap(ctx, hub, followerGVR, "team-a", JSONSchemaTemplateConfigmapPath, []byte(followerSchema)); err != nil {
			t.Fatal(err)
		}
		e := &external{mgmt: mgmt, mgmtDynamic: hub}
		return hub, e.removeOwnSchema(ctx, &leaving, gvk, followerGVR, logging.NewNopLogger())
	}

	t.Run("no one else in the namespace: removed", func(t *testing.T) {
		hub, err := run(t, clientfake.NewClientBuilder().WithScheme(sc).WithObjects(leaving.DeepCopy()).Build())
		if err != nil {
			t.Fatal(err)
		}
		if cmExists(t, hub, "team-a") {
			t.Fatal("the leaving definition's schema ConfigMap was left behind")
		}
	})
	t.Run("a definition in the namespace publishes the same one: kept", func(t *testing.T) {
		hub, err := run(t, clientfake.NewClientBuilder().WithScheme(sc).WithObjects(leaving.DeepCopy(), stays.DeepCopy()).Build())
		if err != nil {
			t.Fatal(err)
		}
		if !cmExists(t, hub, "team-a") {
			t.Fatal("removed the schema ConfigMap another definition publishes")
		}
	})
	t.Run("the list fails: kept, and the error returned so the delete retries", func(t *testing.T) {
		failing := clientfake.NewClientBuilder().WithScheme(sc).WithInterceptorFuncs(interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return errors.New("apiserver unavailable")
			},
		}).Build()
		hub, err := run(t, failing)
		if err == nil {
			t.Fatal("a failed list was read as no one else uses it")
		}
		if !cmExists(t, hub, "team-a") {
			t.Fatal("removed the schema ConfigMap without knowing who else publishes it")
		}
	})
}

// handedOver must not report a deleting definition gone while its own published schema remains: that
// is what let the finalizer drop with the copy left behind (rehome integration test, CI on #548),
// because a definition reported gone never reaches Delete, where the copy is removed.
func TestHandedOverWaitsForItsOwnSchemaConfigMap(t *testing.T) {
	useTestSchemaTemplate(t)
	sc := runtime.NewScheme()
	if err := apis.AddToScheme(sc); err != nil {
		t.Fatal(err)
	}
	if err := appsv1.AddToScheme(sc); err != nil { // handedOver also looks up the controller Deployment
		t.Fatal(err)
	}
	gvk := schema.GroupVersionKind{Group: "composition.krateo.io", Version: "v1-0-0", Kind: "FireworksApp"}
	gvr := schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1-0-0", Resource: "fireworksapps"}
	onV1 := func(cd compositiondefinitionsv1alpha1.CompositionDefinition) *compositiondefinitionsv1alpha1.CompositionDefinition {
		cd.Status.ApiVersion = "composition.krateo.io/v1-0-0"
		cd.Status.Kind = "FireworksApp"
		return &cd
	}
	leaving := onV1(ownerCD("team-a", "fw", 0))
	elsewhere := onV1(ownerCD("team-b", "fw", 1))   // a successor in another namespace
	sameNS := onV1(ownerCD("team-a", "fw-copy", 2)) // a successor sharing leaving's namespace
	ctx := context.Background()

	check := func(t *testing.T, withCM bool, cds ...*compositiondefinitionsv1alpha1.CompositionDefinition) bool {
		t.Helper()
		var objs []client.Object
		var dynObjs []runtime.Object
		for _, cd := range cds {
			objs = append(objs, cd)
			u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cd)
			if err != nil {
				t.Fatal(err)
			}
			o := &unstructured.Unstructured{Object: u}
			o.SetGroupVersionKind(schema.GroupVersionKind{Group: compositionDefinitionsGVR.Group, Version: compositionDefinitionsGVR.Version, Kind: "CompositionDefinition"})
			dynObjs = append(dynObjs, o)
		}
		cli := clientfake.NewClientBuilder().WithScheme(sc).WithObjects(objs...).Build()
		dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
			map[schema.GroupVersionResource]string{compositionDefinitionsGVR: "CompositionDefinitionList", cmGVR: "ConfigMapList"}, dynObjs...)
		if withCM {
			if err := deploy.ApplyDefinitionSchemaConfigMap(ctx, dyn, gvr, "team-a", JSONSchemaTemplateConfigmapPath, []byte(followerSchema)); err != nil {
				t.Fatal(err)
			}
		}
		got, err := (&external{mgmt: cli, kube: cli, mgmtDynamic: dyn}).handedOver(ctx, leaving, gvk, gvr)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	t.Run("its own copy remains: not handed over, so Delete runs and removes it", func(t *testing.T) {
		if check(t, true, leaving, elsewhere) {
			t.Fatal("reported gone with its own schema ConfigMap still in team-a")
		}
	})
	t.Run("the copy is gone: handed over (the control)", func(t *testing.T) {
		if !check(t, false, leaving, elsewhere) {
			t.Fatal("not reported gone with nothing of its own left")
		}
	})
	t.Run("a successor in the same namespace publishes the same copy: handed over at once", func(t *testing.T) {
		if !check(t, true, leaving, sameNS) {
			t.Fatal("waited on a ConfigMap that is the same-namespace successor's too")
		}
	})
}

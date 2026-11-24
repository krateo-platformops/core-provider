package compositiondefinitions

import (
	"context"
	"testing"

	"github.com/krateo-platformops/core-provider/apis"
	contexttools "github.com/krateo-platformops/core-provider/internal/tools/context"
	"github.com/krateo-platformops/core-provider/internal/tools/deploy"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"
)

// #651. This package's paths that could meet the chart's own template ConfigMaps in the management
// namespace; the by-name ones are in deploy's templateconfigmaps_651_test.go.

// templateConfigMaps are the chart's immutable ConfigMaps (its templates), as rendered: names and labels.
func templateConfigMaps(t *testing.T, metrics bool) []*unstructured.Unstructured {
	t.Helper()
	chrt, err := loadChartForTest(chartDir)
	if err != nil {
		t.Fatal(err)
	}
	vals, err := chartutil.ToRenderValues(chrt, map[string]any{
		"fullnameOverride": "installer-core-provider",
		"cdc":              map[string]any{"metrics": map[string]any{"enabled": metrics}},
	}, chartutil.ReleaseOptions{Name: "installer", Namespace: "krateo-system", IsInstall: true}, chartutil.DefaultCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	files, err := engine.Render(chrt, vals)
	if err != nil {
		t.Fatal(err)
	}
	var out []*unstructured.Unstructured
	for _, body := range files {
		var u unstructured.Unstructured
		if yaml.Unmarshal([]byte(body), &u.Object) != nil || u.GetKind() != "ConfigMap" || u.Object["immutable"] != true {
			continue
		}
		u.SetNamespace("krateo-system")
		out = append(out, &u)
	}
	if len(out) != 5 {
		t.Fatalf("%d immutable ConfigMaps rendered, want the 5 template ones", len(out))
	}
	return out
}

func forEachTemplateRendering(t *testing.T, f func(t *testing.T, cms []*unstructured.Unstructured)) {
	for name, metrics := range map[string]bool{"cdc.metrics off (krateo-057)": false, "cdc.metrics on": true} {
		t.Run(name, func(t *testing.T) { f(t, templateConfigMaps(t, metrics)) })
	}
}

// Path: reconcilePools, the one place that LISTS ConfigMaps (by the pool label). With the templates in
// the namespace it finds the same pools, and the last leaver's teardown leaves them alone.
func TestReconcilePoolsDoesNotSeeTheTemplateConfigMaps(t *testing.T) {
	const ns = "krateo-system"
	old := ManagementNamespace
	ManagementNamespace = ns
	t.Cleanup(func() { ManagementNamespace = old })
	sc := runtime.NewScheme()
	if err := apis.AddToScheme(sc); err != nil {
		t.Fatal(err)
	}
	ctx := contexttools.CtxWithLogger(context.Background(), logging.NewNopLogger())
	leaver := poolCD("team-a", "page", 1, "v1-0-0", "")

	forEachTemplateRendering(t, func(t *testing.T, cms []*unstructured.Unstructured) {
		hub := poolHub(t, ns)
		for _, cm := range cms {
			if err := hub.Tracker().Add(cm.DeepCopy()); err != nil {
				t.Fatal(err)
			}
		}
		e := &external{kube: clientfake.NewClientBuilder().WithScheme(sc).WithObjects(leaver.DeepCopy()).Build(), mgmtDynamic: hub}
		if err := e.reconcilePools(ctx, leaver.DeepCopy(), schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1-0-0", Resource: "pages"}); err != nil {
			t.Fatal(err)
		}
		if idx, dep := poolObjectsLeft(t, hub, ns); idx || dep {
			t.Errorf("pool left behind: index=%v deployment=%v", idx, dep)
		}
		for _, cm := range cms {
			if _, err := hub.Tracker().Get(configMapsGVR, ns, cm.GetName()); err != nil {
				t.Errorf("template ConfigMap %s is gone: %v", cm.GetName(), err)
			}
			if cm.GetLabels()[deploy.PoolLabel] != "" {
				t.Errorf("template ConfigMap %s carries the pool label", cm.GetName())
			}
		}
	})
}

// noConfigMaps is a management-cluster client that fails the test on any ConfigMap request.
func noConfigMaps(t *testing.T, b *clientfake.ClientBuilder) client.Client {
	t.Helper()
	check := func(verb string, o runtime.Object) {
		switch o.(type) {
		case *corev1.ConfigMap, *corev1.ConfigMapList:
			t.Errorf("%s of ConfigMaps", verb)
		}
	}
	return b.WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			check("get", obj)
			return c.Get(ctx, key, obj, opts...)
		},
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			check("list", list)
			return c.List(ctx, list, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			check("delete", obj)
			return c.Delete(ctx, obj, opts...)
		},
	}).Build()
}

// Path: the leaked-controller search, which matches names by prefix and suffix, matches Deployments only.
// What it finds is torn down by deploy.Undeploy (covered there).
func TestFindLeakedControllersReadsNoConfigMap(t *testing.T) {
	e := leakExternal(t, leakCRD("v0-3-484", "vacuum"), nil, leakComposition("installer", "v0-3-484"))
	kube := noConfigMaps(t, clientfake.NewClientBuilder().WithScheme(leakScheme(t)).
		WithObjects(leakCRD("v0-3-484", "vacuum"), leakDeploy("installers-v0-3-484-controller"), leakDeploy("installers-v0-3-480-controller")))
	e.kube, e.mgmt = kube, kube
	leaked, _, err := e.findLeakedControllers(context.Background(), leakGVK("v0-3-484"), leakGVR(), "installer", leakNS, []string{leakNS})
	if err != nil || len(leaked) != 1 {
		t.Fatalf("leaked = %v, %v; want the one leaked controller, or nothing was shown", leaked, err)
	}
}

// Path: scale-to-zero's reading of the controller, and its pods.
func TestControllerConditionsReadNoConfigMap(t *testing.T) {
	sc := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{appsv1.AddToScheme, corev1.AddToScheme} {
		if err := add(sc); err != nil {
			t.Fatal(err)
		}
	}
	dep := leakDeploy("pages-v1-0-0-controller")
	e := &external{kube: noConfigMaps(t, clientfake.NewClientBuilder().WithScheme(sc).WithObjects(dep))}
	cr := poolCD("team-a", "page", 1, "v1-0-0", "")
	old := ManagementNamespace
	ManagementNamespace = leakNS
	t.Cleanup(func() { ManagementNamespace = old })
	_, active, ok, err := e.controllerConditions(context.Background(), &cr,
		controllerPlacement(&cr, schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1-0-0", Resource: "pages"}))
	if err != nil || !ok || active.Type == "" {
		t.Fatalf("active = %+v, %v, %v; want a verdict, or nothing was shown", active, ok, err)
	}
}

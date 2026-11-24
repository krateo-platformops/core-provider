package compositiondefinitions

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	contexttools "github.com/krateo-platformops/core-provider/internal/tools/context"
	"github.com/krateo-platformops/core-provider/internal/tools/deploy"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var configMapsGVR = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}

// poolHub is a management cluster holding pool "general": its index listing the Page v1-0-0
// controller, and its Deployment.
func poolHub(t *testing.T, ns string) *dynfake.FakeDynamicClient {
	t.Helper()
	names := deploy.PoolNames("general")
	pages := schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1-0-0", Resource: "pages"}
	index := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": names.Index, "namespace": ns, "labels": map[string]any{deploy.PoolLabel: "general"}},
		"data":     map[string]any{deploy.MemberKey(pages): `{"configMap":"x","checksum":"y"}`},
	}}
	dep := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": names.Workload, "namespace": ns, "labels": map[string]any{deploy.PoolLabel: "general"}},
		"spec":     map[string]any{"selector": map[string]any{"matchLabels": map[string]any{"app.kubernetes.io/name": names.Workload}}},
	}}
	return dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		configMapsGVR: "ConfigMapList",
		{Group: "apps", Version: "v1", Resource: "deployments"}:                       "DeploymentList",
		{Version: "v1", Resource: "serviceaccounts"}:                                  "ServiceAccountList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}:        "RoleList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}: "RoleBindingList",
	}, index, dep)
}

func poolObjectsLeft(t *testing.T, hub *dynfake.FakeDynamicClient, ns string) (index, deployment bool) {
	t.Helper()
	names := deploy.PoolNames("general")
	_, ierr := hub.Resource(configMapsGVR).Namespace(ns).Get(context.Background(), names.Index, metav1.GetOptions{})
	_, derr := hub.Resource(schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}).Namespace(ns).Get(context.Background(), names.Workload, metav1.GetOptions{})
	return ierr == nil, derr == nil
}

// memberEnv is a Page controller's env ConfigMap as Deploy leaves it in pool "general".
func memberEnv(ns, version string, draining bool) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": "pages-" + version + "-configmap", "namespace": ns, "labels": map[string]any{deploy.PoolLabel: "general"}},
		"data": map[string]any{"COMPOSITION_CONTROLLER_GROUP": "composition.krateo.io", "COMPOSITION_CONTROLLER_VERSION": version,
			"COMPOSITION_CONTROLLER_RESOURCE": "pages"},
	}}
	if draining {
		u.SetAnnotations(map[string]string{deploy.PoolDrainingAnnotation: "true"})
	}
	return u
}

// The pool's teardown arm, by input state. The last leaver deletes the pool; and "could not determine
// the membership" is never "empty": a failed list, or a remaining member, returns an error or keeps the
// pool, and deletes nothing (the plausible value is the destructive one).
func TestReconcilePools_TeardownOnlyOnAKnownEmptyMembership(t *testing.T) {
	ns := ManagementNamespace
	if ns == "" {
		ns = "krateo-system"
		ManagementNamespace = ns
		t.Cleanup(func() { ManagementNamespace = "" })
	}
	ctx := contexttools.CtxWithLogger(context.Background(), logging.NewNopLogger())
	sizing := PoolSizingPath
	PoolSizingPath = filepath.Join(t.TempDir(), "absent.json") // ensuring a pool fails here, and says so
	t.Cleanup(func() { PoolSizingPath = sizing })

	run := func(t *testing.T, hub *dynfake.FakeDynamicClient, cr *compositiondefinitionsv1alpha1.CompositionDefinition, were ...string) error {
		t.Helper()
		e := &external{mgmt: clientfake.NewClientBuilder().Build(), mgmtDynamic: hub}
		left := map[string]bool{}
		for _, p := range were {
			left[p] = true
		}
		return e.reconcilePools(ctx, cr, left)
	}
	with := func(t *testing.T, cms ...*unstructured.Unstructured) *dynfake.FakeDynamicClient {
		hub := poolHub(t, ns)
		for _, cm := range cms {
			if _, err := hub.Resource(configMapsGVR).Namespace(ns).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
		}
		return hub
	}
	leaver := poolCD("team-a", "page", 1, "v1-0-0", "")
	kept := func(t *testing.T, hub *dynfake.FakeDynamicClient) {
		t.Helper()
		if idx, dep := poolObjectsLeft(t, hub, ns); !idx || !dep {
			t.Fatalf("deleted: index=%v deployment=%v", idx, dep)
		}
	}

	t.Run("the last leaver: the pool is deleted", func(t *testing.T) {
		hub := with(t)
		if err := run(t, hub, &leaver, "general"); err != nil {
			t.Fatal(err)
		}
		if idx, dep := poolObjectsLeft(t, hub, ns); idx || dep {
			t.Fatalf("pool left behind: index=%v deployment=%v", idx, dep)
		}
	})
	t.Run("its pods are waited for: nothing they run with goes before them", func(t *testing.T) {
		hub := with(t)
		roles := hub.Resource(schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}).Namespace(ns)
		role := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "Role",
			"metadata": map[string]any{"name": deploy.PoolNames("general").Workload, "namespace": ns}}}
		if _, err := roles.Create(ctx, role, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "draining", Namespace: ns, Labels: map[string]string{"app.kubernetes.io/name": deploy.PoolNames("general").Workload}}}
		sc := runtime.NewScheme()
		_ = corev1.AddToScheme(sc)
		kube := clientfake.NewClientBuilder().WithScheme(sc).WithObjects(pod).Build()
		there := make(chan bool, 1)
		go func() {
			time.Sleep(700 * time.Millisecond)
			_, err := roles.Get(ctx, role.GetName(), metav1.GetOptions{})
			there <- err == nil
			_ = kube.Delete(ctx, pod)
		}()
		e := &external{mgmt: kube, mgmtDynamic: hub}
		if err := e.reconcilePools(ctx, &leaver, map[string]bool{"general": true}); err != nil {
			t.Fatal(err)
		}
		if !<-there {
			t.Error("the pool's Role was deleted while its pod was still there")
		}
		if idx, _ := poolObjectsLeft(t, hub, ns); idx {
			t.Error("the pool was not deleted once its pod was gone")
		}
	})
	t.Run("the pool's ConfigMaps cannot be listed: nothing deleted, error returned", func(t *testing.T) {
		hub := with(t)
		hub.PrependReactor("list", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("apiserver unavailable")
		})
		if err := run(t, hub, &leaver, "general"); err == nil || !strings.Contains(err.Error(), "apiserver unavailable") {
			t.Fatalf("err = %v, want the failed list", err)
		}
		kept(t, hub)
	})
	t.Run("a member remains: the pool is ensured, not deleted", func(t *testing.T) {
		hub := with(t, memberEnv(ns, "v2-0-0", false))
		if err := run(t, hub, &leaver, "general"); err == nil || !strings.Contains(err.Error(), "pool sizing") {
			t.Fatalf("err = %v, want EnsurePool reaching the pool's sizing", err)
		}
		kept(t, hub)
	})
	t.Run("a member is still draining: the pool runs it until it has stopped", func(t *testing.T) {
		hub := with(t, memberEnv(ns, "v1-0-0", true))
		_ = run(t, hub, &leaver, "general")
		kept(t, hub)
	})
	t.Run("another definition's pass leaves a pool it has nothing to do with alone", func(t *testing.T) {
		hub := with(t, memberEnv(ns, "v2-0-0", false))
		other := poolCD("team-b", "other", 2, "v1-0-0", "")
		if err := run(t, hub, &other); err != nil {
			t.Fatal(err)
		}
		kept(t, hub)
	})
	t.Run("a remote definition has no pools", func(t *testing.T) {
		hub := with(t)
		far := remoteCD(poolCD("team-c", "far", 3, "v1-0-0", ""))
		if err := run(t, hub, &far, "general"); err != nil {
			t.Fatal(err)
		}
		kept(t, hub)
	})
}

package compositiondefinitions

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/krateo-platformops/core-provider/apis"
	contexttools "github.com/krateo-platformops/core-provider/internal/tools/context"
	"github.com/krateo-platformops/core-provider/internal/tools/deploy"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

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

// The pool's teardown arm, by input state. The last leaver deletes the pool; and "could not
// determine the membership" is never "empty": a failed list, or a remaining member whose env
// ConfigMap cannot be read, returns an error and deletes nothing (the plausible value is the
// destructive one).
func TestReconcilePools_TeardownOnlyOnAKnownEmptyMembership(t *testing.T) {
	ns := ManagementNamespace
	if ns == "" {
		ns = "krateo-system"
		ManagementNamespace = ns
		t.Cleanup(func() { ManagementNamespace = "" })
	}
	sc := runtime.NewScheme()
	if err := apis.AddToScheme(sc); err != nil {
		t.Fatal(err)
	}
	ctx := contexttools.CtxWithLogger(context.Background(), logging.NewNopLogger())
	pages := schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1-0-0", Resource: "pages"}

	// The leaver: Page v1-0-0's owner, which no longer names a pool.
	leaver := poolCD("team-a", "page", 1, "v1-0-0", "")

	run := func(kube client.Client) (*dynfake.FakeDynamicClient, error) {
		hub := poolHub(t, ns)
		e := &external{kube: kube, mgmtDynamic: hub}
		cr := leaver.DeepCopy()
		return hub, e.reconcilePools(ctx, cr, pages)
	}

	t.Run("the last leaver: the pool is deleted", func(t *testing.T) {
		hub, err := run(clientfake.NewClientBuilder().WithScheme(sc).WithObjects(leaver.DeepCopy()).Build())
		if err != nil {
			t.Fatal(err)
		}
		if idx, dep := poolObjectsLeft(t, hub, ns); idx || dep {
			t.Fatalf("pool left behind: index=%v deployment=%v", idx, dep)
		}
	})

	t.Run("the definitions cannot be listed: nothing deleted, error returned", func(t *testing.T) {
		failing := clientfake.NewClientBuilder().WithScheme(sc).WithInterceptorFuncs(interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return errors.New("apiserver unavailable")
			},
		}).Build()
		hub, err := run(failing)
		if err == nil || !strings.Contains(err.Error(), "listing CompositionDefinitions") {
			t.Fatalf("err = %v, want the failed definitions list", err)
		}
		if idx, dep := poolObjectsLeft(t, hub, ns); !idx || !dep {
			t.Fatalf("deleted on a failed list: index=%v deployment=%v", idx, dep)
		}
	})

	t.Run("a member remains whose env ConfigMap cannot be read: nothing deleted, error returned", func(t *testing.T) {
		stays := poolCD("team-b", "other", 2, "v2-0-0", "general")
		stays.Status.Kind, stays.Status.Resource = "Other", "others"
		hub, err := run(clientfake.NewClientBuilder().WithScheme(sc).WithObjects(leaver.DeepCopy(), &stays).Build())
		// Pinned to the env ConfigMap read: LoadPoolSizing would fail right after it in this test
		// (no sizing file), so a bare err != nil would pass with the read's error swallowed.
		if err == nil || !strings.Contains(err.Error(), "reading member") {
			t.Fatalf("err = %v, want the member's env ConfigMap read", err)
		}
		if idx, dep := poolObjectsLeft(t, hub, ns); !idx || !dep {
			t.Fatalf("deleted a pool that still has a member: index=%v deployment=%v", idx, dep)
		}
	})
}

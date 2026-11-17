package deploy

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// cutoverCluster holds a dedicated Portal v1-8-70 controller (Deployment, Service, ServiceAccount)
// and one composition's derived RoleBinding subjecting that ServiceAccount.
func cutoverCluster(t *testing.T) (*fake.FakeDynamicClient, types.NamespacedName, corev1.ServiceAccount) {
	t.Helper()
	controller := types.NamespacedName{Namespace: saNS, Name: oldSA}
	depName := getCDCDeploymentNN(controller).Name
	obj := func(apiVersion, kind, name string) *unstructured.Unstructured {
		u := &unstructured.Unstructured{}
		u.SetAPIVersion(apiVersion)
		u.SetKind(kind)
		u.SetName(name)
		u.SetNamespace(saNS)
		return u
	}
	dep := obj("apps/v1", "Deployment", depName)
	_ = unstructured.SetNestedStringMap(dep.Object, map[string]string{"app.kubernetes.io/name": depName}, "spec", "selector", "matchLabels")
	dyn := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		roleBindingGVR:        "RoleBindingList",
		clusterRoleBindingGVR: "ClusterRoleBindingList",
		deploymentGVR:         "DeploymentList",
		serviceGVR:            "ServiceList",
		serviceAccountGVR:     "ServiceAccountList",
	},
		binding("RoleBinding", "demo-system", "portal-krateo-rbac", oldSA, saNS),
		dep,
		obj("v1", "Service", depName),
		obj("v1", "ServiceAccount", oldSA),
	)
	sa := corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: oldSA, Namespace: saNS}}
	return dyn, controller, sa
}

// writes is the order of the mutating calls cutoverToPool made, as "verb resource".
func writes(dyn *fake.FakeDynamicClient) []string {
	var out []string
	for _, a := range dyn.Actions() {
		if a.GetVerb() == "update" || a.GetVerb() == "delete" {
			out = append(out, a.GetVerb()+" "+a.GetResource().Resource)
		}
	}
	return out
}

// §5c Cutover, stop-before-start: the compositions' derived RBAC names the pool's ServiceAccount
// BEFORE the dedicated controller is retired, the dedicated Deployment is deleted (and drained) before
// its Service and ServiceAccount, and a failure stops everything after it.
func TestCutoverToPool_Order(t *testing.T) {
	opts := func(dyn *fake.FakeDynamicClient) DeployOptions {
		return DeployOptions{Pool: "general", GVR: oldPortalGVR, DynClient: dyn, KubeClient: clientfake.NewClientBuilder().Build()}
	}

	t.Run("re-subject, then the Deployment, then its Service and ServiceAccount", func(t *testing.T) {
		dyn, controller, sa := cutoverCluster(t)
		if err := cutoverToPool(context.Background(), opts(dyn), controller, sa); err != nil {
			t.Fatal(err)
		}
		got := writes(dyn)
		want := []string{"update rolebindings", "delete deployments", "delete services", "delete serviceaccounts"}
		if len(got) != len(want) {
			t.Fatalf("writes = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("writes = %v, want %v", got, want)
			}
		}
		if s := subjectName(t, dyn, roleBindingGVR, "demo-system", "portal-krateo-rbac"); s != PoolServiceAccount("general", saNS).Name {
			t.Errorf("the derived binding names %q, want the pool's ServiceAccount", s)
		}
	})

	t.Run("idempotent: a second cutover finds nothing to do and succeeds", func(t *testing.T) {
		dyn, controller, sa := cutoverCluster(t)
		o := opts(dyn)
		if err := cutoverToPool(context.Background(), o, controller, sa); err != nil {
			t.Fatal(err)
		}
		if err := cutoverToPool(context.Background(), o, controller, sa); err != nil {
			t.Fatalf("second cutover: %v", err)
		}
	})

	t.Run("the re-subject fails: the dedicated controller is left running", func(t *testing.T) {
		dyn, controller, sa := cutoverCluster(t)
		dyn.PrependReactor("list", "rolebindings", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("apiserver unavailable")
		})
		if err := cutoverToPool(context.Background(), opts(dyn), controller, sa); err == nil {
			t.Fatal("cutover succeeded with the derived RBAC still naming the dedicated ServiceAccount")
		}
		for _, w := range writes(dyn) {
			if w == "delete deployments" || w == "delete serviceaccounts" {
				t.Errorf("%s after a failed re-subject: the pool's member would run without its grants", w)
			}
		}
	})
}

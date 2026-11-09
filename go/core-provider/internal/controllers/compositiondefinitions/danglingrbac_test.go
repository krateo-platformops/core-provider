package compositiondefinitions

import (
	"context"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var (
	rbGVR  = schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}
	crbGVR = schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}
)

// sweepBinding is a RoleBinding in app-ns whose only subject is leakNS/<sa>.
func sweepBinding(name, sa string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding",
		"metadata": map[string]any{"name": name, "namespace": "app-ns"},
		"roleRef":  map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": name},
		"subjects": []any{map[string]any{"kind": "ServiceAccount", "name": sa, "namespace": leakNS}},
	}}
	return u
}

func sweepExternal(t *testing.T, crd *apiextensionsv1.CustomResourceDefinition, remove bool, objs ...runtime.Object) (*external, *dynamicfake.FakeDynamicClient) {
	t.Helper()
	b := clientfake.NewClientBuilder().WithScheme(leakScheme(t))
	if crd != nil {
		b = b.WithObjects(crd)
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		rbGVR: "RoleBindingList", crbGVR: "ClusterRoleBindingList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}:        "RoleList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}: "ClusterRoleList",
		{Version: "v1", Resource: "serviceaccounts"}:                                  "ServiceAccountList",
	}, objs...)
	kc := b.Build()
	return &external{kube: kc, mgmt: kc, dynamic: dyn, removeDanglingRBAC: remove}, dyn
}

func bindingExists(t *testing.T, dyn *dynamicfake.FakeDynamicClient, name string) bool {
	t.Helper()
	_, err := dyn.Resource(rbGVR).Namespace("app-ns").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	return err == nil
}

// #414. The versions that are NOT retired come from the live CRD: v0-3-484 is on it, so its
// binding is never a candidate even though its ServiceAccount is absent. v0-3-480 is off it.
func TestSweepDanglingControllerRBAC(t *testing.T) {
	ctx := context.Background()
	objs := func() []runtime.Object {
		return []runtime.Object{
			sweepBinding("retired", leakResource+"-v0-3-480"),
			sweepBinding("on-crd", leakResource+"-v0-3-484"),
		}
	}

	t.Run("dry run reports and deletes nothing", func(t *testing.T) {
		e, dyn := sweepExternal(t, leakCRD("v0-3-484", "vacuum"), false, objs()...)
		got, err := e.sweepDanglingControllerRBAC(ctx, leakGVR(), []string{leakNS})
		if err != nil {
			t.Fatal(err)
		}
		if !got.DryRun || len(got.Removed) != 0 {
			t.Errorf("dry run: DryRun=%v Removed=%v, want true and none", got.DryRun, got.Removed)
		}
		if names := strings.Join(danglingNames(got.Found), ","); names != "RoleBinding app-ns/retired -> krateo-system/installers-v0-3-480" {
			t.Errorf("found %q, want only the retired version's binding", names)
		}
		if !bindingExists(t, dyn, "retired") {
			t.Error("a dry run deleted the binding")
		}
	})

	t.Run("removal enabled deletes only the retired version's binding", func(t *testing.T) {
		e, dyn := sweepExternal(t, leakCRD("v0-3-484", "vacuum"), true, objs()...)
		got, err := e.sweepDanglingControllerRBAC(ctx, leakGVR(), []string{leakNS})
		if err != nil {
			t.Fatal(err)
		}
		if got.DryRun || len(got.Removed) == 0 {
			t.Errorf("DryRun=%v Removed=%v, want false and the binding", got.DryRun, got.Removed)
		}
		if bindingExists(t, dyn, "retired") {
			t.Error("the retired version's binding is still there")
		}
		if !bindingExists(t, dyn, "on-crd") {
			t.Error("a binding for a version on the CRD was deleted")
		}
	})

	t.Run("no CRD is an error, not every version retired", func(t *testing.T) {
		e, dyn := sweepExternal(t, nil, true, objs()...)
		got, err := e.sweepDanglingControllerRBAC(ctx, leakGVR(), []string{leakNS})
		if err == nil {
			t.Errorf("no CRD returned no error (found %v)", danglingNames(got.Found))
		}
		if !bindingExists(t, dyn, "retired") || !bindingExists(t, dyn, "on-crd") {
			t.Error("a sweep without a CRD deleted something")
		}
	})
}

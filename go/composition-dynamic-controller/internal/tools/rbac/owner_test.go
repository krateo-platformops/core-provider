package rbac

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

const (
	ownerA types.UID = "aaaaaaaa-0000-0000-0000-000000000001"
	ownerB types.UID = "bbbbbbbb-0000-0000-0000-000000000002"
)

var (
	gvrRoles = schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}
	gvrRB    = schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}
	gvrCR    = schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}
	gvrCRB   = schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}
)

func fakeRBACClient(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		gvrRoles: "RoleList", gvrRB: "RoleBindingList", gvrCR: "ClusterRoleList", gvrCRB: "ClusterRoleBindingList",
	}, objs...)
}

func rbacObj(kind, ns, name string, labels map[string]string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("rbac.authorization.k8s.io/v1")
	u.SetKind(kind)
	u.SetNamespace(ns)
	u.SetName(name)
	u.SetLabels(labels)
	return u
}

func owned(uids ...types.UID) map[string]string {
	m := map[string]string{"app.kubernetes.io/part-of": "krateo"}
	for _, u := range uids {
		m[OwnerLabelKey(u)] = "true"
	}
	return m
}

// SetOwner labels every RBAC object and deliberately NOT the Namespaces: removing a Namespace
// because a label matched would delete everything in it.
func TestSetOwnerLabelsRBACButNotNamespaces(t *testing.T) {
	r := &RBAC{
		Namespaces:         []*corev1.Namespace{{ObjectMeta: metav1.ObjectMeta{Name: "app-ns"}}},
		ClusterRole:        &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "x-krateo-rbac"}},
		ClusterRoleBinding: &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "x-krateo-rbac"}},
		Namespaced: map[string]Namespaced{
			"app-ns": {Role: &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "x-krateo-rbac", Namespace: "app-ns"}},
				RoleBinding: &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "x-krateo-rbac", Namespace: "app-ns"}}},
		},
	}
	r.SetOwner(ownerA)
	key := OwnerLabelKey(ownerA)
	for what, l := range map[string]map[string]string{
		"ClusterRole": r.ClusterRole.Labels, "ClusterRoleBinding": r.ClusterRoleBinding.Labels,
		"Role": r.Namespaced["app-ns"].Role.Labels, "RoleBinding": r.Namespaced["app-ns"].RoleBinding.Labels,
	} {
		if l[key] != "true" {
			t.Errorf("%s labels = %v, want %s=true", what, l, key)
		}
	}
	if _, ok := r.Namespaces[0].Labels[key]; ok {
		t.Errorf("the Namespace was labelled as owned (%v); deleting by label would delete its contents", r.Namespaces[0].Labels)
	}
}

// The release by recorded owner, against the three states an object can be in: owned only by
// this composition (deleted), shared with another (kept, only this owner's key removed), and not
// recorded at all (untouched; the legacy regeneration path handles it).
func TestUninstallOwned(t *testing.T) {
	dyn := fakeRBACClient(
		rbacObj("Role", "app-ns", "a-krateo-rbac", owned(ownerA)),
		rbacObj("RoleBinding", "app-ns", "a-krateo-rbac", owned(ownerA)),
		rbacObj("ClusterRole", "", "shared-krateo-rbac", owned(ownerA, ownerB)),
		rbacObj("ClusterRoleBinding", "", "shared-krateo-rbac", owned(ownerA, ownerB)),
		rbacObj("Role", "other-ns", "legacy-krateo-rbac", map[string]string{"app.kubernetes.io/part-of": "krateo"}),
	)
	i := NewRBACInstaller(dyn)

	found, err := i.UninstallOwned(context.Background(), ownerA)
	if err != nil {
		t.Fatal(err)
	}
	if found != 4 {
		t.Errorf("found = %d, want 4 (two sole-owned, two shared)", found)
	}
	ctx := context.Background()
	for _, g := range []struct {
		gvr      schema.GroupVersionResource
		ns, name string
	}{{gvrRoles, "app-ns", "a-krateo-rbac"}, {gvrRB, "app-ns", "a-krateo-rbac"}} {
		if _, err := dyn.Resource(g.gvr).Namespace(g.ns).Get(ctx, g.name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			t.Errorf("%s %s/%s owned only by A should be deleted (err %v)", g.gvr.Resource, g.ns, g.name, err)
		}
	}
	for _, gvr := range []schema.GroupVersionResource{gvrCR, gvrCRB} {
		u, err := dyn.Resource(gvr).Get(ctx, "shared-krateo-rbac", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("%s shared with B was deleted: %v", gvr.Resource, err)
		}
		if _, ok := u.GetLabels()[OwnerLabelKey(ownerA)]; ok {
			t.Errorf("%s still records A after A released it: %v", gvr.Resource, u.GetLabels())
		}
		if u.GetLabels()[OwnerLabelKey(ownerB)] != "true" {
			t.Errorf("%s lost B's ownership: %v", gvr.Resource, u.GetLabels())
		}
	}
	if _, err := dyn.Resource(gvrRoles).Namespace("other-ns").Get(ctx, "legacy-krateo-rbac", metav1.GetOptions{}); err != nil {
		t.Errorf("an object recording no owner was touched: %v", err)
	}

	// Releasing again finds nothing: the caller then takes the legacy path, which is correct only
	// because a release already happened. found==0 is not evidence of anything else.
	again, err := i.UninstallOwned(ctx, ownerA)
	if err != nil || again != 0 {
		t.Errorf("second release: found %d, err %v; want 0, nil", again, err)
	}
}

// A failed LIST must be an error, never "nothing owned": the caller treats 0 as "fall back to the
// chart", and an unreadable cluster is not evidence that nothing is labelled.
func TestUninstallOwnedListFailureIsAnError(t *testing.T) {
	dyn := fakeRBACClient()
	dyn.PrependReactor("list", "clusterroles", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("connection refused")
	})
	if found, err := NewRBACInstaller(dyn).UninstallOwned(context.Background(), ownerA); err == nil {
		t.Fatalf("found %d with no error although LIST failed", found)
	}
}

func TestUninstallOwnedRefusesAnEmptyUID(t *testing.T) {
	if _, err := NewRBACInstaller(fakeRBACClient()).UninstallOwned(context.Background(), ""); err == nil {
		t.Fatal("an empty UID must be refused: its selector matches nothing and would read as \"nothing owned\"")
	}
}

// Existing objects get the owner recorded on their next apply (the backfill), and an apply that
// replaces a whole binding keeps the OTHER owners' keys.
func TestApplyRecordsOwnerAndKeepsOthers(t *testing.T) {
	ctx := context.Background()
	rule := rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"get"}}
	existingRole := rbacObj("Role", "app-ns", "x-krateo-rbac", map[string]string{"app.kubernetes.io/part-of": "krateo"})
	_ = unstructured.SetNestedSlice(existingRole.Object, []interface{}{map[string]interface{}{
		"apiGroups": []interface{}{""}, "resources": []interface{}{"configmaps"}, "verbs": []interface{}{"get"}}}, "rules")
	existingRB := rbacObj("RoleBinding", "app-ns", "x-krateo-rbac", owned(ownerB))
	dyn := fakeRBACClient(existingRole, existingRB)
	i := NewRBACInstaller(dyn)

	// Same rules as already there: before #402 this was "not modified" and the label was never added.
	if _, err := i.ApplyRole(ctx, &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "x-krateo-rbac", Namespace: "app-ns",
		Labels: map[string]string{OwnerLabelKey(ownerA): "true"}}, Rules: []rbacv1.PolicyRule{rule}}); err != nil {
		t.Fatal(err)
	}
	r, _ := dyn.Resource(gvrRoles).Namespace("app-ns").Get(ctx, "x-krateo-rbac", metav1.GetOptions{})
	if r.GetLabels()[OwnerLabelKey(ownerA)] != "true" || r.GetLabels()["app.kubernetes.io/part-of"] != "krateo" {
		t.Errorf("Role labels after apply = %v, want A recorded and existing labels kept", r.GetLabels())
	}

	if _, err := i.ApplyRoleBinding(ctx, &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "x-krateo-rbac", Namespace: "app-ns",
		Labels: map[string]string{OwnerLabelKey(ownerA): "true"}}, RoleRef: rbacv1.RoleRef{Kind: "Role", Name: "x-krateo-rbac"}}); err != nil {
		t.Fatal(err)
	}
	rb, _ := dyn.Resource(gvrRB).Namespace("app-ns").Get(ctx, "x-krateo-rbac", metav1.GetOptions{})
	if rb.GetLabels()[OwnerLabelKey(ownerA)] != "true" || rb.GetLabels()[OwnerLabelKey(ownerB)] != "true" {
		t.Errorf("RoleBinding labels after A's apply = %v, want both A and B recorded", rb.GetLabels())
	}
}

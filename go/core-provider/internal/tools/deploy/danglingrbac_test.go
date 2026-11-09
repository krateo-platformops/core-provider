package deploy

import (
	"context"
	"errors"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

const mgmtNS = "krateo-system"

func danglingClient(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		roleBindingGVR: "RoleBindingList", clusterRoleBindingGVR: "ClusterRoleBindingList",
		roleGVR: "RoleList", clusterRoleGVR: "ClusterRoleList", serviceAccountGVR: "ServiceAccountList",
	}, objs...)
}

// binding builds a RoleBinding (ns != "") or ClusterRoleBinding (ns == "") to roleKind/roleName with
// ServiceAccount subjects given as "namespace/name".
func dBinding(ns, name, roleKind, roleName string, annotations map[string]string, subjects ...string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("rbac.authorization.k8s.io/v1")
	if ns == "" {
		u.SetKind("ClusterRoleBinding")
	} else {
		u.SetKind("RoleBinding")
		u.SetNamespace(ns)
	}
	u.SetName(name)
	u.SetAnnotations(annotations)
	var subs []interface{}
	for _, s := range subjects {
		sns, sname, _ := strings.Cut(s, "/")
		subs = append(subs, map[string]interface{}{"kind": "ServiceAccount", "name": sname, "namespace": sns})
	}
	_ = unstructured.SetNestedSlice(u.Object, subs, "subjects")
	_ = unstructured.SetNestedMap(u.Object, map[string]interface{}{"apiGroup": "rbac.authorization.k8s.io", "kind": roleKind, "name": roleName}, "roleRef")
	return u
}

func dRole(ns, name string, annotations map[string]string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("rbac.authorization.k8s.io/v1")
	if ns == "" {
		u.SetKind("ClusterRole")
	} else {
		u.SetKind("Role")
		u.SetNamespace(ns)
	}
	u.SetName(name)
	u.SetAnnotations(annotations)
	return u
}

func dServiceAccount(ns, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("v1")
	u.SetKind("ServiceAccount")
	u.SetNamespace(ns)
	u.SetName(name)
	return u
}

func portalsOpts(dyn *dynamicfake.FakeDynamicClient) DanglingOptions {
	return DanglingOptions{DynamicClient: dyn, Resource: "portals", KnownVersions: []string{"v1-8-101", "vacuum"}, Placements: []string{mgmtNS}}
}

// The census shapes from krateo-057 (#414), each arm one rule of "retire by subject".
func TestFindDanglingControllerBindings(t *testing.T) {
	helm := map[string]string{helmReleaseNameAnnotation: "some-chart"}
	dyn := danglingClient(
		// FOUND: the pre-#130 bare <release> name, naming a retired, absent controller SA.
		dBinding("app-ns", "portal", "Role", "portal", nil, mgmtNS+"/portals-v1-8-28"),
		dBinding("", "portal", "ClusterRole", "portal", nil, mgmtNS+"/portals-v1-8-28"),
		// FOUND: the hash era. Name shape is irrelevant; the subject decides.
		dBinding("app-ns", "portal-4f2874bz", "Role", "portal-4f2874bz", nil, mgmtNS+"/portals-v1-8-29"),
		// KEPT: its ServiceAccount exists.
		dBinding("app-ns", "portal-live-sa", "Role", "x", nil, mgmtNS+"/portals-v1-8-50"),
		dServiceAccount(mgmtNS, "portals-v1-8-50"),
		// KEPT: helm-owned, a chart's own object.
		dBinding("app-ns", "portal-chart", "Role", "x", helm, mgmtNS+"/portals-v1-8-30"),
		// NOT A CANDIDATE: the version is still on the CRD.
		dBinding("app-ns", "portal-current", "Role", "x", nil, mgmtNS+"/portals-v1-8-101"),
		// NOT A CANDIDATE: a second subject, so it is not this controller's alone. Both orders, so
		// neither the first nor the last subject alone can decide.
		dBinding("app-ns", "portal-mixed", "Role", "x", nil, mgmtNS+"/portals-v1-8-31", mgmtNS+"/someone-else"),
		dBinding("app-ns", "portal-mixed-2", "Role", "x", nil, mgmtNS+"/someone-else", mgmtNS+"/portals-v1-8-31"),
		// NOT A CANDIDATE: outside the controller placements.
		dBinding("app-ns", "portal-elsewhere", "Role", "x", nil, "other-ns/portals-v1-8-32"),
		// NOT A CANDIDATE: another Kind's controller (prefix must be exactly "portals-").
		dBinding("app-ns", "portalagents", "Role", "x", nil, mgmtNS+"/portalagents-v1-8-28"),
	)
	found, kept, err := FindDanglingControllerBindings(context.Background(), portalsOpts(dyn))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range found {
		got = append(got, f.String())
	}
	want := []string{
		"ClusterRoleBinding/portal -> krateo-system/portals-v1-8-28",
		"RoleBinding app-ns/portal -> krateo-system/portals-v1-8-28",
		"RoleBinding app-ns/portal-4f2874bz -> krateo-system/portals-v1-8-29",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("found:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	joined := strings.Join(kept, "\n")
	for _, w := range []string{"RoleBinding app-ns/portal-live-sa: ServiceAccount krateo-system/portals-v1-8-50 exists", "RoleBinding app-ns/portal-chart: helm-owned"} {
		if !strings.Contains(joined, w) {
			t.Errorf("kept = %q, want it to report %q", joined, w)
		}
	}
}

// A ServiceAccount that cannot be read is not absent, and a binding list that fails is not "none":
// both stop the search instead of reporting something to remove.
func TestFindDanglingControllerBindingsRefusesToGuess(t *testing.T) {
	dyn := danglingClient(dBinding("app-ns", "portal", "Role", "portal", nil, mgmtNS+"/portals-v1-8-28"))
	dyn.PrependReactor("get", "serviceaccounts", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("connection refused")
	})
	if found, _, err := FindDanglingControllerBindings(context.Background(), portalsOpts(dyn)); err == nil {
		t.Errorf("an unreadable ServiceAccount was treated as absent: found %v", found)
	}

	dyn2 := danglingClient()
	dyn2.PrependReactor("list", "rolebindings", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("connection refused")
	})
	if _, _, err := FindDanglingControllerBindings(context.Background(), portalsOpts(dyn2)); err == nil {
		t.Error("a failed binding list reported a result")
	}
	// A clean client, so the refusal below can only come from the empty scope, not from a list.
	clean := danglingClient(dBinding("app-ns", "portal", "Role", "portal", nil, mgmtNS+"/portals-v1-8-28"))
	if _, _, err := FindDanglingControllerBindings(context.Background(), DanglingOptions{DynamicClient: clean, Resource: "portals"}); err == nil {
		t.Error("a search with no placements must be refused")
	}
	if _, _, err := FindDanglingControllerBindings(context.Background(), DanglingOptions{DynamicClient: clean, Placements: []string{mgmtNS}}); err == nil {
		t.Error("a search with no resource must be refused")
	}
}

// Removal deletes the bindings, then each role ONLY if no remaining binding references it and it is
// not helm-owned.
func TestRemoveDanglingControllerBindings(t *testing.T) {
	ctx := context.Background()
	helm := map[string]string{helmReleaseNameAnnotation: "some-chart"}
	dyn := danglingClient(
		dBinding("app-ns", "portal", "Role", "portal", nil, mgmtNS+"/portals-v1-8-28"),
		dRole("app-ns", "portal", nil), // referenced only by the dangling binding: removed
		dBinding("app-ns", "portal-b", "Role", "shared", nil, mgmtNS+"/portals-v1-8-28"),
		dBinding("app-ns", "keeps-shared", "Role", "shared", nil, mgmtNS+"/a-live-sa"),
		dRole("app-ns", "shared", nil), // still referenced by keeps-shared: kept
		dBinding("", "portal", "ClusterRole", "portal-helm", nil, mgmtNS+"/portals-v1-8-28"),
		dRole("", "portal-helm", helm), // helm-owned: kept
	)
	found, _, err := FindDanglingControllerBindings(ctx, portalsOpts(dyn))
	if err != nil || len(found) != 3 {
		t.Fatalf("setup: found %d (%v), err %v; want the 3 dangling bindings", len(found), found, err)
	}
	removed, err := RemoveDanglingControllerBindings(ctx, dyn, found)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(removed, "\n")
	for _, w := range []string{"RoleBinding app-ns/portal -> ", "RoleBinding app-ns/portal-b -> ", "ClusterRoleBinding/portal -> ", "Role/app-ns/portal"} {
		if !strings.Contains(got, w) {
			t.Errorf("removed = %q, want it to include %q", got, w)
		}
	}
	for _, gone := range []struct {
		gvr      schema.GroupVersionResource
		ns, name string
	}{{roleBindingGVR, "app-ns", "portal"}, {roleBindingGVR, "app-ns", "portal-b"}, {clusterRoleBindingGVR, "", "portal"}, {roleGVR, "app-ns", "portal"}} {
		if _, err := dyn.Resource(gone.gvr).Namespace(gone.ns).Get(ctx, gone.name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			t.Errorf("%s %s/%s should be gone (err %v)", gone.gvr.Resource, gone.ns, gone.name, err)
		}
	}
	for _, stays := range []struct {
		gvr      schema.GroupVersionResource
		ns, name string
	}{{roleGVR, "app-ns", "shared"}, {clusterRoleGVR, "", "portal-helm"}, {roleBindingGVR, "app-ns", "keeps-shared"}} {
		if _, err := dyn.Resource(stays.gvr).Namespace(stays.ns).Get(ctx, stays.name, metav1.GetOptions{}); err != nil {
			t.Errorf("%s %s/%s must stay: %v", stays.gvr.Resource, stays.ns, stays.name, err)
		}
	}
}

package deploy

import (
	"context"
	"os"
	"regexp"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
)

const (
	saNS   = "krateo-system"
	oldSA  = "portals-v1-8-70"
	newSA  = "portals-v1-8-71"
	otherA = "frontends-v1-8-4"
)

var (
	oldPortalGVR = schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1-8-70", Resource: "portals"}
	newPortalGVR = schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1-8-71", Resource: "portals"}
)

func binding(kind, namespace, name, saName, saNamespace string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("rbac.authorization.k8s.io/v1")
	u.SetKind(kind)
	u.SetName(name)
	if namespace != "" {
		u.SetNamespace(namespace)
	}
	_ = unstructured.SetNestedSlice(u.Object, []any{
		map[string]any{"kind": "ServiceAccount", "name": saName, "namespace": saNamespace},
	}, "subjects")
	return u
}

func rbacDyn(objs ...runtime.Object) *fake.FakeDynamicClient {
	return fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			roleBindingGVR:        "RoleBindingList",
			clusterRoleBindingGVR: "ClusterRoleBindingList",
		},
		objs...,
	)
}

func subjectName(t *testing.T, dyn *fake.FakeDynamicClient, gvr schema.GroupVersionResource, namespace, name string) string {
	t.Helper()
	got, err := dyn.Resource(gvr).Namespace(namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting %s %s/%s: %v", gvr.Resource, namespace, name, err)
	}
	subjects, found, err := unstructured.NestedSlice(got.Object, "subjects")
	if err != nil || !found || len(subjects) == 0 {
		t.Fatalf("no subjects on %s %s/%s (found=%v err=%v)", gvr.Resource, namespace, name, found, err)
	}
	return subjects[0].(map[string]any)["name"].(string)
}

// A version bump retires the ServiceAccount the derived bindings name. Every binding the platform
// generated must follow the identity; nothing else may be touched.
func TestResubjectDerivedRBAC_MovesOnlyWhatThePlatformGenerated(t *testing.T) {
	dyn := rbacDyn(
		// Derived, in the CompositionDefinition's own namespace: the ordinary case.
		binding("RoleBinding", saNS, "portal-krateo-rbac", oldSA, saNS),
		// Derived, in ANOTHER namespace: a composition in demo-system whose controller identity
		// lives in krateo-system. Observed live on krateo-057, and the reason the sweep cannot be
		// scoped to the definition's namespace.
		binding("RoleBinding", "demo-system", "portal-krateo-rbac", oldSA, saNS),
		binding("ClusterRoleBinding", "", "portal-krateo-rbac", oldSA, saNS),
		// NOT derived: pre-rename debris, or a chart's own binding under its fullname. It names the
		// retired identity too, but repointing it would hand a live ServiceAccount whatever rules
		// that orphaned role still holds.
		binding("ClusterRoleBinding", "", "portal", oldSA, saNS),
		binding("RoleBinding", saNS, "portal", oldSA, saNS),
		// Derived, but another Kind's controller: untouched, its own bump will move it.
		binding("ClusterRoleBinding", "", "frontend-krateo-rbac", otherA, saNS),
		// Derived, already current: must not be counted as a repair.
		binding("ClusterRoleBinding", "", "portal-agents-krateo-rbac", newSA, saNS),
		// Derived, same ServiceAccount NAME in a different namespace — a different identity.
		binding("RoleBinding", "other-system", "tenant-portal-krateo-rbac", oldSA, "other-system"),
	)

	repaired, err := ResubjectDerivedRBAC(context.Background(), ResubjectOptions{
		DynamicClient: dyn,
		Namespace:     saNS,
		OldGVR:        oldPortalGVR,
		NewGVR:        newPortalGVR,
	})
	if err != nil {
		t.Fatalf("ResubjectDerivedRBAC: %v", err)
	}
	if repaired != 3 {
		t.Errorf("repaired = %d, want 3 (two RoleBindings and one ClusterRoleBinding)", repaired)
	}

	for _, want := range []struct {
		gvr                 schema.GroupVersionResource
		namespace, name, sa string
		why                 string
	}{
		{roleBindingGVR, saNS, "portal-krateo-rbac", newSA, "derived binding in the definition's namespace"},
		{roleBindingGVR, "demo-system", "portal-krateo-rbac", newSA, "derived binding in another namespace"},
		{clusterRoleBindingGVR, "", "portal-krateo-rbac", newSA, "derived cluster binding"},
		{clusterRoleBindingGVR, "", "portal", oldSA, "chart-named binding must not be repointed"},
		{roleBindingGVR, saNS, "portal", oldSA, "chart-named binding must not be repointed"},
		{clusterRoleBindingGVR, "", "frontend-krateo-rbac", otherA, "another Kind's controller"},
		{clusterRoleBindingGVR, "", "portal-agents-krateo-rbac", newSA, "already current"},
		{roleBindingGVR, "other-system", "tenant-portal-krateo-rbac", oldSA, "same SA name, different namespace"},
	} {
		if got := subjectName(t, dyn, want.gvr, want.namespace, want.name); got != want.sa {
			t.Errorf("%s: %s %s/%s subject = %q, want %q",
				want.why, want.gvr.Resource, want.namespace, want.name, got, want.sa)
		}
	}
}

// Running the sweep twice must repair nothing the second time: the reconcile that calls it is
// re-entered on every CompositionDefinition change, and a repair that re-counted (or re-wrote)
// would make "nothing left to fix" indistinguishable from "fixed it again".
func TestResubjectDerivedRBAC_IsIdempotent(t *testing.T) {
	dyn := rbacDyn(binding("ClusterRoleBinding", "", "portal-krateo-rbac", oldSA, saNS))
	opts := ResubjectOptions{DynamicClient: dyn, Namespace: saNS, OldGVR: oldPortalGVR, NewGVR: newPortalGVR}

	first, err := ResubjectDerivedRBAC(context.Background(), opts)
	if err != nil || first != 1 {
		t.Fatalf("first sweep: repaired = %d, err = %v; want 1, nil", first, err)
	}

	second, err := ResubjectDerivedRBAC(context.Background(), opts)
	if err != nil || second != 0 {
		t.Fatalf("second sweep: repaired = %d, err = %v; want 0, nil", second, err)
	}
}

// A no-op rotation (same version deployed again) must not walk the cluster at all.
func TestResubjectDerivedRBAC_SameVersionIsANoOp(t *testing.T) {
	dyn := rbacDyn(binding("ClusterRoleBinding", "", "portal-krateo-rbac", oldSA, saNS))

	repaired, err := ResubjectDerivedRBAC(context.Background(), ResubjectOptions{
		DynamicClient: dyn,
		Namespace:     saNS,
		OldGVR:        oldPortalGVR,
		NewGVR:        oldPortalGVR,
	})
	if err != nil {
		t.Fatalf("ResubjectDerivedRBAC: %v", err)
	}
	if repaired != 0 {
		t.Errorf("repaired = %d, want 0", repaired)
	}
	if actions := len(dyn.Actions()); actions != 0 {
		t.Errorf("issued %d API calls for an unchanged version, want 0", actions)
	}
}

// The suffix is the safety predicate for rewriting someone else's binding, and its owner lives in
// another Go module (composition-dynamic-controller), so the compiler cannot keep the two in step.
// Read the owner's declaration rather than restating it: a copy that asserts against itself would
// agree with anything, including a rename that silently narrows this sweep to nothing.
func TestDerivedRBACSuffixMatchesRBACGen(t *testing.T) {
	const owner = "../../../../composition-dynamic-controller/internal/rbacgen/rbacgen.go"

	src, err := os.ReadFile(owner)
	if err != nil {
		t.Fatalf("reading %s — if rbacgen moved, this test must follow it rather than be deleted: %v", owner, err)
	}

	m := regexp.MustCompile(`rbacNameSuffix\s*=\s*"([^"]*)"`).FindSubmatch(src)
	if m == nil {
		t.Fatalf("no rbacNameSuffix declaration found in %s", owner)
	}
	if got := string(m[1]); got != DerivedRBACNameSuffix {
		t.Errorf("rbacgen.rbacNameSuffix = %q, DerivedRBACNameSuffix = %q — the sweep would stop "+
			"matching the objects cdc generates", got, DerivedRBACNameSuffix)
	}
}

// Moving a Kind+version's controller to another definition's namespace keeps the ServiceAccount
// NAME and changes its namespace: every derived binding naming the old identity must follow it,
// and a binding naming the same SA name in a third namespace (another identity) must not.
func TestResubjectDerivedRBAC_FollowsANamespaceMove(t *testing.T) {
	const movedNS = "krateo-bp-portal"
	dyn := rbacDyn(
		binding("RoleBinding", "demo-system", "portal-krateo-rbac", oldSA, saNS),
		binding("ClusterRoleBinding", "", "portal-krateo-rbac", oldSA, saNS),
		binding("RoleBinding", "other-system", "tenant-portal-krateo-rbac", oldSA, "other-system"),
	)

	repaired, err := ResubjectDerivedRBAC(context.Background(), ResubjectOptions{
		DynamicClient: dyn,
		Namespace:     saNS,
		NewNamespace:  movedNS,
		OldGVR:        oldPortalGVR,
		NewGVR:        oldPortalGVR,
	})
	if err != nil {
		t.Fatalf("ResubjectDerivedRBAC: %v", err)
	}
	if repaired != 2 {
		t.Errorf("repaired = %d, want 2", repaired)
	}

	subjectNS := func(gvr schema.GroupVersionResource, namespace, name string) string {
		got, err := dyn.Resource(gvr).Namespace(namespace).Get(context.Background(), name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("getting %s %s/%s: %v", gvr.Resource, namespace, name, err)
		}
		subjects, _, _ := unstructured.NestedSlice(got.Object, "subjects")
		return subjects[0].(map[string]any)["namespace"].(string)
	}
	if got := subjectNS(roleBindingGVR, "demo-system", "portal-krateo-rbac"); got != movedNS {
		t.Errorf("derived RoleBinding subject namespace = %q, want %q", got, movedNS)
	}
	if got := subjectNS(clusterRoleBindingGVR, "", "portal-krateo-rbac"); got != movedNS {
		t.Errorf("derived ClusterRoleBinding subject namespace = %q, want %q", got, movedNS)
	}
	if got := subjectNS(roleBindingGVR, "other-system", "tenant-portal-krateo-rbac"); got != "other-system" {
		t.Errorf("another identity's binding was moved to %q", got)
	}
}

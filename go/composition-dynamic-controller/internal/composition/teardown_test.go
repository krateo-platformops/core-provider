package composition

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// teardownDyn builds a fake dynamic client that knows the kinds a builder-publish release manages.
func teardownDyn(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	gvrToListKind := map[schema.GroupVersionResource]string{
		{Group: "", Version: "v1", Resource: "secrets"}:                                        "SecretList",
		{Group: "git.krateo.io", Version: "v1alpha1", Resource: "localresources"}:              "LocalResourceList",
		{Group: "github.krateo.io", Version: "v2022-11-28", Resource: "repositories"}:          "RepositoryList",
		{Group: "github.krateo.io", Version: "v1alpha1", Resource: "repositoryconfigurations"}: "RepositoryConfigurationList",
	}
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), gvrToListKind, objs...)
}

func child(apiVersion, kind, name string, finalizers []string, deleting *metav1.Time) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": apiVersion, "kind": kind}}
	u.SetName(name)
	u.SetNamespace("krateo-system")
	if len(finalizers) > 0 {
		u.SetFinalizers(finalizers)
	}
	if deleting != nil {
		u.SetDeletionTimestamp(deleting)
	}
	return u
}

func managedRef(apiVersion, resource, name string) map[string]any {
	return map[string]any{"apiVersion": apiVersion, "resource": resource, "name": name, "namespace": "krateo-system"}
}

func compositionWithManaged(refs ...map[string]any) *unstructured.Unstructured {
	mg := &unstructured.Unstructured{Object: map[string]any{}}
	items := make([]any, len(refs))
	for i, r := range refs {
		items[i] = r
	}
	_ = unstructured.SetNestedSlice(mg.Object, items, "status", "managed")
	return mg
}

// The krateo-core-provider#108 shape, taken from a live BuilderPublish: two finalizer-bearing
// children that strand, and two finalizer-free children they depend on (the git credentials Secret
// and the RepositoryConfiguration that Repository's delete handler resolves).
//
// The drain must delete ONLY the finalizer-bearing pair. If it touched the Secret or the
// RepositoryConfiguration it would recreate the very inversion it exists to prevent.
func TestDrain_DeletesOnlyFinalizerBearingChildren(t *testing.T) {
	secret := child("v1", "Secret", "publish-pet-git-username", nil, nil)
	repoCfg := child("github.krateo.io/v1alpha1", "RepositoryConfiguration", "publish-pet-repo", nil, nil)
	local := child("git.krateo.io/v1alpha1", "LocalResource", "publish-pet-000", []string{"finalizer.managedresource.krateo.io"}, nil)
	repo := child("github.krateo.io/v2022-11-28", "Repository", "publish-pet-repo", []string{"composition.krateo.io/finalizer"}, nil)

	dyn := teardownDyn(secret, repoCfg, local, repo)
	mg := compositionWithManaged(
		managedRef("v1", "secrets", "publish-pet-git-username"),
		managedRef("git.krateo.io/v1alpha1", "localresources", "publish-pet-000"),
		managedRef("github.krateo.io/v2022-11-28", "repositories", "publish-pet-repo"),
		managedRef("github.krateo.io/v1alpha1", "repositoryconfigurations", "publish-pet-repo"),
	)

	h := &handler{}
	pending := h.drainFinalizerBoundChildren(context.Background(), dyn, mg)

	if len(pending) != 2 {
		t.Fatalf("expected the 2 finalizer-bearing children to be drained, got %d: %s", len(pending), pendingIDs(pending))
	}

	// The dependencies must still exist — that is the whole point of draining first.
	ctx := context.Background()
	if _, err := dyn.Resource(schema.GroupVersionResource{Version: "v1", Resource: "secrets"}).
		Namespace("krateo-system").Get(ctx, "publish-pet-git-username", metav1.GetOptions{}); err != nil {
		t.Errorf("the credentials Secret must survive the drain, but it is gone: %v", err)
	}
	if _, err := dyn.Resource(schema.GroupVersionResource{Group: "github.krateo.io", Version: "v1alpha1", Resource: "repositoryconfigurations"}).
		Namespace("krateo-system").Get(ctx, "publish-pet-repo", metav1.GetOptions{}); err != nil {
		t.Errorf("the RepositoryConfiguration must survive the drain, but it is gone: %v", err)
	}
}

// A child already mid-deletion must be reported with ITS OWN deletionTimestamp, so the grace is
// measured from when deletion actually began and survives a controller restart.
func TestDrain_UsesChildDeletionTimestamp(t *testing.T) {
	began := metav1.NewTime(time.Now().Add(-9 * time.Minute))
	local := child("git.krateo.io/v1alpha1", "LocalResource", "publish-pet-000",
		[]string{"finalizer.managedresource.krateo.io"}, &began)

	dyn := teardownDyn(local)
	mg := compositionWithManaged(managedRef("git.krateo.io/v1alpha1", "localresources", "publish-pet-000"))

	pending := (&handler{}).drainFinalizerBoundChildren(context.Background(), dyn, mg)
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending child, got %d", len(pending))
	}
	if waited := drainedFor(pending, time.Now()); waited < 8*time.Minute {
		t.Errorf("grace must be measured from the child's own deletionTimestamp, got %s", waited)
	}
}

// Fail-safe: nothing to drain must never be an error or a block — the caller falls through to a
// plain uninstall, i.e. exactly today's behaviour.
func TestDrain_FailSafeWhenNothingToDrain(t *testing.T) {
	cases := []struct {
		name string
		mg   *unstructured.Unstructured
		dyn  *dynamicfake.FakeDynamicClient
	}{
		{"no status.managed", &unstructured.Unstructured{Object: map[string]any{}}, teardownDyn()},
		{
			"child already gone (NotFound)",
			compositionWithManaged(managedRef("git.krateo.io/v1alpha1", "localresources", "absent")),
			teardownDyn(),
		},
		{
			"only finalizer-free children",
			compositionWithManaged(managedRef("v1", "secrets", "publish-pet-git-username")),
			teardownDyn(child("v1", "Secret", "publish-pet-git-username", nil, nil)),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if pending := (&handler{}).drainFinalizerBoundChildren(context.Background(), c.dyn, c.mg); len(pending) != 0 {
				t.Errorf("expected nothing pending, got %s", pendingIDs(pending))
			}
		})
	}
}

func TestDrainedFor_TakesTheLongestWaiter(t *testing.T) {
	now := time.Now()
	pending := []pendingChild{
		{id: "a", since: now.Add(-1 * time.Minute), hasSince: true},
		{id: "b", since: now.Add(-7 * time.Minute), hasSince: true},
		{id: "c"}, // no timestamp: ignored, never counted as "waited forever"
	}
	got := drainedFor(pending, now)
	if got < 7*time.Minute || got > 8*time.Minute {
		t.Errorf("drainedFor = %s, want ~7m (the longest waiter)", got)
	}
}

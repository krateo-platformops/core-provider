package composition

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/events"

	"github.com/krateo-platformops/composition-dynamic-controller/internal/tools/archive"
	"github.com/krateo-platformops/composition-dynamic-controller/internal/tools/rbac"
	"github.com/krateo-platformops/plumbing/kubeutil/event"
	"github.com/krateo-platformops/unstructured-runtime/pkg/logging"
	"k8s.io/apimachinery/pkg/types"
)

func hookJobManifest(name, ns string, annotations string) string {
	m := "apiVersion: batch/v1\nkind: Job\nmetadata:\n  name: " + name + "\n"
	if ns != "" {
		m += "  namespace: " + ns + "\n"
	}
	if annotations != "" {
		m += "  annotations:\n" + annotations
	}
	return m
}

// #576: which of a release's recorded hooks are Jobs to delete at uninstall, by input state.
func TestRecordedHookJobs(t *testing.T) {
	rel := &helmconfig.Release{Name: "nova", Namespace: "openstack", Hooks: []helmconfig.Hook{
		{Name: "nova-db-sync", Kind: "Job", Manifest: hookJobManifest("nova-db-sync", "", ""), Events: []string{"post-install", "post-upgrade"}},
		{Name: "nova-ks-user", Kind: "Job", Manifest: hookJobManifest("nova-ks-user", "keystone", ""), Events: []string{"post-install"}},
		{Name: "nova-bootstrap", Kind: "Job", Manifest: hookJobManifest("nova-bootstrap", "", "    helm.sh/resource-policy: keep\n"), Events: []string{"post-install"}},
		{Name: "nova-hook-cm", Kind: "ConfigMap", Manifest: "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: nova-hook-cm\n"},
		{Name: "not-batch", Kind: "Job", Manifest: "apiVersion: example.com/v1\nkind: Job\nmetadata:\n  name: not-batch\n"},
	}}
	got, err := recordedHookJobs(rel)
	if err != nil {
		t.Fatal(err)
	}
	want := []hookJobRef{
		{Namespace: "openstack", Name: "nova-db-sync"}, // no namespace: the release's, as helm applies it
		{Namespace: "keystone", Name: "nova-ks-user"},  // its own manifest's namespace
		// nova-bootstrap: resource-policy keep. nova-hook-cm: not a Job. not-batch: a Kind named Job outside batch.
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("recordedHookJobs = %+v\nwant              %+v", got, want)
	}

	if _, err := recordedHookJobs(&helmconfig.Release{Hooks: []helmconfig.Hook{{Name: "bad", Kind: "Job", Manifest: "metadata: [unclosed"}}}); err == nil {
		t.Error("an unreadable hook manifest was skipped instead of failing: on a teardown it would read as nothing to delete")
	}
	if got, err := recordedHookJobs(nil); got != nil || err != nil {
		t.Errorf("nil release = %v, %v", got, err)
	}
}

// The recorded list survives as an annotation, deduplicated and sorted; empty removes it; a value
// that does not parse is an error, never "nothing pending".
func TestPendingHookJobs_Annotation(t *testing.T) {
	mg := &unstructured.Unstructured{Object: map[string]any{}}
	setPendingHookJobs(mg, []hookJobRef{{"b", "y"}, {"a", "x"}, {"b", "y"}})
	got, err := pendingHookJobs(mg)
	if err != nil || !reflect.DeepEqual(got, []hookJobRef{{"a", "x"}, {"b", "y"}}) {
		t.Fatalf("round trip = %+v, %v", got, err)
	}
	setPendingHookJobs(mg, nil)
	if _, ok := mg.GetAnnotations()[annotationHookJobsToDelete]; ok {
		t.Error("an empty list left the annotation behind")
	}
	mg.SetAnnotations(map[string]string{annotationHookJobsToDelete: "{not json"})
	if _, err := pendingHookJobs(mg); err == nil {
		t.Error("a malformed annotation read as nothing pending")
	}
}

// Deleting: background propagation (the pods go too), an already-gone Job is done, any other
// failure keeps that Job in remaining and is returned.
func TestDeleteHookJobs(t *testing.T) {
	jobsGVRList := map[schema.GroupVersionResource]string{jobsGVR: "JobList"}
	job := func(ns, name string) *unstructured.Unstructured {
		u := &unstructured.Unstructured{}
		u.SetAPIVersion("batch/v1")
		u.SetKind("Job")
		u.SetNamespace(ns)
		u.SetName(name)
		return u
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), jobsGVRList,
		job("openstack", "nova-db-sync"), job("openstack", "locked"))
	var propagation []string
	dyn.PrependReactor("delete", "jobs", func(a k8stesting.Action) (bool, runtime.Object, error) {
		d := a.(k8stesting.DeleteActionImpl)
		if p := d.DeleteOptions.PropagationPolicy; p != nil {
			propagation = append(propagation, string(*p))
		}
		if d.Name == "locked" {
			return true, nil, errors.New("admission webhook denied the request")
		}
		return false, nil, nil
	})

	_, remaining, err := deleteHookJobs(context.Background(), dyn, []hookJobRef{
		{"openstack", "nova-db-sync"}, {"openstack", "already-gone"}, {"openstack", "locked"},
	})
	if err == nil || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("err = %v, want the failed delete named", err)
	}
	if !reflect.DeepEqual(remaining, []hookJobRef{{"openstack", "locked"}}) {
		t.Errorf("remaining = %+v, want only the failed one (already-gone is done)", remaining)
	}
	for _, p := range propagation {
		if p != string(metav1.DeletePropagationBackground) {
			t.Errorf("propagation = %s, want Background so the Job's pods are deleted too", p)
		}
	}
	if len(propagation) != 3 {
		t.Errorf("control: %d deletes sent, want 3", len(propagation))
	}
	if _, gerr := dyn.Resource(jobsGVR).Namespace("openstack").Get(context.Background(), "nova-db-sync", metav1.GetOptions{}); gerr == nil {
		t.Error("nova-db-sync still exists")
	}
}

// finishTeardown, the path a Delete takes once the release is gone, including a RETRY after the
// release is already gone (the rel == nil branch). A hook Job whose delete fails stays recorded on
// the composition and fails the pass BEFORE the derived RBAC is released (deleting it needs those
// grants); the next pass deletes it and then releases the RBAC.
func TestFinishTeardown_AFailedHookJobDeleteIsRecordedAndRetried(t *testing.T) {
	ctx := context.Background()
	uid := "11111111-2222-4333-8444-555555555555"
	roleGVR := schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}

	portal := srPortal("1", "x")
	portal.SetUID(types.UID(uid))
	setPendingHookJobs(portal, []hookJobRef{{"krateo-system", "db-sync"}, {"krateo-system", "locked"}})
	ownedRole := &unstructured.Unstructured{}
	ownedRole.SetAPIVersion("rbac.authorization.k8s.io/v1")
	ownedRole.SetKind("Role")
	ownedRole.SetNamespace("krateo-system")
	ownedRole.SetName("portal-krateo-rbac")
	ownedRole.SetLabels(map[string]string{rbac.OwnerLabelKey(types.UID(uid)): "true"})
	jobObj := func(name string) *unstructured.Unstructured {
		u := &unstructured.Unstructured{}
		u.SetAPIVersion("batch/v1")
		u.SetKind("Job")
		u.SetNamespace("krateo-system")
		u.SetName(name)
		return u
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		srGVR: "PortalList", jobsGVR: "JobList", roleGVR: "RoleList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}:        "RoleBindingList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}:        "ClusterRoleList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}: "ClusterRoleBindingList",
	}, portal.DeepCopy(), ownedRole, jobObj("db-sync"), jobObj("locked"))
	locked := true
	dyn.PrependReactor("delete", "jobs", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.(k8stesting.DeleteActionImpl).Name == "locked" && locked {
			return true, nil, errors.New("admission webhook denied the request")
		}
		return false, nil, nil
	})
	h := &handler{eventRecorder: *event.NewAPIRecorder(events.NewFakeRecorder(10))}
	pkg := &archive.Info{URL: "oci://example/portal"}
	stored := func() *unstructured.Unstructured {
		u, err := dyn.Resource(srGVR).Namespace("krateo-system").Get(ctx, "portal", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	roleExists := func() bool {
		_, err := dyn.Resource(roleGVR).Namespace("krateo-system").Get(ctx, "portal-krateo-rbac", metav1.GetOptions{})
		return err == nil
	}

	// Pass 1: one delete fails.
	err := h.finishTeardown(ctx, stored(), dyn, pkg, srOpts(dyn), logging.NewNopLogger())
	if err == nil || !strings.Contains(err.Error(), "1 of 2 remain") {
		t.Fatalf("pass 1 err = %v, want the failed hook Job reported", err)
	}
	if got, _ := pendingHookJobs(stored()); !reflect.DeepEqual(got, []hookJobRef{{"krateo-system", "locked"}}) {
		t.Fatalf("after pass 1 the composition records %+v, want only the failed Job", got)
	}
	if !roleExists() {
		t.Fatal("the derived RBAC was released while a hook Job still needed its grant")
	}

	// Pass 2 (the retry, release already gone): the delete now succeeds; the list clears and the
	// RBAC is released.
	locked = false
	if err := h.finishTeardown(ctx, stored(), dyn, pkg, srOpts(dyn), logging.NewNopLogger()); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if _, ok := stored().GetAnnotations()[annotationHookJobsToDelete]; ok {
		t.Error("the annotation survived a pass that deleted everything")
	}
	if roleExists() {
		t.Error("pass 2 did not release the derived RBAC")
	}
}

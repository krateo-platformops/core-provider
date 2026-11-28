package deploy

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

func roleObject(ns, name, label string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "Role",
		"metadata": map[string]any{"namespace": ns, "name": name, "labels": map[string]any{ApiRefRBACLabel: label}}}}
	return u
}

// A LIST that fails on teardown is an error and deletes nothing: it must never read as "there are
// none" (the opposite mistake, deleting from an answer that is missing, is what this guards against).
func TestDeleteApiRefRoles_AFailedListIsAnErrorAndDeletesNothing(t *testing.T) {
	live := roleObject("team-a", "n", "n")
	dyn := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		roleGVR: "RoleList", roleBindingGVR: "RoleBindingList"}, live)
	dyn.PrependReactor("list", "roles", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("apiserver unavailable")
	})

	if err := DeleteApiRefRoles(context.Background(), dyn, "n"); err == nil {
		t.Fatal("a failed LIST must be an error")
	}
	if _, err := dyn.Resource(roleGVR).Namespace("team-a").Get(context.Background(), "n", metav1.GetOptions{}); err != nil {
		t.Fatalf("the Role was deleted although the LIST failed: %v", err)
	}
}

// CONTROL for the test above: with a working LIST the same call does delete what carries the label,
// and only that.
func TestDeleteApiRefRoles_DeletesByLabelOnly(t *testing.T) {
	mine, other := roleObject("team-a", "n", "n"), roleObject("team-b", "n", "another")
	dyn := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		roleGVR: "RoleList", roleBindingGVR: "RoleBindingList"}, mine, other)
	if err := DeleteApiRefRoles(context.Background(), dyn, "n"); err != nil {
		t.Fatal(err)
	}
	if _, err := dyn.Resource(roleGVR).Namespace("team-a").Get(context.Background(), "n", metav1.GetOptions{}); err == nil {
		t.Error("the labelled Role must be deleted")
	}
	if _, err := dyn.Resource(roleGVR).Namespace("team-b").Get(context.Background(), "n", metav1.GetOptions{}); err != nil {
		t.Errorf("another definition's Role must stay: %v", err)
	}
}

package deploy

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	keepSharedOldNS = "krateo-system"
	keepSharedName  = "fireworksapps-v1-0-0"
)

func keepSharedObj(apiVersion, kind, namespace, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(apiVersion)
	u.SetKind(kind)
	u.SetNamespace(namespace)
	u.SetName(name)
	return u
}

// The controller of one Kind+version as Deploy leaves it: namespaced objects in the definition's
// namespace, plus the cluster-scoped RBAC whose name carries no namespace.
func keepSharedCluster() *fake.FakeDynamicClient {
	return fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{guardGVR: "FireworksAppList"},
		keepSharedObj("rbac.authorization.k8s.io/v1", "ClusterRole", "", keepSharedName),
		keepSharedObj("rbac.authorization.k8s.io/v1", "ClusterRoleBinding", "", keepSharedName),
		keepSharedObj("v1", "ServiceAccount", keepSharedOldNS, keepSharedName),
		keepSharedObj("apps/v1", "Deployment", keepSharedOldNS, keepSharedName+"-controller"),
		keepSharedObj("v1", "ConfigMap", keepSharedOldNS, keepSharedName+"-configmap"),
	)
}

func keepSharedUndeploy(t *testing.T, dyn *fake.FakeDynamicClient, keepShared bool) {
	t.Helper()
	err := Undeploy(context.Background(), nil, UndeployOptions{
		KubeClient:             clientfake.NewClientBuilder().Build(),
		DynamicClient:          dyn,
		GVR:                    guardGVR,
		Namespace:              keepSharedOldNS,
		SkipCRD:                true,
		KeepShared:             keepShared,
		RBACFolderPath:         "testdata",
		DeploymentTemplatePath: "testdata/deploy.yaml",
		ConfigmapTemplatePath:  "testdata/cm.yaml",
		JsonSchemaTemplatePath: "testdata/configmap_jsonschema.yaml",
		ServiceTemplatePath:    "testdata/service.yaml",
	})
	if err != nil {
		t.Fatalf("Undeploy(KeepShared=%v): %v", keepShared, err)
	}
}

func keepSharedExists(t *testing.T, dyn *fake.FakeDynamicClient, gvr schema.GroupVersionResource, namespace, name string) bool {
	t.Helper()
	_, err := dyn.Resource(gvr).Namespace(namespace).Get(context.Background(), name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false
	}
	if err != nil {
		t.Fatalf("get %s %s/%s: %v", gvr.Resource, namespace, name, err)
	}
	return true
}

// Handing a Kind+version's controller to a definition in another namespace removes only what
// lives in the departing definition's namespace. The ClusterRole/ClusterRoleBinding are named
// {resource}-{apiVersion} for every definition on that version, so deleting them would strip the
// successor's controller of its permissions.
func TestUndeploy_KeepSharedLeavesClusterScopedRBAC(t *testing.T) {
	dyn := keepSharedCluster()
	keepSharedUndeploy(t, dyn, true)

	if !keepSharedExists(t, dyn, clusterRoleGVR, "", keepSharedName) {
		t.Error("ClusterRole shared with the successor was deleted")
	}
	if !keepSharedExists(t, dyn, clusterRoleBindingGVR, "", keepSharedName) {
		t.Error("ClusterRoleBinding shared with the successor was deleted")
	}
	if keepSharedExists(t, dyn, deploymentGVR, keepSharedOldNS, keepSharedName+"-controller") {
		t.Error("the departing namespace's controller Deployment was left running")
	}
	if keepSharedExists(t, dyn, serviceAccountGVR, keepSharedOldNS, keepSharedName) {
		t.Error("the departing namespace's ServiceAccount was left behind")
	}
}

// Without KeepShared (the last definition on the version) everything goes, as before.
func TestUndeploy_WithoutKeepSharedRemovesClusterScopedRBAC(t *testing.T) {
	dyn := keepSharedCluster()
	keepSharedUndeploy(t, dyn, false)

	if keepSharedExists(t, dyn, clusterRoleGVR, "", keepSharedName) {
		t.Error("ClusterRole survived the last definition's teardown")
	}
	if keepSharedExists(t, dyn, clusterRoleBindingGVR, "", keepSharedName) {
		t.Error("ClusterRoleBinding survived the last definition's teardown")
	}
}

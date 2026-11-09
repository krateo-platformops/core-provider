package deploy

import (
	"context"
	"testing"

	"k8s.io/client-go/dynamic/fake"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// #313. Relocating removes the departing PLACEMENT of a controller that is moving to another
// namespace, and nothing the new placement shares: the namespaced grant Role/RoleBinding in the
// definition's namespace has the same name in both placements.
func relocatingCluster() *fake.FakeDynamicClient {
	dyn := keepSharedCluster()
	for _, o := range []struct{ apiVersion, kind string }{
		{"rbac.authorization.k8s.io/v1", "Role"},
		{"rbac.authorization.k8s.io/v1", "RoleBinding"},
	} {
		if err := dyn.Tracker().Add(keepSharedObj(o.apiVersion, o.kind, keepSharedOldNS, keepSharedName)); err != nil {
			panic(err)
		}
	}
	return dyn
}

func relocatingUndeploy(t *testing.T, dyn *fake.FakeDynamicClient, opts UndeployOptions) {
	t.Helper()
	opts.KubeClient = clientfake.NewClientBuilder().Build()
	opts.DynamicClient = dyn
	opts.GVR = guardGVR
	opts.Namespace = keepSharedOldNS
	opts.ControllerNamespace = keepSharedOldNS
	opts.SkipCRD = true
	opts.RBACFolderPath = "testdata"
	opts.DeploymentTemplatePath = "testdata/deploy.yaml"
	opts.ConfigmapTemplatePath = "testdata/cm.yaml"
	opts.JsonSchemaTemplatePath = "testdata/configmap_jsonschema.yaml"
	opts.ServiceTemplatePath = "testdata/service.yaml"
	if err := Undeploy(context.Background(), nil, opts); err != nil {
		t.Fatalf("Undeploy(%+v): %v", opts, err)
	}
}

func TestUndeploy_RelocatingKeepsTheSharedGrantAndRemovesThePlacement(t *testing.T) {
	dyn := relocatingCluster()
	relocatingUndeploy(t, dyn, UndeployOptions{Relocating: true})

	for _, kept := range []struct {
		what, ns string
		exists   bool
	}{
		{"grant Role", keepSharedOldNS, keepSharedExists(t, dyn, roleGVR, keepSharedOldNS, keepSharedName)},
		{"grant RoleBinding", keepSharedOldNS, keepSharedExists(t, dyn, roleBindingGVR, keepSharedOldNS, keepSharedName)},
		{"ClusterRole", "", keepSharedExists(t, dyn, clusterRoleGVR, "", keepSharedName)},
		{"ClusterRoleBinding", "", keepSharedExists(t, dyn, clusterRoleBindingGVR, "", keepSharedName)},
	} {
		if !kept.exists {
			t.Errorf("%s was deleted; the new placement shares it", kept.what)
		}
	}
	for _, gone := range []struct {
		what   string
		exists bool
	}{
		{"Deployment", keepSharedExists(t, dyn, deploymentGVR, keepSharedOldNS, keepSharedName+"-controller")},
		{"ServiceAccount", keepSharedExists(t, dyn, serviceAccountGVR, keepSharedOldNS, keepSharedName)},
	} {
		if gone.exists {
			t.Errorf("the departing placement's %s was left behind", gone.what)
		}
	}
}

// The control: KeepShared alone (a definition handing a version to a sibling) DOES delete the
// namespaced grant pair. This is what makes the Relocating assertion above non-vacuous.
func TestUndeploy_KeepSharedAloneDeletesTheGrant(t *testing.T) {
	dyn := relocatingCluster()
	relocatingUndeploy(t, dyn, UndeployOptions{KeepShared: true})

	if keepSharedExists(t, dyn, roleGVR, keepSharedOldNS, keepSharedName) ||
		keepSharedExists(t, dyn, roleBindingGVR, keepSharedOldNS, keepSharedName) {
		t.Error("KeepShared alone kept the grant pair, so the Relocating test above proves nothing")
	}
}

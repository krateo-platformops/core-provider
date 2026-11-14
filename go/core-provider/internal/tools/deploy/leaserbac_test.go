package deploy

import (
	"context"
	"slices"
	"testing"

	kubecli "github.com/krateo-platformops/core-provider/internal/tools/kube"
	"github.com/krateo-platformops/core-provider/internal/tools/objects"
	"github.com/krateo-platformops/core-provider/internal/tools/objects/templates"
	hasher "github.com/krateo-platformops/plumbing/kubeutil/hasher"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func leaseFake() *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{roleGVR: "RoleList", roleBindingGVR: "RoleBindingList"})
}

// cdc-pooling step 2: the controller's lease grant is applied, read back to the SAME digest, read as
// drift once a piece of it is gone, and removed by uninstall.
func TestLeaseRBAC_ApplyLookupUninstall(t *testing.T) {
	ctx := context.Background()
	gvr := schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1-0-0", Resource: "fireworksapps"}
	nn := getCDCrbacNN(types.NamespacedName{Namespace: "krateo-system", Name: resourceNamer(gvr.Resource, gvr.Version)})
	dyn := leaseFake()

	role, binding, err := renderLeaseRBAC(gvr, nn, "testdata", "fireworksapps-v1-0-0", "krateo-system")
	if err != nil {
		t.Fatal(err)
	}
	deployed := hasher.NewFNVObjectHash()
	if err := installLeaseRBAC(ctx, dyn, role, binding, &deployed, kubecli.ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	live := func() string {
		h := hasher.NewFNVObjectHash()
		if err := lookupLeaseRBAC(ctx, dyn, role, binding, &h); err != nil {
			t.Fatal(err)
		}
		return h.GetHash()
	}
	if got, want := live(), deployed.GetHash(); got != want {
		t.Fatalf("Lookup digest %s != Deploy digest %s: every reconcile would re-deploy", got, want)
	}

	if err := dyn.Resource(roleBindingGVR).Namespace(binding.Namespace).Delete(ctx, binding.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if live() == deployed.GetHash() {
		t.Fatal("a deleted lease RoleBinding must read as drift, so the next Deploy restores it")
	}

	if err := uninstallLeaseRBAC(ctx, dyn, role, binding); err != nil {
		t.Fatalf("uninstall (binding already gone must be tolerated): %v", err)
	}
	if _, err := dyn.Resource(roleGVR).Namespace(role.Namespace).Get(ctx, role.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the lease Role survived uninstall: %v", err)
	}
}

// The grant and the controller must name the SAME Lease, in the namespace the controller runs in, or
// the controller can never take it (it then reports not-ready and reconciles nothing). Rendered from
// the SHIPPED chart through both stages, so a change to either side alone fails here: the Role
// (assets/cdc/rbac/lease-role.yaml), the ConfigMap key the controller reads
// (COMPOSITION_CONTROLLER_LEASE_NAME, assets/cdc/configmap.yaml), or the downward-API env the
// controller refuses to start without (POD_NAMESPACE, POD_NAME, assets/cdc/deployment.yaml).
func TestShippedLeaseGrant_NamesTheControllersLease(t *testing.T) {
	manifests := renderShippedChart(t, nil)
	dep, cm := renderControllerObjects(t, nil)
	gvr := schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v0-13-23", Resource: "kubernetesclusters"}
	nn := getCDCrbacNN(types.NamespacedName{Namespace: "krateo-system", Name: resourceNamer(gvr.Resource, gvr.Version)})

	lease := cm.Data["COMPOSITION_CONTROLLER_LEASE_NAME"]
	if want := templates.ControllerLeaseName(gvr.Group, gvr.Version, gvr.Resource); lease != want {
		t.Fatalf("COMPOSITION_CONTROLLER_LEASE_NAME = %q, want %q", lease, want)
	}

	c := controllerContainer(t, dep)
	for _, name := range []string{"POD_NAMESPACE", "POD_NAME"} {
		i := slices.IndexFunc(c.Env, func(e corev1.EnvVar) bool { return e.Name == name })
		if i < 0 || c.Env[i].ValueFrom == nil || c.Env[i].ValueFrom.FieldRef == nil {
			t.Fatalf("the controller container carries no downward-API %s; with a lease name and no %s the controller refuses to start", name, name)
		}
	}

	var role rbacv1.Role
	if err := objects.CreateK8sObject(&role, gvr, nn, mountedTemplate(t, manifests, "cdc-rbac.yaml", "lease-role.yaml")); err != nil {
		t.Fatalf("stage 2, lease Role: %v", err)
	}
	var binding rbacv1.RoleBinding
	if err := objects.CreateK8sObject(&binding, gvr, nn, mountedTemplate(t, manifests, "cdc-rbac.yaml", "lease-rolebinding.yaml"),
		"serviceAccount", dep.Spec.Template.Spec.ServiceAccountName, "saNamespace", dep.Namespace); err != nil {
		t.Fatalf("stage 2, lease RoleBinding: %v", err)
	}

	if role.Namespace != dep.Namespace || role.Namespace != cm.Namespace {
		t.Fatalf("lease Role in %q, controller Deployment in %q, ConfigMap in %q: the controller takes the Lease in its OWN namespace", role.Namespace, dep.Namespace, cm.Namespace)
	}
	var canCreate, canGetUpdate bool
	for _, r := range role.Rules {
		if !slices.Equal(r.APIGroups, []string{"coordination.k8s.io"}) || !slices.Equal(r.Resources, []string{"leases"}) {
			t.Fatalf("lease Role grants more than leases: %+v", r)
		}
		switch {
		case slices.Equal(r.Verbs, []string{"create"}) && len(r.ResourceNames) == 0:
			canCreate = true
		case slices.Contains(r.Verbs, "get") && slices.Contains(r.Verbs, "update") && len(r.Verbs) == 2:
			if !slices.Equal(r.ResourceNames, []string{lease}) {
				t.Fatalf("lease Role get/update is limited to %v, but the controller takes %q", r.ResourceNames, lease)
			}
			canGetUpdate = true
		default:
			t.Fatalf("unexpected lease Role rule %+v", r)
		}
	}
	if !canCreate || !canGetUpdate {
		t.Fatalf("lease Role must grant create, and get+update on %q: %+v", lease, role.Rules)
	}
	if binding.RoleRef.Kind != "Role" || binding.RoleRef.Name != role.Name {
		t.Fatalf("lease RoleBinding refers to %s %q, want Role %q", binding.RoleRef.Kind, binding.RoleRef.Name, role.Name)
	}
	if len(binding.Subjects) != 1 || binding.Subjects[0].Name != dep.Spec.Template.Spec.ServiceAccountName || binding.Subjects[0].Namespace != dep.Namespace {
		t.Fatalf("lease RoleBinding subjects %+v, want the controller's ServiceAccount %s/%s", binding.Subjects, dep.Namespace, dep.Spec.Template.Spec.ServiceAccountName)
	}
}

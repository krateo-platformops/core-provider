package deploy

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/krateo-platformops/core-provider/internal/tools/auditrec"
	kubecli "github.com/krateo-platformops/core-provider/internal/tools/kube"
	"github.com/krateo-platformops/core-provider/internal/tools/objects"
	hasher "github.com/krateo-platformops/plumbing/kubeutil/hasher"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

// The controller's ownership Lease grant (docs/design/cdc-pooling.md, step 2). A composition
// controller reconciles only while it holds the Lease templates.ControllerLeaseName names, in its own
// namespace, so a second controller for the same Kind x version waits instead of reconciling
// alongside. These two objects let its ServiceAccount take that Lease: a Role in the controller's
// namespace (create, and get/update on that one name) and its binding.
//
// They live where the ServiceAccount and the Deployment live (rbacNN, the controller namespace) and
// share their lifecycle: applied by Deploy, read back by Lookup, deleted by Undeploy once the
// controller has drained.
const (
	leaseRoleTemplate        = "lease-role.yaml"
	leaseRoleBindingTemplate = "lease-rolebinding.yaml"
)

func renderLeaseRBAC(gvr schema.GroupVersionResource, rbacNN types.NamespacedName, rbacFolderPath, saName, saNamespace string) (rbacv1.Role, rbacv1.RoleBinding, error) {
	role := rbacv1.Role{}
	if err := objects.CreateK8sObject(&role, gvr, rbacNN, filepath.Join(rbacFolderPath, leaseRoleTemplate)); err != nil {
		return rbacv1.Role{}, rbacv1.RoleBinding{}, fmt.Errorf("rendering the controller's lease Role: %w", err)
	}
	binding := rbacv1.RoleBinding{}
	if err := objects.CreateK8sObject(&binding, gvr, rbacNN, filepath.Join(rbacFolderPath, leaseRoleBindingTemplate),
		bindingValues(saName, saNamespace)...); err != nil {
		return rbacv1.Role{}, rbacv1.RoleBinding{}, fmt.Errorf("rendering the controller's lease RoleBinding: %w", err)
	}
	return role, binding, nil
}

func hashLeaseRBAC(hsh *hasher.ObjectHash, role rbacv1.Role, binding rbacv1.RoleBinding) error {
	if err := hsh.SumHash(role.Name, role.Namespace, role.Rules); err != nil {
		return fmt.Errorf("error hashing the lease role: %v", err)
	}
	if err := hsh.SumHash(binding.Name, binding.Namespace, binding.Subjects, binding.RoleRef); err != nil {
		return fmt.Errorf("error hashing the lease rolebinding: %v", err)
	}
	return nil
}

func installLeaseRBAC(ctx context.Context, dyn dynamic.Interface, role rbacv1.Role, binding rbacv1.RoleBinding, hsh *hasher.ObjectHash, applyOpts kubecli.ApplyOptions) error {
	if _, err := applyRecorded(ctx, dyn, roleGVR, "Role", auditrec.ActionRBACApply, &role, applyOpts); err != nil {
		return fmt.Errorf("installing the controller's lease Role %s/%s: %w", role.Namespace, role.Name, err)
	}
	if _, err := applyRecorded(ctx, dyn, roleBindingGVR, "RoleBinding", auditrec.ActionRBACApply, &binding, applyOpts); err != nil {
		return fmt.Errorf("installing the controller's lease RoleBinding %s/%s: %w", binding.Namespace, binding.Name, err)
	}
	return hashLeaseRBAC(hsh, role, binding)
}

// lookupLeaseRBAC hashes the LIVE objects the way installLeaseRBAC hashes the rendered ones, so a
// missing or edited grant reads as drift and the next Deploy restores it. A missing object hashes
// as empty, as lookupRBACResources does; any other read error is returned, never read as absent.
func lookupLeaseRBAC(ctx context.Context, dyn dynamic.Interface, role rbacv1.Role, binding rbacv1.RoleBinding, hsh *hasher.ObjectHash) error {
	if err := kubecli.Get(ctx, dyn, roleGVR, &role); err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("error getting the lease role: %w", err)
		}
		role = rbacv1.Role{}
	}
	if err := kubecli.Get(ctx, dyn, roleBindingGVR, &binding); err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("error getting the lease rolebinding: %w", err)
		}
		binding = rbacv1.RoleBinding{}
	}
	return hashLeaseRBAC(hsh, role, binding)
}

func uninstallLeaseRBAC(ctx context.Context, dyn dynamic.Interface, role rbacv1.Role, binding rbacv1.RoleBinding) error {
	if _, err := uninstallRecorded(ctx, dyn, roleBindingGVR, "RoleBinding", auditrec.ActionRBACDelete, &binding, kubecli.UninstallOptions{}); err != nil {
		return fmt.Errorf("uninstalling the controller's lease RoleBinding %s/%s: %w", binding.Namespace, binding.Name, err)
	}
	if _, err := uninstallRecorded(ctx, dyn, roleGVR, "Role", auditrec.ActionRBACDelete, &role, kubecli.UninstallOptions{}); err != nil {
		return fmt.Errorf("uninstalling the controller's lease Role %s/%s: %w", role.Namespace, role.Name, err)
	}
	return nil
}

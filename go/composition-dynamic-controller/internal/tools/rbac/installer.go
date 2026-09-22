package rbac

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/errors"

	"reflect"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

type RBACInstaller struct {
	DynamicClient dynamic.Interface
}

func NewRBACInstaller(cli dynamic.Interface) *RBACInstaller {
	return &RBACInstaller{DynamicClient: cli}
}

func (i *RBACInstaller) ApplyRBAC(rbac *RBAC) error {
	if rbac == nil {
		return fmt.Errorf("RBAC cannot be nil")
	}

	if rbac.Namespaces != nil {
		for _, ns := range rbac.Namespaces {
			if ns != nil {
				_, err := i.ApplyNamespace(context.Background(), ns)
				if err != nil {
					return fmt.Errorf("failed to Apply Namespace %s: %w", ns.Name, err)
				}
			}
		}
	}

	if rbac.ClusterRole != nil {
		_, err := i.ApplyClusterRole(context.Background(), rbac.ClusterRole)
		if err != nil {
			return fmt.Errorf("failed to Apply ClusterRole: %w - name: %s", err, rbac.ClusterRole.Name)
		}
	}
	if rbac.ClusterRoleBinding != nil {
		_, err := i.ApplyClusterRoleBinding(context.Background(), rbac.ClusterRoleBinding)
		if err != nil {
			return fmt.Errorf("failed to Apply ClusterRoleBinding: %w - name: %s", err, rbac.ClusterRoleBinding.Name)
		}
	}
	for _, ns := range rbac.Namespaced {
		if ns.Role != nil {
			_, err := i.ApplyRole(context.Background(), ns.Role)
			if err != nil {
				return fmt.Errorf("failed to Apply Role: %w - name: %s", err, ns.Role.Name)
			}
		}
		if ns.RoleBinding != nil {
			_, err := i.ApplyRoleBinding(context.Background(), ns.RoleBinding)
			if err != nil {
				return fmt.Errorf("failed to Apply RoleBinding: %w - name: %s", err, ns.RoleBinding.Name)
			}
		}
	}
	return nil

}

func (r *RBACInstaller) ApplyNamespace(ctx context.Context, namespace *corev1.Namespace) (*corev1.Namespace, error) {
	cli := r.DynamicClient.Resource(
		schema.GroupVersionResource{
			Group:    "",
			Version:  "v1",
			Resource: "namespaces",
		},
	)

	existingU, err := cli.Get(ctx, namespace.Name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(namespace)
		if err != nil {
			return nil, fmt.Errorf("failed to convert Namespace to unstructured: %w", err)
		}
		res, err := cli.Create(ctx, &unstructured.Unstructured{Object: m}, metav1.CreateOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to Create Namespace: %w", err)
		}
		namespace = &corev1.Namespace{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(res.Object, namespace); err != nil {
			return nil, fmt.Errorf("failed to convert unstructured to Namespace: %w", err)
		}
		return namespace, nil
	} else if err != nil {
		return nil, fmt.Errorf("failed to Get Namespace: %w", err)
	}

	// The Namespace already exists. It may be owned by the composition's Helm
	// release (e.g. the portal's demo-system, declared as a chart child), in
	// which case its metadata carries foreign labels such as the postrenderer's
	// krateo.io/composition-* set. A blind full-PUT of our freshly-built object
	// would replace metadata.labels and strip those, so helm re-adds them on the
	// next reconcile — an endless label tug-of-war that bumps a helm revision
	// every cycle. Instead merge our labels/annotations INTO the existing object
	// (like ApplyRole does for rules) and only Update when something is missing.
	existing := &corev1.Namespace{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(existingU.Object, existing); err != nil {
		return nil, fmt.Errorf("failed to convert unstructured to Namespace: %w", err)
	}

	modified := false
	if len(namespace.Labels) > 0 && existing.Labels == nil {
		existing.Labels = map[string]string{}
	}
	for k, v := range namespace.Labels {
		if existing.Labels[k] != v {
			existing.Labels[k] = v
			modified = true
		}
	}
	if len(namespace.Annotations) > 0 && existing.Annotations == nil {
		existing.Annotations = map[string]string{}
	}
	for k, v := range namespace.Annotations {
		if existing.Annotations[k] != v {
			existing.Annotations[k] = v
			modified = true
		}
	}

	if !modified {
		return existing, nil
	}

	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(existing)
	if err != nil {
		return nil, fmt.Errorf("failed to convert Namespace to unstructured: %w", err)
	}
	res, err := cli.Update(ctx, &unstructured.Unstructured{Object: m}, metav1.UpdateOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to Update Namespace: %w", err)
	}
	namespace = &corev1.Namespace{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(res.Object, namespace); err != nil {
		return nil, fmt.Errorf("failed to convert unstructured to Namespace: %w", err)
	}
	return namespace, nil
}

// helmReleaseNameAnnotation is the marker Helm stamps on every object it owns.
const helmReleaseNameAnnotation = "meta.helm.sh/release-name"

// errIfHelmOwned refuses to modify RBAC that belongs to a Helm release.
//
// Generated RBAC used to be named after the release, which is exactly what a chart's conventional
// `{{ include "chart.fullname" . }}` produces — so a chart shipping its own Role/RoleBinding wrote
// to the same object we did, and Helm won (core-provider#130). The names no longer collide (see
// rbacgen.rbacNameSuffix), so reaching this should now be impossible.
//
// It is still worth failing on, because the ORIGINAL defect was invisible precisely here: unioning
// rules into a chart-owned object succeeds, Helm reverts it on that release's next apply, and
// neither side logs anything. The composition then fails on a permission it was granted moments
// earlier, which reads as "generated RBAC is incomplete" rather than "generated RBAC was
// overwritten". An error naming the release turns a silent revert into a diagnosis.
func errIfHelmOwned(u *unstructured.Unstructured, kind string) error {
	rel := u.GetAnnotations()[helmReleaseNameAnnotation]
	if rel == "" {
		return nil
	}
	name := u.GetName()
	if ns := u.GetNamespace(); ns != "" {
		name = ns + "/" + name
	}
	return fmt.Errorf("refusing to modify %s %s: it is owned by helm release %q, so anything written "+
		"here would be silently reverted by that release's next apply", kind, name, rel)
}

func (i *RBACInstaller) ApplyRole(ctx context.Context, role *rbacv1.Role) (*rbacv1.Role, error) {
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(role)
	if err != nil {
		return nil, fmt.Errorf("failed to convert Role to unstructured: %w", err)
	}
	u := &unstructured.Unstructured{Object: m}

	cli := i.DynamicClient.Resource(
		schema.GroupVersionResource{
			Group:    "rbac.authorization.k8s.io",
			Version:  "v1",
			Resource: "roles",
		},
	).Namespace(role.Namespace)

	existingU, err := cli.Get(ctx, role.Name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		res, err := cli.Create(ctx, u, metav1.CreateOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to Create Role: %w", err)
		}

		role = &rbacv1.Role{}
		err = runtime.DefaultUnstructuredConverter.FromUnstructured(res.Object, role)
		if err != nil {
			return nil, fmt.Errorf("failed to convert unstructured to Role: %w", err)
		}

		return role, nil
	} else if err != nil {
		return nil, fmt.Errorf("failed to Get Role: %w", err)
	}

	if err := errIfHelmOwned(existingU, "Role"); err != nil {
		return nil, err
	}

	existingRole := &rbacv1.Role{}
	err = runtime.DefaultUnstructuredConverter.FromUnstructured(existingU.Object, existingRole)
	if err != nil {
		return nil, fmt.Errorf("failed to convert unstructured to Role: %w", err)
	}

	modified := false
	for _, rule := range role.Rules {
		if !ruleExists(existingRole.Rules, rule) {
			existingRole.Rules = append(existingRole.Rules, rule)
			modified = true
		}
	}

	if !modified {
		return existingRole, nil
	}

	m, err = runtime.DefaultUnstructuredConverter.ToUnstructured(existingRole)
	if err != nil {
		return nil, fmt.Errorf("failed to convert Role to unstructured: %w", err)
	}
	u.Object = m

	res, err := cli.Update(ctx, u, metav1.UpdateOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to Update Role: %w", err)
	}

	role = &rbacv1.Role{}
	err = runtime.DefaultUnstructuredConverter.FromUnstructured(res.Object, role)
	if err != nil {
		return nil, fmt.Errorf("failed to convert unstructured to Role: %w", err)
	}

	return role, nil
}

func (i *RBACInstaller) ApplyRoleBinding(ctx context.Context, roleBinding *rbacv1.RoleBinding) (*rbacv1.RoleBinding, error) {
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(roleBinding)
	if err != nil {
		return nil, fmt.Errorf("failed to convert RoleBinding to unstructured: %w", err)
	}
	u := &unstructured.Unstructured{Object: m}

	cli := i.DynamicClient.Resource(
		schema.GroupVersionResource{
			Group:    "rbac.authorization.k8s.io",
			Version:  "v1",
			Resource: "rolebindings",
		},
	).Namespace(roleBinding.Namespace)

	existingU, err := cli.Get(ctx, roleBinding.Name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		res, err := cli.Create(ctx, u, metav1.CreateOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to Create RoleBinding: %w", err)
		}

		roleBinding = &rbacv1.RoleBinding{}
		err = runtime.DefaultUnstructuredConverter.FromUnstructured(res.Object, roleBinding)
		if err != nil {
			return nil, fmt.Errorf("failed to convert unstructured to RoleBinding: %w", err)
		}

		return roleBinding, nil
	} else if err != nil {
		return nil, fmt.Errorf("failed to Get RoleBinding: %w", err)
	}

	if err := errIfHelmOwned(existingU, "RoleBinding"); err != nil {
		return nil, err
	}

	res, err := cli.Update(ctx, u, metav1.UpdateOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to Update RoleBinding: %w", err)
	}

	roleBinding = &rbacv1.RoleBinding{}
	err = runtime.DefaultUnstructuredConverter.FromUnstructured(res.Object, roleBinding)
	if err != nil {
		return nil, fmt.Errorf("failed to convert unstructured to RoleBinding: %w", err)
	}

	return roleBinding, nil
}

func (i *RBACInstaller) ApplyClusterRole(ctx context.Context, clusterRole *rbacv1.ClusterRole) (*rbacv1.ClusterRole, error) {
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(clusterRole)
	if err != nil {
		return nil, fmt.Errorf("failed to convert ClusterRole to unstructured: %w", err)
	}
	u := &unstructured.Unstructured{Object: m}

	cli := i.DynamicClient.Resource(
		schema.GroupVersionResource{
			Group:    "rbac.authorization.k8s.io",
			Version:  "v1",
			Resource: "clusterroles",
		},
	)

	existingU, err := cli.Get(ctx, clusterRole.Name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		res, err := cli.Create(ctx, u, metav1.CreateOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to Create ClusterRole: %w", err)
		}

		clusterRole = &rbacv1.ClusterRole{}
		err = runtime.DefaultUnstructuredConverter.FromUnstructured(res.Object, clusterRole)
		if err != nil {
			return nil, fmt.Errorf("failed to convert unstructured to ClusterRole: %w", err)
		}

		return clusterRole, nil
	} else if err != nil {
		return nil, fmt.Errorf("failed to Get ClusterRole: %w", err)
	}

	if err := errIfHelmOwned(existingU, "ClusterRole"); err != nil {
		return nil, err
	}

	existingClusterRole := &rbacv1.ClusterRole{}
	err = runtime.DefaultUnstructuredConverter.FromUnstructured(existingU.Object, existingClusterRole)
	if err != nil {
		return nil, fmt.Errorf("failed to convert unstructured to ClusterRole: %w", err)
	}

	modified := false
	for _, rule := range clusterRole.Rules {
		if !ruleExists(existingClusterRole.Rules, rule) {
			existingClusterRole.Rules = append(existingClusterRole.Rules, rule)
			modified = true
		}
	}

	if !modified {
		return existingClusterRole, nil
	}

	m, err = runtime.DefaultUnstructuredConverter.ToUnstructured(existingClusterRole)
	if err != nil {
		return nil, fmt.Errorf("failed to convert ClusterRole to unstructured: %w", err)
	}
	u.Object = m

	res, err := cli.Update(ctx, u, metav1.UpdateOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to Update ClusterRole: %w", err)
	}

	clusterRole = &rbacv1.ClusterRole{}
	err = runtime.DefaultUnstructuredConverter.FromUnstructured(res.Object, clusterRole)
	if err != nil {
		return nil, fmt.Errorf("failed to convert unstructured to ClusterRole: %w", err)
	}
	return clusterRole, nil
}

func (i *RBACInstaller) ApplyClusterRoleBinding(ctx context.Context, clusterRoleBinding *rbacv1.ClusterRoleBinding) (*rbacv1.ClusterRoleBinding, error) {
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(clusterRoleBinding)
	if err != nil {
		return nil, fmt.Errorf("failed to convert ClusterRoleBinding to unstructured: %w", err)
	}
	u := &unstructured.Unstructured{Object: m}

	cli := i.DynamicClient.Resource(
		schema.GroupVersionResource{
			Group:    "rbac.authorization.k8s.io",
			Version:  "v1",
			Resource: "clusterrolebindings",
		},
	)

	existingU, err := cli.Get(ctx, clusterRoleBinding.Name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		res, err := cli.Create(ctx, u, metav1.CreateOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to Create ClusterRoleBinding: %w", err)
		}

		clusterRoleBinding = &rbacv1.ClusterRoleBinding{}
		err = runtime.DefaultUnstructuredConverter.FromUnstructured(res.Object, clusterRoleBinding)
		if err != nil {
			return nil, fmt.Errorf("failed to convert unstructured to ClusterRoleBinding: %w", err)
		}

		return clusterRoleBinding, nil
	} else if err != nil {
		return nil, fmt.Errorf("failed to Get ClusterRoleBinding: %w", err)
	}

	if err := errIfHelmOwned(existingU, "ClusterRoleBinding"); err != nil {
		return nil, err
	}

	res, err := cli.Update(ctx, u, metav1.UpdateOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to Update ClusterRoleBinding: %w", err)
	}

	clusterRoleBinding = &rbacv1.ClusterRoleBinding{}
	err = runtime.DefaultUnstructuredConverter.FromUnstructured(res.Object, clusterRoleBinding)
	if err != nil {
		return nil, fmt.Errorf("failed to convert unstructured to ClusterRoleBinding: %w", err)
	}

	return clusterRoleBinding, nil
}

func (i *RBACInstaller) UninstallRBAC(rbac *RBAC) error {
	if rbac == nil {
		return fmt.Errorf("RBAC cannot be nil")
	}
	// in this case it will not delete the namespaces, only the roles and bindings because Helm should delete the namespaces itself

	if rbac.ClusterRole != nil {
		err := i.DeleteClusterRole(context.Background(), rbac.ClusterRole.Name)
		if err != nil {
			return fmt.Errorf("failed to Delete ClusterRole: %w", err)
		}
	}
	if rbac.ClusterRoleBinding != nil {
		err := i.DeleteClusterRoleBinding(context.Background(), rbac.ClusterRoleBinding.Name)
		if err != nil {
			return fmt.Errorf("failed to Delete ClusterRoleBinding: %w", err)
		}
	}
	for _, ns := range rbac.Namespaced {
		if ns.Role != nil {
			err := i.DeleteRole(context.Background(), ns.Role.Namespace, ns.Role.Name)
			if err != nil {
				return fmt.Errorf("failed to Delete Role: %w", err)
			}
		}
		if ns.RoleBinding != nil {
			err := i.DeleteRoleBinding(context.Background(), ns.RoleBinding.Namespace, ns.RoleBinding.Name)
			if err != nil {
				return fmt.Errorf("failed to Delete RoleBinding: %w", err)
			}
		}
	}
	return nil
}

func (i *RBACInstaller) DeleteRole(ctx context.Context, namespace, name string) error {
	cli := i.DynamicClient.Resource(
		schema.GroupVersionResource{
			Group:    "rbac.authorization.k8s.io",
			Version:  "v1",
			Resource: "roles",
		},
	).Namespace(namespace)

	err := cli.Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("failed to Delete Role: %w", err)
	}
	return nil
}

func (i *RBACInstaller) DeleteRoleBinding(ctx context.Context, namespace, name string) error {
	cli := i.DynamicClient.Resource(
		schema.GroupVersionResource{
			Group:    "rbac.authorization.k8s.io",
			Version:  "v1",
			Resource: "rolebindings",
		},
	).Namespace(namespace)

	err := cli.Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("failed to Delete RoleBinding: %w", err)
	}
	return nil
}

func (i *RBACInstaller) DeleteClusterRole(ctx context.Context, name string) error {
	cli := i.DynamicClient.Resource(
		schema.GroupVersionResource{
			Group:    "rbac.authorization.k8s.io",
			Version:  "v1",
			Resource: "clusterroles",
		},
	)

	err := cli.Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("failed to Delete ClusterRole: %w", err)
	}
	return nil
}

func (i *RBACInstaller) DeleteClusterRoleBinding(ctx context.Context, name string) error {
	cli := i.DynamicClient.Resource(
		schema.GroupVersionResource{
			Group:    "rbac.authorization.k8s.io",
			Version:  "v1",
			Resource: "clusterrolebindings",
		},
	)

	err := cli.Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("failed to Delete ClusterRoleBinding: %w", err)
	}
	return nil
}

func ruleExists(rules []rbacv1.PolicyRule, target rbacv1.PolicyRule) bool {
	for _, rule := range rules {
		if reflect.DeepEqual(rule, target) {
			return true
		}
	}
	return false
}

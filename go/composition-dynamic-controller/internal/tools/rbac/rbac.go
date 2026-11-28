package rbac

import (
	corev1 "k8s.io/api/core/v1"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type Namespaced struct {
	Role        *rbacv1.Role
	RoleBinding *rbacv1.RoleBinding
}

type RBAC struct {
	Namespaces         []*corev1.Namespace
	Namespaced         map[string]Namespaced
	ClusterRole        *rbacv1.ClusterRole
	ClusterRoleBinding *rbacv1.ClusterRoleBinding
}

// DeepCopy returns a copy that shares no pointer or map with r, so a caller can keep the rules it
// computed and hand a private copy to every apply (#697). The apply path may set fields on what it
// is given; a cache must not see them.
func (r *RBAC) DeepCopy() *RBAC {
	if r == nil {
		return nil
	}
	out := &RBAC{
		ClusterRole:        r.ClusterRole.DeepCopy(),
		ClusterRoleBinding: r.ClusterRoleBinding.DeepCopy(),
	}
	if r.Namespaces != nil {
		out.Namespaces = make([]*corev1.Namespace, len(r.Namespaces))
		for i, ns := range r.Namespaces {
			out.Namespaces[i] = ns.DeepCopy()
		}
	}
	if r.Namespaced != nil {
		out.Namespaced = make(map[string]Namespaced, len(r.Namespaced))
		for k, v := range r.Namespaced {
			out.Namespaced[k] = Namespaced{Role: v.Role.DeepCopy(), RoleBinding: v.RoleBinding.DeepCopy()}
		}
	}
	return out
}

func CreateNamespace(name string, releaseName string, releaseNamespace string) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "Helm", // We need to add this label to avoid issues with Helm
			},
			Annotations: map[string]string{
				"meta.helm.sh/release-name":      releaseName,
				"meta.helm.sh/release-namespace": releaseNamespace,
			},
		},
	}

}

func InitRole(name string, namespace string) *rbacv1.Role {
	return &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Rules: []rbacv1.PolicyRule{},
	}
}

func InitRoleBinding(name, roleRefName, namespace string, saName string, saNamespace string) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		RoleRef: rbacv1.RoleRef{
			Kind:     "Role",
			Name:     roleRefName,
			APIGroup: "rbac.authorization.k8s.io",
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      saName,
				Namespace: saNamespace,
			},
		},
	}
}

func InitClusterRole(name string) *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Rules: []rbacv1.PolicyRule{},
	}
}

func InitClusterRoleBinding(name, roleRefName, saName, saNamespace string) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     roleRefName,
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      saName,
				Namespace: saNamespace,
			},
		},
	}
}

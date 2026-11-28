package deploy

import (
	"context"
	"errors"
	"fmt"

	"github.com/krateo-platformops/core-provider/internal/tools/auditrec"
	kubecli "github.com/krateo-platformops/core-provider/internal/tools/kube"
	"github.com/krateo-platformops/core-provider/internal/tools/restactionrbac"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The apiRef read grants that a composition's extras place in a namespace are Roles and RoleBindings
// there, one pair per (definition version, namespace) under one name (core-provider#691). They are
// found, pruned and torn down by this label, whose value is that name, because a namespaced object
// cannot be owned by the cluster-scoped objects around it and a namespace is not known from the
// definition alone.
const (
	// ApiRefRBACLabel marks a Role or RoleBinding as the apiRef grant of one definition version; its
	// value is ApiRefRBACName.
	ApiRefRBACLabel = "core.krateo.io/apiref-rbac"
	// ApiRefRBACDefinitionLabel and ApiRefRBACDefinitionNamespaceLabel name the definition that wrote it.
	ApiRefRBACDefinitionLabel          = "core.krateo.io/apiref-rbac-definition"
	ApiRefRBACDefinitionNamespaceLabel = "core.krateo.io/apiref-rbac-definition-namespace"
)

// ApiRefRBACName is the name of a definition version's apiRef RBAC objects, shared by its
// ClusterRole, ClusterRoleBinding and by every Role and RoleBinding.
func ApiRefRBACName(gvr schema.GroupVersionResource) string {
	return restActionRBACName(resourceNamer(gvr.Resource, gvr.Version))
}

// ApiRefRBACGroup is the group the definition version's apiRef grants are bound to, read from the
// ClusterRoleBinding Deploy wrote, so a pool member's group (its pool's) is the one used. ok is false
// when it is not there yet; an error is a failed read, which is not "absent".
func ApiRefRBACGroup(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource) (group string, ok bool, err error) {
	u, err := dyn.Resource(clusterRoleBindingGVR).Get(ctx, ApiRefRBACName(gvr), metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("reading the apiRef ClusterRoleBinding: %w", err)
	}
	var crb rbacv1.ClusterRoleBinding
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &crb); err != nil {
		return "", false, err
	}
	for _, s := range crb.Subjects {
		if s.Kind == rbacv1.GroupKind {
			return s.Name, true, nil
		}
	}
	return "", false, nil
}

// ApplyApiRefRoles applies composed's Roles and RoleBindings, labelled with the definition, and then
// deletes the ones this definition version has in a namespace composed no longer names.
//
// composed must come from a read-set that WAS determined: the caller never reaches here when snowplow
// could not be asked, because an empty or partial plan reads as "no namespace needs a Role" and this
// deletes on that. A failure to LIST what exists is an error and deletes nothing.
func ApplyApiRefRoles(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource,
	definition client.ObjectKey, composed restactionrbac.Composed) error {
	name := ApiRefRBACName(gvr)
	labels := map[string]string{
		ApiRefRBACLabel:                    name,
		ApiRefRBACDefinitionLabel:          definition.Name,
		ApiRefRBACDefinitionNamespaceLabel: definition.Namespace,
	}
	var errs []error
	want := map[string]bool{}
	for i := range composed.Roles {
		role, binding := composed.Roles[i], composed.RoleBindings[i]
		role.Labels, binding.Labels = labels, labels
		want[role.Namespace] = true
		if _, err := applyRecorded(ctx, dyn, roleGVR, "Role", auditrec.ActionRBACApply, &role, kubecli.ApplyOptions{}); err != nil {
			errs = append(errs, fmt.Errorf("applying Role %s/%s: %w", role.Namespace, role.Name, err))
			continue
		}
		if _, err := applyRecorded(ctx, dyn, roleBindingGVR, "RoleBinding", auditrec.ActionRBACApply, &binding, kubecli.ApplyOptions{}); err != nil {
			errs = append(errs, fmt.Errorf("applying RoleBinding %s/%s: %w", binding.Namespace, binding.Name, err))
		}
	}
	if err := pruneApiRefRoles(ctx, dyn, name, want); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// DeleteApiRefRoles removes every Role and RoleBinding of the definition version name, on teardown.
// A LIST that fails is an error: it must never read as "there are none", or a live composition's
// grants would be left (here) or deleted (where the same mistake is made on the other side).
func DeleteApiRefRoles(ctx context.Context, dyn dynamic.Interface, name string) error {
	return pruneApiRefRoles(ctx, dyn, name, nil)
}

// pruneApiRefRoles deletes the labelled Roles and RoleBindings whose namespace is not in keep.
func pruneApiRefRoles(ctx context.Context, dyn dynamic.Interface, name string, keep map[string]bool) error {
	sel := ApiRefRBACLabel + "=" + name
	var errs []error
	for _, k := range []struct {
		gvr  schema.GroupVersionResource
		kind string
		obj  func(ns, n string) client.Object
	}{
		{roleBindingGVR, "RoleBinding", func(ns, n string) client.Object {
			return &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: n}}
		}},
		{roleGVR, "Role", func(ns, n string) client.Object {
			return &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: n}}
		}},
	} {
		list, err := dyn.Resource(k.gvr).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{LabelSelector: sel})
		if err != nil {
			return fmt.Errorf("listing the apiRef %ss (nothing is deleted): %w", k.kind, err)
		}
		for i := range list.Items {
			it := &list.Items[i]
			if keep[it.GetNamespace()] {
				continue
			}
			if _, err := uninstallRecorded(ctx, dyn, k.gvr, k.kind, auditrec.ActionRBACDelete, k.obj(it.GetNamespace(), it.GetName()), kubecli.UninstallOptions{}); err != nil {
				errs = append(errs, fmt.Errorf("deleting %s %s/%s: %w", k.kind, it.GetNamespace(), it.GetName(), err))
			}
		}
	}
	return errors.Join(errs...)
}

package rbac

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

// OwnerLabelPrefix marks a derived RBAC object as belonging to a composition (#402). The key is
// OwnerLabelPrefix + the composition's UID and the value is "true".
//
// It is a SET, one key per owner, not a single "owner: <uid>" label, because a derived object can
// be shared: every object is named <release name>-krateo-rbac, and the release name is overridable
// (krateo.io/release-name) and loses its UID suffix with safeReleaseName off, so two compositions
// can render the same ClusterRole name. A single owner label would be last-writer-wins, and deleting
// by it would remove RBAC another composition still uses.
//
// Why it exists: a composition's Delete removed its RBAC by REGENERATING it from the chart (through
// chart-inspector), so a composition whose chart could not be read (its passwordRef Secret gone)
// could never finish deleting. With the owner recorded on the objects, Delete finds them by label.
const OwnerLabelPrefix = "owner.composition.krateo.io/"

// OwnerLabelKey is the label key recording uid as an owner.
func OwnerLabelKey(uid types.UID) string { return OwnerLabelPrefix + string(uid) }

// SetOwner records uid as an owner of every object r applies: ClusterRole, ClusterRoleBinding, and
// each Role and RoleBinding. Namespaces are deliberately NOT labelled: they are created for the
// chart, not as RBAC, and removing a Namespace because a label matched would delete its contents.
func (r *RBAC) SetOwner(uid types.UID) {
	if r == nil || uid == "" {
		return
	}
	key := OwnerLabelKey(uid)
	set := func(m *metav1.ObjectMeta) {
		if m.Labels == nil {
			m.Labels = map[string]string{}
		}
		m.Labels[key] = "true"
	}
	if r.ClusterRole != nil {
		set(&r.ClusterRole.ObjectMeta)
	}
	if r.ClusterRoleBinding != nil {
		set(&r.ClusterRoleBinding.ObjectMeta)
	}
	for _, ns := range r.Namespaced {
		if ns.Role != nil {
			set(&ns.Role.ObjectMeta)
		}
		if ns.RoleBinding != nil {
			set(&ns.RoleBinding.ObjectMeta)
		}
	}
}

// mergeLabels returns existing with every label of desired added (desired wins on a conflict), and
// whether that changed anything. Existing keys desired lacks are kept: they include OTHER owners.
func mergeLabels(existing, desired map[string]string) (map[string]string, bool) {
	out := make(map[string]string, len(existing)+len(desired))
	for k, v := range existing {
		out[k] = v
	}
	changed := false
	for k, v := range desired {
		if cur, ok := out[k]; !ok || cur != v {
			out[k] = v
			changed = true
		}
	}
	return out, changed
}

// otherOwners counts owner keys on labels other than uid's.
func otherOwners(labels map[string]string, uid types.UID) int {
	own := OwnerLabelKey(uid)
	n := 0
	for k := range labels {
		if strings.HasPrefix(k, OwnerLabelPrefix) && k != own {
			n++
		}
	}
	return n
}

var ownedRBACResources = []struct {
	gvr        schema.GroupVersionResource
	namespaced bool
}{
	{schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}, false},
	{schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}, false},
	{schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}, true},
	{schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}, true},
}

// UninstallOwned releases uid's ownership of every derived RBAC object that records it, with no
// chart: an object with no other owner is deleted; one that other compositions also own keeps
// existing, with only uid's key removed. It returns how many objects recorded uid.
//
// A failed LIST is an error, never "none found": the caller falls back to regenerating from the
// chart when nothing is found, and an unreadable cluster is not evidence that nothing is labelled.
func (i *RBACInstaller) UninstallOwned(ctx context.Context, uid types.UID) (int, error) {
	if uid == "" {
		return 0, fmt.Errorf("refusing to release RBAC for an empty composition UID: the selector would match nothing and read as \"nothing owned\"")
	}
	key := OwnerLabelKey(uid)
	found := 0
	for _, res := range ownedRBACResources {
		var lister dynamic.ResourceInterface = i.DynamicClient.Resource(res.gvr)
		if res.namespaced {
			lister = i.DynamicClient.Resource(res.gvr).Namespace(metav1.NamespaceAll)
		}
		list, err := lister.List(ctx, metav1.ListOptions{LabelSelector: key})
		if err != nil {
			return found, fmt.Errorf("listing %s owned by %s: %w", res.gvr.Resource, uid, err)
		}
		for idx := range list.Items {
			u := &list.Items[idx]
			found++
			var cli dynamic.ResourceInterface = i.DynamicClient.Resource(res.gvr)
			if res.namespaced {
				cli = i.DynamicClient.Resource(res.gvr).Namespace(u.GetNamespace())
			}
			if otherOwners(u.GetLabels(), uid) > 0 {
				labels := u.GetLabels()
				delete(labels, key)
				u.SetLabels(labels)
				if _, err := cli.Update(ctx, u, metav1.UpdateOptions{}); err != nil && !errors.IsNotFound(err) {
					return found, fmt.Errorf("releasing %s %s (still owned by others): %w", res.gvr.Resource, describe(u.GetNamespace(), u.GetName()), err)
				}
				continue
			}
			if err := cli.Delete(ctx, u.GetName(), metav1.DeleteOptions{}); err != nil && !errors.IsNotFound(err) {
				return found, fmt.Errorf("deleting %s %s: %w", res.gvr.Resource, describe(u.GetNamespace(), u.GetName()), err)
			}
		}
	}
	return found, nil
}

func describe(ns, name string) string {
	if ns == "" {
		return name
	}
	return ns + "/" + name
}

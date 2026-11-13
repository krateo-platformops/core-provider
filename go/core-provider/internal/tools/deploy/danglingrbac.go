package deploy

import (
	"context"
	"errors"
	"fmt"
	"github.com/krateo-platformops/core-provider/internal/tools/auditrec"
	"regexp"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// #414: bindings left behind by retired composition-controller versions.
//
// Derived RBAC was renamed twice (the release-name hash era, then #130's -krateo-rbac suffix) and
// the old-named objects were never removed. ResubjectDerivedRBAC deliberately touches only the
// current name (repointing an unrecognised binding at a live identity would hand it the orphaned
// role's rules), so those bindings kept naming their era's controller ServiceAccount, which version
// retirement later deleted. On krateo-057 (2026-11-09) that was 128 bindings, all still holding rules.
//
// Per Diego (2026-11-09: "retire by subject"), a binding is removed by its RECORDED SUBJECT, never by
// its name. Every one of these must hold:
//  1. every subject is the same ServiceAccount, <plural>-<version>, in one of the definition's
//     controller placements;
//  2. that ServiceAccount does not exist (a failed read is an error, never "absent");
//  3. the version is not on the Kind's CRD (a version still there may come back);
//  4. the binding is not helm-owned (a chart's own object; removing it would fight helm).
// Deleting is safe where repointing was not: a binding to a ServiceAccount that does not exist grants
// nothing to anyone today, and removing it closes the door on anything that later takes that name.

// compositionVersion is the shape of a version core-provider derives for a composition Kind: "v" plus
// the chart's semver with dots as dashes (chart.go, adoption.go), e.g. v0-1-2, v1-8-101, and a
// prerelease tail such as v1-0-0-rc-1. A subject <plural>-<x> is a composition controller's only if <x>
// has this shape: two CRDs can share a plural (modelconfigs.kagent.dev and
// modelconfigs.composition.krateo.io on krateo-057), so a non-composition ServiceAccount named
// modelconfigs-v1alpha2 in the controller namespace must never read as a retired controller's
// (Diego, 2026-11-10, from installer's finding). Every served composition.krateo.io version on 057
// other than vacuum matches.
var compositionVersion = regexp.MustCompile(`^v[0-9]+-[0-9]+-[0-9]+(-[0-9A-Za-z-]+)?$`)

// helmReleaseNameAnnotation marks an object helm owns (also used by errIfHelmOwned in the RBAC
// installer of the cdc).
const helmReleaseNameAnnotation = "meta.helm.sh/release-name"

// DanglingBinding is one binding that names only a retired controller's absent ServiceAccount.
type DanglingBinding struct {
	Kind           string // RoleBinding or ClusterRoleBinding
	Namespace      string // "" for a ClusterRoleBinding
	Name           string
	ServiceAccount string // namespace/name of the retired controller's ServiceAccount
	RoleRefKind    string
	RoleRefName    string
}

func (d DanglingBinding) String() string {
	if d.Namespace == "" {
		return fmt.Sprintf("%s/%s -> %s", d.Kind, d.Name, d.ServiceAccount)
	}
	return fmt.Sprintf("%s %s/%s -> %s", d.Kind, d.Namespace, d.Name, d.ServiceAccount)
}

// DanglingOptions scopes the search to one Kind.
type DanglingOptions struct {
	DynamicClient dynamic.Interface
	// Resource is the Kind's plural (gvr.Resource); its controllers' ServiceAccounts are
	// <Resource>-<version>.
	Resource string
	// KnownVersions are the versions on the Kind's CRD, current included. A ServiceAccount for one of
	// them is never a candidate.
	KnownVersions []string
	// Placements are the namespaces the Kind's controllers live or lived in (controllerPlacements).
	Placements []string
}

// FindDanglingControllerBindings lists every RoleBinding and ClusterRoleBinding that satisfies all
// four conditions above. It changes nothing. Bindings that fail only the "absent" check, or that are
// helm-owned, are reported in kept with the reason, so a dry run shows what was considered.
func FindDanglingControllerBindings(ctx context.Context, opts DanglingOptions) (found []DanglingBinding, kept []string, err error) {
	if opts.Resource == "" || len(opts.Placements) == 0 {
		return nil, nil, fmt.Errorf("refusing to search for dangling bindings without a resource and placements")
	}
	known := make(map[string]bool, len(opts.KnownVersions))
	for _, v := range opts.KnownVersions {
		known[v] = true
	}
	inPlacement := make(map[string]bool, len(opts.Placements))
	for _, p := range opts.Placements {
		inPlacement[p] = true
	}
	prefix := opts.Resource + "-"
	saExists := map[string]bool{}

	for _, k := range []struct {
		kind string
		gvr  schema.GroupVersionResource
	}{
		{"ClusterRoleBinding", clusterRoleBindingGVR},
		{"RoleBinding", roleBindingGVR},
	} {
		list, lerr := opts.DynamicClient.Resource(k.gvr).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
		if lerr != nil {
			// A failed LIST is not "nothing dangling": stop rather than report a partial picture.
			return nil, nil, fmt.Errorf("listing %s: %w", k.gvr.Resource, lerr)
		}
		for i := range list.Items {
			b := &list.Items[i]
			sa, ok := soleServiceAccountSubject(b)
			if !ok {
				continue
			}
			ns, name, _ := strings.Cut(sa, "/")
			if !inPlacement[ns] || !strings.HasPrefix(name, prefix) {
				continue
			}
			version := strings.TrimPrefix(name, prefix)
			if !compositionVersion.MatchString(version) || known[version] {
				continue
			}
			id := k.kind + " " + b.GetNamespace() + "/" + b.GetName()
			if _, helm := b.GetAnnotations()[helmReleaseNameAnnotation]; helm {
				kept = append(kept, id+": helm-owned")
				continue
			}
			exists, cached := saExists[sa]
			if !cached {
				_, gerr := opts.DynamicClient.Resource(serviceAccountGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
				switch {
				case gerr == nil:
					exists = true
				case apierrors.IsNotFound(gerr):
					exists = false
				default:
					// Unreadable is not absent: removing on a read error would delete live grants.
					return nil, nil, fmt.Errorf("reading ServiceAccount %s: %w", sa, gerr)
				}
				saExists[sa] = exists
			}
			if exists {
				kept = append(kept, id+": ServiceAccount "+sa+" exists")
				continue
			}
			roleKind, _, _ := unstructured.NestedString(b.Object, "roleRef", "kind")
			roleName, _, _ := unstructured.NestedString(b.Object, "roleRef", "name")
			found = append(found, DanglingBinding{
				Kind: k.kind, Namespace: b.GetNamespace(), Name: b.GetName(),
				ServiceAccount: sa, RoleRefKind: roleKind, RoleRefName: roleName,
			})
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].String() < found[j].String() })
	sort.Strings(kept)
	return found, kept, nil
}

// soleServiceAccountSubject returns "namespace/name" when EVERY subject of b is that one
// ServiceAccount (a namespaceless subject takes the binding's namespace), and false otherwise:
// no subjects, any non-ServiceAccount subject, or two different ServiceAccounts.
func soleServiceAccountSubject(b *unstructured.Unstructured) (string, bool) {
	subjects, _, _ := unstructured.NestedSlice(b.Object, "subjects")
	if len(subjects) == 0 {
		return "", false
	}
	sole := ""
	for _, s := range subjects {
		m, ok := s.(map[string]any)
		if !ok || m["kind"] != "ServiceAccount" {
			return "", false
		}
		name, _ := m["name"].(string)
		ns, _ := m["namespace"].(string)
		if ns == "" {
			ns = b.GetNamespace()
		}
		if name == "" || ns == "" {
			return "", false
		}
		id := ns + "/" + name
		if sole != "" && sole != id {
			return "", false
		}
		sole = id
	}
	return sole, true
}

// RemoveDanglingControllerBindings deletes the bindings found, then each Role or ClusterRole they
// referenced that NO remaining binding references and that is not helm-owned. It returns what it
// removed. NotFound counts as removed (someone got there first). A failed list of the remaining
// bindings keeps every role, because "no other binding" cannot be shown.
func RemoveDanglingControllerBindings(ctx context.Context, dyn dynamic.Interface, found []DanglingBinding) (removed []string, err error) {
	var errs []error
	for _, d := range found {
		gvr := roleBindingGVR
		if d.Kind == "ClusterRoleBinding" {
			gvr = clusterRoleBindingGVR
		}
		derr := dyn.Resource(gvr).Namespace(d.Namespace).Delete(ctx, d.Name, metav1.DeleteOptions{})
		auditrec.RecordDeleteSent(ctx, auditrec.ActionRBACDelete, auditrec.Target{GVR: gvr, Kind: d.Kind, Namespace: d.Namespace, Name: d.Name, Namespaces: auditrec.LabelsFrom(dyn)}, "", derr)
		if derr != nil && !apierrors.IsNotFound(derr) {
			errs = append(errs, fmt.Errorf("deleting %s: %w", d, derr))
			continue
		}
		removed = append(removed, d.String())
	}

	referenced, rerr := referencedRoles(ctx, dyn)
	if rerr != nil {
		return removed, errors.Join(append(errs, fmt.Errorf("listing bindings to decide which roles are still referenced; keeping every role: %w", rerr))...)
	}
	seen := map[string]bool{}
	for _, d := range found {
		var key, ns string
		var gvr schema.GroupVersionResource
		switch d.RoleRefKind {
		case "ClusterRole":
			gvr, key = clusterRoleGVR, "ClusterRole//"+d.RoleRefName
		case "Role":
			gvr, ns, key = roleGVR, d.Namespace, "Role/"+d.Namespace+"/"+d.RoleRefName
		default:
			continue
		}
		if seen[key] || referenced[key] {
			continue
		}
		seen[key] = true
		role, gerr := dyn.Resource(gvr).Namespace(ns).Get(ctx, d.RoleRefName, metav1.GetOptions{})
		if apierrors.IsNotFound(gerr) {
			continue
		}
		if gerr != nil {
			errs = append(errs, fmt.Errorf("reading %s: %w", key, gerr))
			continue
		}
		if _, helm := role.GetAnnotations()[helmReleaseNameAnnotation]; helm {
			continue
		}
		derr := dyn.Resource(gvr).Namespace(ns).Delete(ctx, d.RoleRefName, metav1.DeleteOptions{})
		auditrec.RecordDeleteSent(ctx, auditrec.ActionRBACDelete, auditrec.Target{GVR: gvr, Kind: d.RoleRefKind, Namespace: ns, Name: d.RoleRefName, Namespaces: auditrec.LabelsFrom(dyn)}, string(role.GetUID()), derr)
		if derr != nil && !apierrors.IsNotFound(derr) {
			errs = append(errs, fmt.Errorf("deleting %s: %w", key, derr))
			continue
		}
		removed = append(removed, key)
	}
	return removed, errors.Join(errs...)
}

// referencedRoles is the set of roles still referenced by any binding, as "Role/<ns>/<name>" and
// "ClusterRole//<name>". A RoleBinding can reference a ClusterRole too.
func referencedRoles(ctx context.Context, dyn dynamic.Interface) (map[string]bool, error) {
	out := map[string]bool{}
	for _, gvr := range []schema.GroupVersionResource{clusterRoleBindingGVR, roleBindingGVR} {
		list, err := dyn.Resource(gvr).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		for i := range list.Items {
			b := &list.Items[i]
			kind, _, _ := unstructured.NestedString(b.Object, "roleRef", "kind")
			name, _, _ := unstructured.NestedString(b.Object, "roleRef", "name")
			switch kind {
			case "ClusterRole":
				out["ClusterRole//"+name] = true
			case "Role":
				out["Role/"+b.GetNamespace()+"/"+name] = true
			}
		}
	}
	return out, nil
}

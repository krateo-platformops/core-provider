package deploy

import (
	"context"
	"errors"
	"fmt"
	"strings"

	contexttools "github.com/krateo-platformops/core-provider/internal/tools/context"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// DerivedRBACNameSuffix is the suffix composition-dynamic-controller gives every RBAC object it
// derives for a composition (rbacgen.rbacNameSuffix). It is duplicated here rather than imported
// because the two live in separate Go modules; TestDerivedRBACSuffixMatchesRBACGen reads the
// declaration out of the cdc source and fails if they ever diverge.
//
// It is also the safety predicate for the re-subjecting below, and that is the whole reason it can
// be one. The suffix exists (krateo-platformops/core-provider#130) precisely because a chart's own
// `{{ include "chart.fullname" . }}` can produce `<release>` but cannot produce `<release>-krateo-rbac`
// — a chart would have to be NAMED "krateo-rbac". So a binding carrying this suffix is one the
// platform generated and may rewrite. A binding without it may be a chart's, or pre-rename debris
// still naming a ServiceAccount retired several versions ago, and repointing THAT at a live identity
// would silently hand it whatever rules the orphaned role still holds.
const DerivedRBACNameSuffix = "-krateo-rbac"

// ResubjectOptions describes one controller-identity rotation: the per-version ServiceAccount being
// retired, and the one taking over. Namespace is where the retired one lives — the
// CompositionDefinition's namespace — not the namespace of the bindings, which follow their
// compositions and may be anywhere.
//
// NewNamespace is where the one taking over lives, when that differs: the shared controller of a
// Kind+version moving to another CompositionDefinition's namespace keeps its ServiceAccount NAME
// and changes only its namespace. Empty means the same namespace as the retired one.
type ResubjectOptions struct {
	DynamicClient dynamic.Interface
	Namespace     string
	NewNamespace  string
	OldGVR        schema.GroupVersionResource
	NewGVR        schema.GroupVersionResource
}

// ResubjectDerivedRBAC repoints every derived RoleBinding and ClusterRoleBinding that still names
// the retired controller ServiceAccount at the one replacing it, and reports how many it rewrote.
//
// A composition's derived RBAC is RELEASE-scoped — `<release>-krateo-rbac`, stable across version
// bumps — while the ServiceAccount it names is {resource}-{apiVersion}-scoped and replaced on every
// bump. Nothing owned keeping the two in step. The binding went on naming a ServiceAccount that no
// longer existed, and because RBAC is name-based that binding grants nothing and reports nothing:
// the controller lost exactly the permissions for its chart's own kinds, and the first failure
// surfaced as "could not get information about the resource: <kind>", which reads as a missing CRD.
//
// cdc's Observe does re-subject the binding, so the gap used to close on the next reconcile of each
// composition — about 4m30s measured on krateo-057. But Observe only runs while the composition CR
// exists, and an umbrella dependency gate can withdraw the CR for exactly as long as the gating
// dependency is un-Ready; with the CR gone there is nothing to observe and the gap has no upper
// bound at all. Repairing it HERE, from the CompositionDefinition reconcile that rotates the
// identity in the first place, does not depend on the composition being present to be repaired.
//
// Call this only where the old controller is actually retired. While both versions coexist (an
// UpgradePolicy that defers migration, or another CompositionDefinition still referencing the old
// version) the old ServiceAccount is still a live identity serving live instances, and moving its
// bindings would take permissions away from a controller that is still using them.
func ResubjectDerivedRBAC(ctx context.Context, opts ResubjectOptions) (int, error) {
	log := contexttools.LoggerFromCtx(ctx, logging.NewNopLogger())

	oldSA := resourceNamer(opts.OldGVR.Resource, opts.OldGVR.Version)
	newSA := resourceNamer(opts.NewGVR.Resource, opts.NewGVR.Version)
	newNamespace := opts.NewNamespace
	if newNamespace == "" {
		newNamespace = opts.Namespace
	}
	if oldSA == newSA && opts.Namespace == newNamespace {
		return 0, nil
	}

	var (
		repaired int
		errs     []error
	)

	for _, kind := range []struct {
		gvr       schema.GroupVersionResource
		namespace string
	}{
		{gvr: clusterRoleBindingGVR, namespace: ""},
		{gvr: roleBindingGVR, namespace: metav1.NamespaceAll},
	} {
		list, err := opts.DynamicClient.Resource(kind.gvr).Namespace(kind.namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			errs = append(errs, fmt.Errorf("listing %s: %w", kind.gvr.Resource, err))
			continue
		}

		for i := range list.Items {
			obj := list.Items[i]
			if !strings.HasSuffix(obj.GetName(), DerivedRBACNameSuffix) {
				continue
			}

			rewritten, err := rewriteServiceAccountSubject(&obj, oldSA, newSA, opts.Namespace, newNamespace)
			if err != nil {
				errs = append(errs, fmt.Errorf("reading subjects of %s %s/%s: %w",
					kind.gvr.Resource, obj.GetNamespace(), obj.GetName(), err))
				continue
			}
			if !rewritten {
				continue
			}

			if _, err := opts.DynamicClient.Resource(kind.gvr).Namespace(obj.GetNamespace()).
				Update(ctx, &obj, metav1.UpdateOptions{}); err != nil {
				// Keep going: one composition's binding failing to update must not leave the rest
				// of the fleet stale. The joined error fails the reconcile, which retries the sweep.
				errs = append(errs, fmt.Errorf("re-subjecting %s %s/%s: %w",
					kind.gvr.Resource, obj.GetNamespace(), obj.GetName(), err))
				continue
			}

			repaired++
			log.Debug("Re-subjected derived RBAC after a controller identity rotation",
				"kind", kind.gvr.Resource, "name", obj.GetName(), "namespace", obj.GetNamespace(),
				"from", opts.Namespace+"/"+oldSA, "to", newNamespace+"/"+newSA)
		}
	}

	return repaired, errors.Join(errs...)
}

// rewriteServiceAccountSubject replaces the retired ServiceAccount (oldSA in saNamespace) with newSA
// in newNamespace wherever the binding names it,
// and reports whether anything changed. A binding naming some other identity, or already naming the
// new one, is left untouched — so a re-run after a partial failure is a no-op on what it repaired.
//
// Subjects are matched on (kind, namespace, name) together: a ServiceAccount name is unique only
// within its namespace, and the controller identities of two CompositionDefinitions in different
// namespaces can collide by name.
func rewriteServiceAccountSubject(obj *unstructured.Unstructured, oldSA, newSA, saNamespace, newNamespace string) (bool, error) {
	subjects, found, err := unstructured.NestedSlice(obj.Object, "subjects")
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}

	changed := false
	for i := range subjects {
		subject, ok := subjects[i].(map[string]any)
		if !ok {
			continue
		}
		if subject["kind"] != "ServiceAccount" || subject["name"] != oldSA || subject["namespace"] != saNamespace {
			continue
		}
		subject["name"] = newSA
		subject["namespace"] = newNamespace
		changed = true
	}
	if !changed {
		return false, nil
	}

	if err := unstructured.SetNestedSlice(obj.Object, subjects, "subjects"); err != nil {
		return false, err
	}
	return true, nil
}

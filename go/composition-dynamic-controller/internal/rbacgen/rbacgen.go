package rbacgen

import (
	"context"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/krateo-platformops/plumbing/ptr"
	rbacv1 "k8s.io/api/rbac/v1"

	"github.com/krateo-platformops/composition-dynamic-controller/internal/chartinspector"
	"github.com/krateo-platformops/composition-dynamic-controller/internal/tools/rbac"
)

type RBACGenInterface interface {
	Generate(ctx context.Context, params Parameters) (*rbac.RBAC, error)
	WithBaseName(string) RBACGenInterface
}

type Parameters struct {
	CompositionName                string                      // The name of the composition. Required.
	CompositionNamespace           string                      // The namespace of the composition. Required.
	CompositionGVR                 schema.GroupVersionResource // The GVR of the composition.
	CompositionDefinitionName      string                      // The name of the composition definition. Required.
	CompositionDefinitionNamespace string                      // The namespace of the composition definition.
	CompositionDefintionGVR        schema.GroupVersionResource // The GVR of the composition definition.
}

type RBACGen struct {
	chartInspector chartinspector.ChartInspectorInterface
	baseName       string
	saName         string
	saNamespace    string
}

var _ RBACGenInterface = &RBACGen{}

func NewRBACGen(saName string, saNamespace string, chartInspector chartinspector.ChartInspectorInterface) *RBACGen {
	return &RBACGen{
		chartInspector: chartInspector,
		saName:         saName,
		saNamespace:    saNamespace,
	}
}

func (r *RBACGen) WithBaseName(baseName string) RBACGenInterface {
	r.baseName = baseName
	return r
}

// rbacNameSuffix keeps the RBAC we generate out of the namespace a chart can address.
//
// The generated objects used to be named after the release — which is the composition name. That
// is exactly what Helm's conventional fullname helper produces:
//
//	{{- if contains $name .Release.Name }}{{ .Release.Name }}{{ else }}...
//
// so ANY chart shipping its own Role/RoleBinding under `{{ include "chart.fullname" . }}` collides
// with ours whenever the composition name contains the chart name — the normal case, since a
// composition is usually named after its chart. It is one object with two writers, and Helm wins:
// Role (21) and RoleBinding (23) are applied before workloads like CronJob (34) in InstallOrder, so
// the overwrite is deterministic, not racy.
//
// Both halves were lost. The derived rules were replaced by the chart's, AND the binding's subject
// was rewritten from the cdc's ServiceAccount to the chart's, so the cdc ended up with no link to
// the Role at all. The install then failed on the first kind the STATIC baseline role does not
// already cover — which is how this surfaced as "cronjobs is missing from generated RBAC"
// (krateo-platformops/core-provider#130).
//
// The chart is not at fault: that helper is what `helm create` scaffolds. So the platform moves.
// The suffix has to be something a fullname template cannot produce — a chart would have to be
// NAMED "krateo-rbac" and released under a matching name to collide again.
const rbacNameSuffix = "-krateo-rbac"

// rbacName is the name of every RBAC object we generate for this composition.
func (r *RBACGen) rbacName() string {
	return r.baseName + rbacNameSuffix
}

func (r *RBACGen) Generate(ctx context.Context, params Parameters) (*rbac.RBAC, error) {
	resources, err := r.chartInspector.Resources(ctx, chartinspector.Parameters{
		CompositionName:                params.CompositionName,
		CompositionNamespace:           params.CompositionNamespace,
		CompositionGroup:               params.CompositionGVR.Group,
		CompositionVersion:             params.CompositionGVR.Version,
		CompositionResource:            params.CompositionGVR.Resource,
		CompositionDefinitionName:      params.CompositionDefinitionName,
		CompositionDefinitionNamespace: params.CompositionDefinitionNamespace,
		CompositionDefinitionGroup:     params.CompositionDefintionGVR.Group,
		CompositionDefinitionVersion:   params.CompositionDefintionGVR.Version,
		CompositionDefinitionResource:  params.CompositionDefintionGVR.Resource,
	})
	if err != nil {
		return nil, fmt.Errorf("getting resources from chart-inspector: %w", err)
	}
	policy := rbac.RBAC{
		ClusterRole:        rbac.InitClusterRole(r.rbacName()),
		ClusterRoleBinding: rbac.InitClusterRoleBinding(r.rbacName(), r.rbacName(), r.saName, r.saNamespace),
		Namespaced:         map[string]rbac.Namespaced{},
		Namespaces:         []*corev1.Namespace{},
	}

	// Aggregate observed resources into ONE rule per (group, resource, namespace), unioning verbs.
	// The tracer stamps read-only verbs (get/list/watch) on collection reads — a Helm `lookup` of a
	// set, never a create — and ["*"] on named observations (ambiguous create-check vs named lookup
	// under dry-run). A GVR seen both ways unions to ["*"]. This also collapses the duplicate
	// PolicyRules the old append-per-resource loop emitted for a GVR observed more than once.
	type ruleKey struct{ group, resource, namespace string }
	order := make([]ruleKey, 0, len(resources))
	verbsByKey := map[ruleKey]map[string]struct{}{}

	for _, resource := range resources {
		// A NAMED namespace object needs its Namespace created; a collection lookup of namespaces
		// (Name=="") must NOT — it would create a Namespace named "".
		if resource.Namespace == "" && resource.Group == "" && resource.Version == "v1" &&
			resource.Resource == "namespaces" && resource.Name != "" {
			policy.Namespaces = append(policy.Namespaces, rbac.CreateNamespace(resource.Name, r.baseName, params.CompositionNamespace))
		}

		key := ruleKey{group: resource.Group, resource: resource.Resource, namespace: resource.Namespace}
		if _, ok := verbsByKey[key]; !ok {
			verbsByKey[key] = map[string]struct{}{}
			order = append(order, key)
		}
		verbs := resource.Verbs
		if len(verbs) == 0 {
			verbs = []string{"*"} // older inspector sends no verbs -> preserve today's behavior
		}
		for _, v := range verbs {
			verbsByKey[key][v] = struct{}{}
		}
	}

	for _, key := range order {
		rule := rbacv1.PolicyRule{
			APIGroups: []string{key.group},
			Resources: []string{key.resource},
			Verbs:     collapseVerbs(verbsByKey[key]),
		}
		if key.namespace == "" {
			policy.ClusterRole.Rules = append(policy.ClusterRole.Rules, rule)
			continue
		}
		if _, ok := policy.Namespaced[key.namespace]; !ok {
			policy.Namespaced[key.namespace] = rbac.Namespaced{
				Role:        rbac.InitRole(r.rbacName(), key.namespace),
				RoleBinding: rbac.InitRoleBinding(r.rbacName(), r.rbacName(), key.namespace, r.saName, r.saNamespace),
			}
		}
		policy.Namespaced[key.namespace].Role.Rules = append(policy.Namespaced[key.namespace].Role.Rules, rule)
	}
	return ptr.To(policy), nil
}

// collapseVerbs returns ["*"] if the set contains the wildcard, else the sorted verbs.
func collapseVerbs(set map[string]struct{}) []string {
	if _, ok := set["*"]; ok {
		return []string{"*"}
	}
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

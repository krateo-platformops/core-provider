package rbacgen

import (
	"context"
	"strings"
	"testing"

	"github.com/krateo-platformops/composition-dynamic-controller/internal/chartinspector"
	"github.com/krateo-platformops/composition-dynamic-controller/internal/tools/rbac"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// genNamed builds a policy with a base name that mimics a real release name.
func genNamed(t *testing.T, base string, res []chartinspector.Resource) *rbac.RBAC {
	t.Helper()
	mi := new(MockChartInspector)
	g := NewRBACGen("cdc-sa", "krateo-system", mi).WithBaseName(base)
	mi.On("Resources", mock.Anything, mock.Anything).Return(res, nil)
	policy, err := g.Generate(context.Background(), Parameters{
		CompositionName:      "nightly-review",
		CompositionNamespace: "krateo-system",
	})
	assert.NoError(t, err)
	assert.NotNil(t, policy)
	return policy
}

// The regression for core-provider#130.
//
// Generated RBAC used to be named after the release. Helm's conventional fullname helper collapses
// to the release name verbatim when the release name contains the chart name:
//
//	{{- if contains $name .Release.Name }}{{ .Release.Name }}{{ else }}...
//
// which is the normal case for a composition named after its chart. So a chart shipping its own
// Role under `{{ include "chart.fullname" . }}` addressed the SAME object as ours, and Helm won —
// Role (21) and RoleBinding (23) precede CronJob (34) in InstallOrder, making the overwrite
// deterministic rather than racy.
//
// Every generated name must therefore stay out of reach of that helper.
func TestGeneratedRBACNamesCannotCollideWithChartFullname(t *testing.T) {
	const release = "nightly-review"

	p := genNamed(t, release, []chartinspector.Resource{
		{Group: "batch", Version: "v1", Resource: "cronjobs", Namespace: "krateo-system", Name: release, Verbs: []string{"*"}},
		{Version: "v1", Resource: "nodes", Verbs: []string{"get", "list", "watch"}},
	})

	names := map[string]string{
		"ClusterRole":        p.ClusterRole.Name,
		"ClusterRoleBinding": p.ClusterRoleBinding.Name,
		"Role":               p.Namespaced["krateo-system"].Role.Name,
		"RoleBinding":        p.Namespaced["krateo-system"].RoleBinding.Name,
	}

	for kind, got := range names {
		// The exact failure: a chart whose fullname is the release name must not address this object.
		assert.NotEqual(t, release, got,
			"%s is named after the release, so a chart's {{ include \"chart.fullname\" . }} collides with it", kind)
		assert.True(t, strings.HasSuffix(got, rbacNameSuffix),
			"%s must carry the reserved suffix %q, got %q", kind, rbacNameSuffix, got)
	}
}

// The RoleBinding must keep binding the cdc's ServiceAccount. When the chart's RoleBinding won the
// collision it did not merely replace the rules — it rewrote the subject to the chart's own SA,
// leaving the cdc with no link to the Role at all. Restoring the rules alone would not have helped.
func TestGeneratedRoleBindingBindsTheControllerServiceAccount(t *testing.T) {
	p := genNamed(t, "nightly-review", []chartinspector.Resource{
		{Group: "batch", Version: "v1", Resource: "cronjobs", Namespace: "krateo-system", Name: "nightly-review", Verbs: []string{"*"}},
	})

	rb := p.Namespaced["krateo-system"].RoleBinding
	assert.Equal(t, p.Namespaced["krateo-system"].Role.Name, rb.RoleRef.Name,
		"the binding must reference the Role we generated, not one a chart may own")

	assert.Len(t, rb.Subjects, 1)
	assert.Equal(t, "cdc-sa", rb.Subjects[0].Name)
	assert.Equal(t, "krateo-system", rb.Subjects[0].Namespace)
}

// The derivation itself was never broken — #130 reported cronjobs as "missing from the generated
// set", but the inspector returns it and rbacgen carries it through. Pin that, so a future reader
// does not go looking for a hardcoded resource list that has never existed.
func TestCronJobIsCarriedThroughFromTheInspector(t *testing.T) {
	p := genNamed(t, "nightly-review", []chartinspector.Resource{
		{Group: "batch", Version: "v1", Resource: "cronjobs", Namespace: "krateo-system", Name: "nightly-review", Verbs: []string{"*"}},
	})

	rules := p.Namespaced["krateo-system"].Role.Rules
	assert.Len(t, rules, 1)
	assert.Equal(t, []string{"batch"}, rules[0].APIGroups)
	assert.Equal(t, []string{"cronjobs"}, rules[0].Resources)
}

package rbacgen

import (
	"context"
	"testing"

	"github.com/krateo-platformops/composition-dynamic-controller/internal/chartinspector"
	"github.com/krateo-platformops/composition-dynamic-controller/internal/tools/rbac"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

func genVerbs(t *testing.T, res []chartinspector.Resource) *rbac.RBAC {
	t.Helper()
	mi := new(MockChartInspector)
	g := NewRBACGen("sa", "sa-ns", mi).WithBaseName("base")
	mi.On("Resources", mock.Anything, mock.Anything).Return(res, nil)
	policy, err := g.Generate(context.Background(), Parameters{CompositionName: "c", CompositionNamespace: "c-ns"})
	assert.NoError(t, err)
	assert.NotNil(t, policy)
	return policy
}

func TestRBACGen_CollectionReadGetsReadVerbs(t *testing.T) {
	p := genVerbs(t, []chartinspector.Resource{
		{Version: "v1", Resource: "nodes", Verbs: []string{"get", "list", "watch"}}, // collection lookup
	})
	assert.Len(t, p.ClusterRole.Rules, 1)
	assert.Equal(t, []string{"get", "list", "watch"}, p.ClusterRole.Rules[0].Verbs)
	assert.Equal(t, []string{"nodes"}, p.ClusterRole.Rules[0].Resources)
}

func TestRBACGen_NamedStaysWildcard(t *testing.T) {
	p := genVerbs(t, []chartinspector.Resource{
		{Group: "apps", Version: "v1", Resource: "deployments", Namespace: "ns", Name: "d", Verbs: []string{"*"}},
	})
	assert.Equal(t, []string{"*"}, p.Namespaced["ns"].Role.Rules[0].Verbs)
}

// Same GVR seen as a named observation (["*"]) and a collection read (get/list/watch) unions to ["*"].
func TestRBACGen_UnionCollapsesToWildcard(t *testing.T) {
	p := genVerbs(t, []chartinspector.Resource{
		{Version: "v1", Resource: "services", Namespace: "ns", Verbs: []string{"get", "list", "watch"}},
		{Version: "v1", Resource: "services", Namespace: "ns", Name: "svc", Verbs: []string{"*"}},
	})
	assert.Len(t, p.Namespaced["ns"].Role.Rules, 1)
	assert.Equal(t, []string{"*"}, p.Namespaced["ns"].Role.Rules[0].Verbs)
}

// An older inspector sends no verbs -> default ["*"] (backward-compatible with today's behavior).
func TestRBACGen_AbsentVerbsDefaultWildcard(t *testing.T) {
	p := genVerbs(t, []chartinspector.Resource{
		{Group: "example.io", Version: "v1", Resource: "widgets", Name: "w"}, // no Verbs field
	})
	assert.Len(t, p.ClusterRole.Rules, 1)
	assert.Equal(t, []string{"*"}, p.ClusterRole.Rules[0].Verbs)
}

// A collection lookup of namespaces (Name=="") yields a cluster read rule but creates NO Namespace.
func TestRBACGen_CollectionNamespacesCreatesNoObject(t *testing.T) {
	p := genVerbs(t, []chartinspector.Resource{
		{Version: "v1", Resource: "namespaces", Verbs: []string{"get", "list", "watch"}},
	})
	assert.Len(t, p.ClusterRole.Rules, 1)
	assert.Equal(t, []string{"get", "list", "watch"}, p.ClusterRole.Rules[0].Verbs)
	assert.Empty(t, p.Namespaces)
}

package rbacgen

import (
	"context"
	"errors"
	"testing"

	"github.com/krateo-platformops/composition-dynamic-controller/internal/chartinspector"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type MockChartInspector struct {
	mock.Mock
}

func (m *MockChartInspector) Resources(ctx context.Context, params chartinspector.Parameters) ([]chartinspector.Resource, error) {
	args := m.Called(ctx, params)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]chartinspector.Resource), args.Error(1)
}

var _ chartinspector.ChartInspectorInterface = &MockChartInspector{}

func TestNewRBACGen(t *testing.T) {
	mockInspector := new(MockChartInspector)
	rbacGen := NewRBACGen("test-sa", "test-namespace", mockInspector)

	assert.NotNil(t, rbacGen)
	assert.Equal(t, "test-sa", rbacGen.saName)
	assert.Equal(t, "test-namespace", rbacGen.saNamespace)
	assert.Equal(t, mockInspector, rbacGen.chartInspector)
	assert.Empty(t, rbacGen.baseName)
}

func TestRBACGen_WithBaseName(t *testing.T) {
	mockInspector := new(MockChartInspector)
	rbacGen := NewRBACGen("test-sa", "test-namespace", mockInspector)

	result := rbacGen.WithBaseName("test-base")

	assert.Equal(t, rbacGen, result)
	assert.Equal(t, "test-base", rbacGen.baseName)
}

func TestRBACGen_Generate(t *testing.T) {
	t.Run("successfully generates RBAC policies with mixed resources", func(t *testing.T) {
		mockInspector := new(MockChartInspector)
		rbacGen := NewRBACGen("test-sa", "test-namespace", mockInspector).WithBaseName("test-base")

		params := Parameters{
			CompositionName:                "test-comp",
			CompositionNamespace:           "comp-ns",
			CompositionGVR:                 schema.GroupVersionResource{Group: "comp.group", Version: "v1", Resource: "compositions"},
			CompositionDefinitionName:      "test-compdef",
			CompositionDefinitionNamespace: "compdef-ns",
			CompositionDefintionGVR:        schema.GroupVersionResource{Group: "compdef.group", Version: "v1", Resource: "compositiondefinitions"},
		}

		mockResources := []chartinspector.Resource{
			{Group: "group1", Resource: "resource1", Name: "name1", Version: "v1"},
			{Group: "group2", Resource: "resource2", Name: "name2", Namespace: "namespace1", Version: "v1"},
			{Group: "", Resource: "resource3", Name: "name3", Version: "v1"},
			{Group: "", Resource: "resource4", Name: "name4", Namespace: "namespace1", Version: "v1"},
			{Group: "", Resource: "namespaces", Version: "v1", Name: "namespace1"},
		}

		expectedParams := chartinspector.Parameters{
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
		}

		mockInspector.On("Resources", mock.Anything, expectedParams).Return(mockResources, nil)

		policy, err := rbacGen.Generate(context.Background(), params)

		assert.NoError(t, err)
		assert.NotNil(t, policy)

		// Verify cluster role rules
		assert.Len(t, policy.ClusterRole.Rules, 3) // group1/resource1, core/resource3, core/namespaces

		// Verify namespaced resources
		assert.Len(t, policy.Namespaced, 1)
		assert.Contains(t, policy.Namespaced, "namespace1")
		assert.Len(t, policy.Namespaced["namespace1"].Role.Rules, 2) // group2/resource2, core/resource4

		// Verify namespace creation
		assert.Len(t, policy.Namespaces, 1)

		mockInspector.AssertExpectations(t)
	})

	t.Run("successfully generates RBAC policies with only cluster resources", func(t *testing.T) {
		mockInspector := new(MockChartInspector)
		rbacGen := NewRBACGen("test-sa", "test-namespace", mockInspector).WithBaseName("test-base")

		params := Parameters{
			CompositionName:      "test-comp",
			CompositionNamespace: "comp-ns",
			CompositionGVR:       schema.GroupVersionResource{Group: "comp.group", Version: "v1", Resource: "compositions"},
		}

		mockResources := []chartinspector.Resource{
			{Group: "group1", Resource: "resource1", Name: "name1", Version: "v1"},
			{Group: "group2", Resource: "resource2", Name: "name2", Version: "v1"},
		}

		expectedParams := chartinspector.Parameters{
			CompositionName:      params.CompositionName,
			CompositionNamespace: params.CompositionNamespace,
			CompositionGroup:     params.CompositionGVR.Group,
			CompositionVersion:   params.CompositionGVR.Version,
			CompositionResource:  params.CompositionGVR.Resource,
		}

		mockInspector.On("Resources", mock.Anything, expectedParams).Return(mockResources, nil)

		policy, err := rbacGen.Generate(context.Background(), params)

		assert.NoError(t, err)
		assert.NotNil(t, policy)
		assert.Len(t, policy.ClusterRole.Rules, 2)
		assert.Empty(t, policy.Namespaced)
		assert.Empty(t, policy.Namespaces)

		mockInspector.AssertExpectations(t)
	})

	t.Run("successfully generates RBAC policies with only namespaced resources", func(t *testing.T) {
		mockInspector := new(MockChartInspector)
		rbacGen := NewRBACGen("test-sa", "test-namespace", mockInspector).WithBaseName("test-base")

		params := Parameters{
			CompositionName:      "test-comp",
			CompositionNamespace: "comp-ns",
		}

		mockResources := []chartinspector.Resource{
			{Group: "group1", Resource: "resource1", Name: "name1", Namespace: "ns1", Version: "v1"},
			{Group: "group2", Resource: "resource2", Name: "name2", Namespace: "ns2", Version: "v1"},
			{Group: "group3", Resource: "resource3", Name: "name3", Namespace: "ns1", Version: "v1"},
		}

		expectedParams := chartinspector.Parameters{
			CompositionName:      params.CompositionName,
			CompositionNamespace: params.CompositionNamespace,
		}

		mockInspector.On("Resources", mock.Anything, expectedParams).Return(mockResources, nil)

		policy, err := rbacGen.Generate(context.Background(), params)

		assert.NoError(t, err)
		assert.NotNil(t, policy)
		assert.Empty(t, policy.ClusterRole.Rules)
		assert.Len(t, policy.Namespaced, 2)                   // ns1 and ns2
		assert.Len(t, policy.Namespaced["ns1"].Role.Rules, 2) // group1/resource1, group3/resource3
		assert.Len(t, policy.Namespaced["ns2"].Role.Rules, 1) // group2/resource2
		assert.Empty(t, policy.Namespaces)

		mockInspector.AssertExpectations(t)
	})

	t.Run("successfully generates RBAC policies with no resources", func(t *testing.T) {
		mockInspector := new(MockChartInspector)
		rbacGen := NewRBACGen("test-sa", "test-namespace", mockInspector).WithBaseName("test-base")

		params := Parameters{
			CompositionName:      "test-comp",
			CompositionNamespace: "comp-ns",
		}

		mockResources := []chartinspector.Resource{}

		expectedParams := chartinspector.Parameters{
			CompositionName:      params.CompositionName,
			CompositionNamespace: params.CompositionNamespace,
		}

		mockInspector.On("Resources", mock.Anything, expectedParams).Return(mockResources, nil)

		policy, err := rbacGen.Generate(context.Background(), params)

		assert.NoError(t, err)
		assert.NotNil(t, policy)
		assert.Empty(t, policy.ClusterRole.Rules)
		assert.Empty(t, policy.Namespaced)
		assert.Empty(t, policy.Namespaces)

		mockInspector.AssertExpectations(t)
	})

	t.Run("returns error when chartInspector fails", func(t *testing.T) {
		mockInspector := new(MockChartInspector)
		rbacGen := NewRBACGen("test-sa", "test-namespace", mockInspector).WithBaseName("test-base")

		params := Parameters{
			CompositionName:      "test-comp",
			CompositionNamespace: "comp-ns",
		}

		expectedParams := chartinspector.Parameters{
			CompositionName:      params.CompositionName,
			CompositionNamespace: params.CompositionNamespace,
		}

		mockInspector.On("Resources", mock.Anything, expectedParams).Return(nil, errors.New("some error"))

		policy, err := rbacGen.Generate(context.Background(), params)

		assert.Error(t, err)
		assert.Nil(t, policy)
		assert.EqualError(t, err, "getting resources from chart-inspector: some error")
		mockInspector.AssertExpectations(t)
	})

	t.Run("handles namespace resource creation correctly", func(t *testing.T) {
		mockInspector := new(MockChartInspector)
		rbacGen := NewRBACGen("test-sa", "test-namespace", mockInspector).WithBaseName("test-base")

		params := Parameters{
			CompositionName:      "test-comp",
			CompositionNamespace: "comp-ns",
		}

		mockResources := []chartinspector.Resource{
			{Group: "", Resource: "namespaces", Version: "v1", Name: "test-namespace"},
			{Group: "", Resource: "namespaces", Version: "v1", Name: "another-namespace"},
		}

		expectedParams := chartinspector.Parameters{
			CompositionName:      params.CompositionName,
			CompositionNamespace: params.CompositionNamespace,
		}

		mockInspector.On("Resources", mock.Anything, expectedParams).Return(mockResources, nil)

		policy, err := rbacGen.Generate(context.Background(), params)

		assert.NoError(t, err)
		assert.NotNil(t, policy)
		assert.Len(t, policy.ClusterRole.Rules, 1) // both namespace observations dedupe to one namespaces rule
		assert.Len(t, policy.Namespaces, 2)        // both named namespaces are still created

		mockInspector.AssertExpectations(t)
	})
}

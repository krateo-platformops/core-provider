package composition

import (
	"testing"

	"github.com/krateo-platformops/composition-dynamic-controller/internal/tools/processor"
	"github.com/krateo-platformops/unstructured-runtime/pkg/pluralizer"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type mockPluralizer struct {
	gvrMap map[schema.GroupVersionKind]schema.GroupVersionResource
	errMap map[schema.GroupVersionKind]error
}

func (m *mockPluralizer) GVKtoGVR(gvk schema.GroupVersionKind) (schema.GroupVersionResource, error) {
	if err, exists := m.errMap[gvk]; exists {
		return schema.GroupVersionResource{}, err
	}
	if gvr, exists := m.gvrMap[gvk]; exists {
		return gvr, nil
	}
	return schema.GroupVersionResource{}, nil
}

func newTestMapper() *meta.DefaultRESTMapper {
	// Define the versions the mapper should recognize
	gvks := []schema.GroupVersion{
		{Group: "apps", Version: "v1"},
		{Group: "rbac.authorization.k8s.io", Version: "v1"},
		{Group: "", Version: "v1"},
	}

	mapper := meta.NewDefaultRESTMapper(gvks)

	// Add Namespaced resources
	mapper.Add(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}, meta.RESTScopeNamespace)
	mapper.Add(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"}, meta.RESTScopeNamespace)
	mapper.Add(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Service"}, meta.RESTScopeNamespace)

	// Add Cluster-scoped resources
	mapper.Add(schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole"}, meta.RESTScopeRoot)
	mapper.Add(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Node"}, meta.RESTScopeRoot)

	return mapper
}

func TestPopulateManagedResources(t *testing.T) {
	tests := []struct {
		name        string
		pluralizer  pluralizer.PluralizerInterface
		resources   []processor.MinimalMetadata
		expected    []interface{}
		expectError bool
	}{
		{
			name: "empty resources",
			pluralizer: &mockPluralizer{
				gvrMap: make(map[schema.GroupVersionKind]schema.GroupVersionResource),
				errMap: make(map[schema.GroupVersionKind]error),
			},
			resources: []processor.MinimalMetadata{},
			expected:  []interface{}{},
		},
		{
			name: "namespaced resource with group",
			pluralizer: &mockPluralizer{
				gvrMap: map[schema.GroupVersionKind]schema.GroupVersionResource{
					{Group: "apps", Version: "v1", Kind: "Deployment"}: {Group: "apps", Version: "v1", Resource: "deployments"},
				},
				errMap: make(map[schema.GroupVersionKind]error),
			},
			resources: []processor.MinimalMetadata{
				{
					APIVersion: "apps/v1",
					Kind:       "Deployment",
					Metadata: processor.Metadata{
						Name:      "test-deployment",
						Namespace: "default",
					},
				},
			},
			expected: []interface{}{
				ManagedResource{
					APIVersion: "apps/v1",
					Resource:   "deployments",
					Name:       "test-deployment",
					Namespace:  "default",
					Path:       "/apis/apps/v1/namespaces/default/deployments/test-deployment",
				},
			},
		},
		{
			name: "cluster scoped resource with group",
			pluralizer: &mockPluralizer{
				gvrMap: map[schema.GroupVersionKind]schema.GroupVersionResource{
					{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole"}: {Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"},
				},
				errMap: make(map[schema.GroupVersionKind]error),
			},
			resources: []processor.MinimalMetadata{
				{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole", Metadata: processor.Metadata{Name: "test-clusterrole", Namespace: ""}},
			},
			expected: []interface{}{
				ManagedResource{
					APIVersion: "rbac.authorization.k8s.io/v1",
					Resource:   "clusterroles",
					Name:       "test-clusterrole",
					Namespace:  "",
					Path:       "/apis/rbac.authorization.k8s.io/v1/clusterroles/test-clusterrole",
				},
			},
		},
		{
			name: "core group namespaced resource",
			pluralizer: &mockPluralizer{
				gvrMap: map[schema.GroupVersionKind]schema.GroupVersionResource{
					{Group: "", Version: "v1", Kind: "Pod"}: {Group: "", Version: "v1", Resource: "pods"},
				},
				errMap: make(map[schema.GroupVersionKind]error),
			},
			resources: []processor.MinimalMetadata{
				{APIVersion: "v1", Kind: "Pod", Metadata: processor.Metadata{Name: "test-pod", Namespace: "default"}},
			},
			expected: []interface{}{
				ManagedResource{
					APIVersion: "v1",
					Resource:   "pods",
					Name:       "test-pod",
					Namespace:  "default",
					Path:       "/api/v1/namespaces/default/pods/test-pod",
				},
			},
		},
		{
			name: "core group cluster scoped resource",
			pluralizer: &mockPluralizer{
				gvrMap: map[schema.GroupVersionKind]schema.GroupVersionResource{
					{Group: "", Version: "v1", Kind: "Node"}: {Group: "", Version: "v1", Resource: "nodes"},
				},
				errMap: make(map[schema.GroupVersionKind]error),
			},
			resources: []processor.MinimalMetadata{
				{APIVersion: "v1", Kind: "Node", Metadata: processor.Metadata{Name: "test-node", Namespace: ""}},
			},
			expected: []interface{}{
				ManagedResource{
					APIVersion: "v1",
					Resource:   "nodes",
					Name:       "test-node",
					Namespace:  "",
					Path:       "/api/v1/nodes/test-node",
				},
			},
		},
		{
			name: "multiple resources",
			pluralizer: &mockPluralizer{
				gvrMap: map[schema.GroupVersionKind]schema.GroupVersionResource{
					{Group: "apps", Version: "v1", Kind: "Deployment"}: {Group: "apps", Version: "v1", Resource: "deployments"},
					{Group: "", Version: "v1", Kind: "Service"}:        {Group: "", Version: "v1", Resource: "services"},
				},
				errMap: make(map[schema.GroupVersionKind]error),
			},
			resources: []processor.MinimalMetadata{
				{APIVersion: "apps/v1", Kind: "Deployment", Metadata: processor.Metadata{Name: "test-deployment", Namespace: "default"}},
				{APIVersion: "v1", Kind: "Service", Metadata: processor.Metadata{Name: "test-service", Namespace: "default"}},
			},
			expected: []interface{}{
				ManagedResource{
					APIVersion: "apps/v1",
					Resource:   "deployments",
					Name:       "test-deployment",
					Namespace:  "default",
					Path:       "/apis/apps/v1/namespaces/default/deployments/test-deployment",
				},
				ManagedResource{
					APIVersion: "v1",
					Resource:   "services",
					Name:       "test-service",
					Namespace:  "default",
					Path:       "/api/v1/namespaces/default/services/test-service",
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &handler{
				pluralizer: tt.pluralizer,
				mapper:     newTestMapper(),
			}
			result, err := h.populateManagedResources(tt.resources)

			if tt.expectError && err == nil {
				t.Fatal("expected error but got none")
			}
			if !tt.expectError && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if len(result) != len(tt.expected) {
				t.Fatalf("expected %d managed resources, got %d", len(tt.expected), len(result))
			}

			for i, expected := range tt.expected {
				expectedRes := expected.(ManagedResource)
				// populateManagedResources now stores JSON-native maps (so status.managed is
				// DeepCopyJSONValue-safe); the assertion below also guards that shape. Convert
				// back to compare field-by-field.
				actualMap, ok := result[i].(map[string]interface{})
				if !ok {
					t.Fatalf("managed[%d] must be a JSON-native map for DeepCopy safety, got %T", i, result[i])
				}
				var actualRes ManagedResource
				if err := runtime.DefaultUnstructuredConverter.FromUnstructured(actualMap, &actualRes); err != nil {
					t.Fatalf("converting managed[%d] back: %v", i, err)
				}

				if expectedRes.APIVersion != actualRes.APIVersion {
					t.Errorf("APIVersion mismatch at index %d: expected %s, got %s", i, expectedRes.APIVersion, actualRes.APIVersion)
				}
				if expectedRes.Resource != actualRes.Resource {
					t.Errorf("Resource mismatch at index %d: expected %s, got %s", i, expectedRes.Resource, actualRes.Resource)
				}
				if expectedRes.Name != actualRes.Name {
					t.Errorf("Name mismatch at index %d: expected %s, got %s", i, expectedRes.Name, actualRes.Name)
				}
				if expectedRes.Namespace != actualRes.Namespace {
					t.Errorf("Namespace mismatch at index %d: expected %s, got %s", i, expectedRes.Namespace, actualRes.Namespace)
				}
				if expectedRes.Path != actualRes.Path {
					t.Errorf("Path mismatch at index %d: expected %s, got %s", i, expectedRes.Path, actualRes.Path)
				}
			}
		})
	}
}

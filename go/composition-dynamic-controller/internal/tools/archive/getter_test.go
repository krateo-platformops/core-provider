//go:build integration
// +build integration

package archive

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/gobuffalo/flect"

	"github.com/krateo-platformops/plumbing/e2e"
	xenv "github.com/krateo-platformops/plumbing/env"
	"github.com/krateo-platformops/unstructured-runtime/pkg/pluralizer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apis "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/scheme"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	cacheddiscovery "k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"sigs.k8s.io/e2e-framework/klient/decoder"
	"sigs.k8s.io/e2e-framework/klient/k8s"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/klient/wait/conditions"
	"sigs.k8s.io/e2e-framework/pkg/env"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/envfuncs"
	"sigs.k8s.io/e2e-framework/pkg/features"
	"sigs.k8s.io/e2e-framework/support/kind"
)

var (
	testenv     env.Environment
	clusterName string
	namespace   string
)

const (
	testdataPath = "../../../testdata"
)

type FakePluralizer struct{}

var _ pluralizer.PluralizerInterface = &FakePluralizer{}

func (p FakePluralizer) GVKtoGVR(gvk schema.GroupVersionKind) (schema.GroupVersionResource, error) {
	return schema.GroupVersionResource{
		Group:    gvk.Group,
		Version:  gvk.Version,
		Resource: flect.Pluralize(strings.ToLower(gvk.Kind)),
	}, nil
}

func TestMain(m *testing.M) {
	xenv.SetTestMode(true)

	// Every cluster-starting suite pins KUBECONFIG first: kind writes to AND switches
	// ~/.kube/config otherwise, and DestroyCluster then removes that context again, leaving the
	// developer pointed at nothing. Invisible on CI, where there is no kubeconfig worth
	// clobbering. See e2e.PinKubeconfig.
	restore := e2e.PinKubeconfig("cdc-archive")

	namespace = "demo-system"
	altNamespace := "krateo-system"
	clusterName = "krateo"
	testenv = env.New()

	testenv.Setup(
		envfuncs.CreateCluster(kind.NewProvider(), clusterName),
		e2e.CreateNamespace(namespace),
		e2e.CreateNamespace(altNamespace),

		func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
			r, err := resources.New(cfg.Client().RESTConfig())
			if err != nil {
				return ctx, err
			}

			r.WithNamespace(namespace)

			return ctx, nil
		},
	).Finish(
		envfuncs.DeleteNamespace(namespace),
		envfuncs.DestroyCluster(clusterName),
	)

	code := testenv.Run(m)
	restore() // not deferred: os.Exit does not run defers
	os.Exit(code)
}

// Enhanced test with better coverage
func TestGetter(t *testing.T) {
	os.Setenv("DEBUG", "1")
	tests := []struct {
		name                  string
		compositionDefinition string
		composition           string
		expectError           bool
		expectedURL           string
		expectedRepo          string
		expectedVersion       string
		skipCondition         func(*testing.T) bool
	}{
		{
			name:                  "valid_fireworksapp",
			compositionDefinition: "fireworksapp.yaml",
			composition:           "fireworksapp.yaml",
			expectError:           false,
		},
		// Add more test cases here as you have more test data
	}

	f := features.New("Setup").
		Setup(e2e.Logger("test")).
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			return setupTestEnvironment(ctx, t, cfg)
		}).
		Assess("Testing CompositionDefinition Getter", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			return testGetterFunctionality(ctx, t, c, tests)
		}).
		Assess("Testing Static Getter", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			return testStaticGetter(ctx, t, c)
		}).
		Assess("Testing Error Conditions", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			return testErrorConditions(ctx, t, c)
		}).
		Assess("Testing Edge Cases", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			return testEdgeCases(ctx, t, c)
		}).Feature()

	testenv.Test(t, f)
}

func setupTestEnvironment(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
	r, err := resources.New(cfg.Client().RESTConfig())
	require.NoError(t, err)

	// Setup CRDs
	err = decoder.DecodeEachFile(
		ctx, os.DirFS(filepath.Join(testdataPath, "crds", "finops")), "*.yaml",
		decoder.CreateIgnoreAlreadyExists(r),
	)
	if err != nil {
		t.Logf("Error decoding finops CRDs: %v", err)
		// Don't fail if finops CRDs don't exist
	}

	err = decoder.DecodeEachFile(
		ctx, os.DirFS(filepath.Join(testdataPath, "crds", "core")), "*.yaml",
		decoder.CreateIgnoreAlreadyExists(r),
	)
	require.NoError(t, err, "Core CRDs should be available")

	// Wait for CRDs to be ready
	resli, err := decoder.DecodeAllFiles(ctx, os.DirFS(filepath.Join(testdataPath, "crds", "core")), "*.yaml")
	require.NoError(t, err)

	ress := unstructured.UnstructuredList{}
	for _, res := range resli {
		res, err := runtime.DefaultUnstructuredConverter.ToUnstructured(res)
		require.NoError(t, err)
		ress.Items = append(ress.Items, unstructured.Unstructured{Object: res})
	}

	err = wait.For(
		conditions.New(r).ResourcesFound(&ress),
		wait.WithInterval(100*time.Millisecond),
		wait.WithTimeout(30*time.Second),
	)
	require.NoError(t, err, "CRDs should be ready")

	apis.AddToScheme(r.GetScheme())

	// Setup CompositionDefinitions
	err = decoder.DecodeEachFile(
		ctx, os.DirFS(filepath.Join(testdataPath, "compositiondefinitions")), "*.yaml",
		decoder.CreateIgnoreAlreadyExists(r),
	)
	require.NoError(t, err, "CompositionDefinitions should be created")

	// Setup Compositions
	err = decoder.DecodeEachFile(
		ctx, os.DirFS(filepath.Join(testdataPath, "compositions")), "*.yaml",
		decoder.CreateIgnoreAlreadyExists(r),
	)
	require.NoError(t, err, "Compositions should be created")

	// Patch status for CompositionDefinitions
	resli, err = decoder.DecodeAllFiles(ctx, os.DirFS(filepath.Join(testdataPath, "compositiondefinitions")), "*.yaml")
	require.NoError(t, err)

	for _, res := range resli {
		uns, err := runtime.DefaultUnstructuredConverter.ToUnstructured(res)
		require.NoError(t, err)

		apiVersion, ok, err := unstructured.NestedString(uns, "status", "apiVersion")
		if !ok || err != nil {
			t.Logf("No status.apiVersion found for %s", res.GetName())
			continue
		}
		kind, ok, err := unstructured.NestedString(uns, "status", "kind")
		if !ok || err != nil {
			t.Logf("No status.kind found for %s", res.GetName())
			continue
		}

		err = r.PatchStatus(ctx, res, k8s.Patch{
			PatchType: types.MergePatchType,
			Data:      []byte(fmt.Sprintf(`{"status": {"apiVersion": "%s", "kind": "%s"}}`, apiVersion, kind)),
		})
		require.NoError(t, err, "Status patch should succeed")
	}

	r.WithNamespace(namespace)
	return ctx
}

func testGetterFunctionality(ctx context.Context, t *testing.T, c *envconf.Config, tests []struct {
	name                  string
	compositionDefinition string
	composition           string
	expectError           bool
	expectedURL           string
	expectedRepo          string
	expectedVersion       string
	skipCondition         func(*testing.T) bool
}) context.Context {
	r, err := resources.New(c.Client().RESTConfig())
	require.NoError(t, err)
	r.WithNamespace(namespace)

	apis.AddToScheme(r.GetScheme())

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.skipCondition != nil && tt.skipCondition(t) {
				t.Skip("Skipping test due to condition")
				return
			}

			// Load composition from test data
			composition := &unstructured.Unstructured{}
			err := decoder.DecodeFile(os.DirFS(filepath.Join(testdataPath, "compositions")), tt.composition, composition)
			require.NoError(t, err, "Should be able to decode composition")

			// Get the actual composition from cluster
			uns, err := getUnstructured(c.Client().RESTConfig(), getUnstructuredOptions{
				gvk:       schema.FromAPIVersionAndKind(composition.GetAPIVersion(), composition.GetKind()),
				name:      composition.GetName(),
				namespace: composition.GetNamespace(),
			})
			require.NoError(t, err, "Should be able to get composition from cluster")

			// Test dynamic getter
			pluralizer := FakePluralizer{}
			gt, err := Dynamic(c.Client().RESTConfig(), pluralizer)
			require.NoError(t, err, "Should be able to create dynamic getter")

			// Test the getter
			info, err := gt.Get(uns)

			if tt.expectError {
				assert.Error(t, err, "Should return an error")
				return
			}

			require.NoError(t, err, "Should not return an error")
			require.NotNil(t, info, "Info should not be nil")

			// Validate the returned info
			assert.NotEmpty(t, info.URL, "URL should not be empty")

			if tt.expectedURL != "" {
				assert.Equal(t, tt.expectedURL, info.URL, "URL should match expected")
			}
			if tt.expectedRepo != "" {
				assert.Equal(t, tt.expectedRepo, info.Repo, "Repo should match expected")
			}
			if tt.expectedVersion != "" {
				assert.Equal(t, tt.expectedVersion, info.Version, "Version should match expected")
			}

			// Test URL type detection
			if strings.HasPrefix(info.URL, "oci://") {
				assert.True(t, info.IsOCI(), "Should detect OCI URLs")
				assert.False(t, info.IsTGZ(), "OCI URLs are not TGZ")
				assert.False(t, info.IsHTTP(), "OCI URLs are not HTTP")
			} else if strings.HasPrefix(info.URL, "http") {
				assert.True(t, info.IsHTTP(), "Should detect HTTP URLs")
				assert.False(t, info.IsOCI(), "HTTP URLs are not OCI")
				if strings.HasSuffix(info.URL, ".tgz") {
					assert.True(t, info.IsTGZ(), "Should detect TGZ files")
				}
			}

			// Validate CompositionDefinitionInfo
			if info.CompositionDefinitionInfo != nil {
				assert.NotEmpty(t, info.CompositionDefinitionInfo.Name, "CompositionDefinition name should not be empty")
				assert.NotEmpty(t, info.CompositionDefinitionInfo.Namespace, "CompositionDefinition namespace should not be empty")
				assert.NotEmpty(t, info.CompositionDefinitionInfo.GVR.Resource, "CompositionDefinition GVR resource should not be empty")
			}

			t.Logf("Successfully tested getter for %s", tt.name)
			spew.Dump(info)
		})
	}

	return ctx
}

func testStaticGetter(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
	testCases := []struct {
		name     string
		chartURL string
	}{
		{
			name:     "local_tgz_file",
			chartURL: "/path/to/chart.tgz",
		},
		{
			name:     "http_url",
			chartURL: "https://example.com/chart.tgz",
		},
		{
			name:     "oci_url",
			chartURL: "oci://registry.example.com/charts/my-chart",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			getter := Static(tc.chartURL)
			require.NotNil(t, getter, "Static getter should not be nil")

			// Test with empty unstructured (static getter ignores input)
			info, err := getter.Get(&unstructured.Unstructured{})
			require.NoError(t, err, "Static getter should not return error")
			require.NotNil(t, info, "Info should not be nil")

			assert.Equal(t, tc.chartURL, info.URL, "URL should match input")
			assert.Empty(t, info.Version, "Static getter should not have version")
			assert.Empty(t, info.Repo, "Static getter should not have repo")
			assert.Nil(t, info.Auth, "Static getter should not have registry auth")
			assert.Nil(t, info.CompositionDefinitionInfo, "Static getter should not have composition definition info")

			// Test URL type detection
			if strings.HasPrefix(tc.chartURL, "oci://") {
				assert.True(t, info.IsOCI(), "Should detect OCI URLs")
			} else if strings.HasPrefix(tc.chartURL, "http") {
				assert.True(t, info.IsHTTP(), "Should detect HTTP URLs")
			}
			if strings.HasSuffix(tc.chartURL, ".tgz") {
				assert.True(t, info.IsTGZ(), "Should detect TGZ files")
			}
		})
	}

	return ctx
}

func testErrorConditions(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
	pluralizer := FakePluralizer{}
	getter, err := Dynamic(c.Client().RESTConfig(), pluralizer)
	require.NoError(t, err)

	testCases := []struct {
		name        string
		composition *unstructured.Unstructured
		expectError bool
		errorType   string
	}{
		{
			name: "missing_labels",
			composition: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "composition.krateo.io/v1alpha1",
					"kind":       "NonExistentComposition",
					"metadata": map[string]interface{}{
						"name":      "test-composition",
						"namespace": namespace,
						// No labels - should trigger fallback search
					},
				},
			},
			expectError: true,
			errorType:   "not_found",
		},
		{
			name: "invalid_composition_definition_reference",
			composition: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "composition.krateo.io/v1alpha1",
					"kind":       "TestComposition",
					"metadata": map[string]interface{}{
						"name":      "test-composition",
						"namespace": namespace,
						"labels": map[string]interface{}{
							"krateo.io/composition-definition-name":      "non-existent-def",
							"krateo.io/composition-definition-namespace": namespace,
							"krateo.io/composition-definition-group":     "core.krateo.io",
							"krateo.io/composition-definition-version":   "v1alpha1",
							"krateo.io/composition-definition-resource":  "compositiondefinitions",
						},
					},
				},
			},
			expectError: true,
			errorType:   "not_found",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			info, err := getter.Get(tc.composition)

			if tc.expectError {
				assert.Error(t, err, "Should return an error for %s", tc.name)
				assert.Nil(t, info, "Info should be nil on error")
				t.Logf("Expected error for %s: %v", tc.name, err)
			} else {
				assert.NoError(t, err, "Should not return an error for %s", tc.name)
				assert.NotNil(t, info, "Info should not be nil")
			}
		})
	}

	return ctx
}

func testEdgeCases(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
	pluralizer := FakePluralizer{}
	getter, err := Dynamic(c.Client().RESTConfig(), pluralizer)
	require.NoError(t, err)

	// Test with nil input
	t.Run("nil_input", func(t *testing.T) {
		info, err := getter.Get(nil)
		assert.Error(t, err, "Should return error for nil input")
		assert.Nil(t, info, "Info should be nil for nil input")
	})

	// Test with empty unstructured
	t.Run("empty_unstructured", func(t *testing.T) {
		empty := &unstructured.Unstructured{}
		info, err := getter.Get(empty)
		assert.Error(t, err, "Should return error for empty unstructured")
		assert.Nil(t, info, "Info should be nil for empty unstructured")
	})

	// Test URL detection edge cases
	t.Run("url_detection", func(t *testing.T) {
		testCases := []struct {
			url    string
			isOCI  bool
			isTGZ  bool
			isHTTP bool
		}{
			{"", false, false, false},
			{"oci://", true, false, false},
			{"http://example.com/chart.tgz", false, true, true},
			{"https://example.com/chart.tgz", false, true, true},
			{"ftp://example.com/chart.tgz", false, true, false},
			{"file:///path/to/chart.tgz", false, true, false},
			{"/local/path/chart.tgz", false, true, false},
			{"chart.tar.gz", false, false, false}, // not .tgz
		}

		for _, tc := range testCases {
			info := &Info{URL: tc.url}
			assert.Equal(t, tc.isOCI, info.IsOCI(), "IsOCI mismatch for %s", tc.url)
			assert.Equal(t, tc.isTGZ, info.IsTGZ(), "IsTGZ mismatch for %s", tc.url)
			assert.Equal(t, tc.isHTTP, info.IsHTTP(), "IsHTTP mismatch for %s", tc.url)
		}
	})

	return ctx
}

type getUnstructuredOptions struct {
	gvk       schema.GroupVersionKind
	name      string
	namespace string
}

func getUnstructured(rc *rest.Config, opts getUnstructuredOptions) (*unstructured.Unstructured, error) {
	dynamicClient, err := dynamic.NewForConfig(rc)
	if err != nil {
		return nil, err
	}

	discoveryClient, err := discovery.NewDiscoveryClientForConfig(rc)
	if err != nil {
		return nil, err
	}

	mapper := restmapper.NewDeferredDiscoveryRESTMapper(
		cacheddiscovery.NewMemCacheClient(discoveryClient),
	)

	restMapping, err := mapper.RESTMapping(opts.gvk.GroupKind(), opts.gvk.Version)
	if err != nil {
		return nil, err
	}

	var ri dynamic.ResourceInterface
	if restMapping.Scope.Name() == meta.RESTScopeNameRoot {
		ri = dynamicClient.Resource(restMapping.Resource)
	} else {
		ri = dynamicClient.Resource(restMapping.Resource).
			Namespace(opts.namespace)
	}

	return ri.Get(context.TODO(), opts.name, metav1.GetOptions{})
}

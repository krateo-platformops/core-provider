//go:build integration
// +build integration

package deploy

import (
	"context"
	"os"
	"testing"

	rtv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"

	"github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/plumbing/e2e"
	xenv "github.com/krateo-platformops/plumbing/env"
	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
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

func TestMain(m *testing.M) {
	xenv.SetTestMode(true)

	// Every cluster-starting suite pins KUBECONFIG first: kind writes to AND switches
	// ~/.kube/config otherwise, and DestroyCluster then removes that context again, leaving the
	// developer pointed at nothing. Invisible on CI, where there is no kubeconfig worth
	// clobbering. See e2e.PinKubeconfig.
	restore := e2e.PinKubeconfig("cp-deploy")

	namespace = "test-deploy"
	clusterName = "krateo"
	testenv = env.New()

	testenv.Setup(
		envfuncs.CreateCluster(kind.NewProvider(), clusterName),
		e2e.CreateNamespace(namespace),

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

func TestDeploy(t *testing.T) {
	os.Setenv("DEBUG", "1")

	f := features.New("Setup").
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			return ctx
		}).Assess("Deploy", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		cli, err := client.New(cfg.Client().RESTConfig(), client.Options{})
		if err != nil {
			t.Fatalf("failed to create client: %v", err)
			return ctx
		}
		dyn := dynamic.NewForConfigOrDie(cfg.Client().RESTConfig())
		opts := DeployOptions{
			DiscoveryClient:        memory.NewMemCacheClient(discovery.NewDiscoveryClientForConfigOrDie(cfg.Client().RESTConfig())),
			RBACFolderPath:         "testdata",
			DeploymentTemplatePath: "testdata/deploy.yaml",
			ConfigmapTemplatePath:  "testdata/cm.yaml",
			JsonSchemaTemplatePath: "testdata/configmap_jsonschema.yaml",
			JsonSchemaBytes:        []byte(`{"type": "object", "properties": {"key": {"type": "string"}}}`),
			ServiceTemplatePath:    "testdata/service.yaml",
			KubeClient:             cli,
			DynClient:              dyn,
			Namespace:              namespace,
			GVR: schema.GroupVersionResource{
				Group:    "compositions.krateo.io",
				Version:  "v1alpha1",
				Resource: "fireworksapps",
			},
			Spec: &v1alpha1.ChartInfo{
				Version: "1.1.10",
				Repo:    "fireworks-app",
				Url:     "https://charts.krateo.io",
			},
		}

		dig, err := Deploy(context.Background(), cli, opts)
		assert.NoError(t, err)
		assert.NotNil(t, dig)

		return ctx
	}).Assess("Undeploy", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		cli, err := client.New(cfg.Client().RESTConfig(), client.Options{})
		if err != nil {
			t.Fatalf("failed to create client: %v", err)
			return ctx
		}

		opts := UndeployOptions{
			DiscoveryClient:        memory.NewMemCacheClient(discovery.NewDiscoveryClientForConfigOrDie(cfg.Client().RESTConfig())),
			RBACFolderPath:         "testdata",
			DeploymentTemplatePath: "testdata/deploy.yaml",
			ConfigmapTemplatePath:  "testdata/cm.yaml",
			JsonSchemaTemplatePath: "testdata/configmap_jsonschema.yaml",
			ServiceTemplatePath:    "testdata/service.yaml",
			JsonSchemaBytes:        []byte(`{"type": "object", "properties": {"key": {"type": "string"}}}`),
			KubeClient:             cli,
			Namespace:              namespace,
			Spec: &v1alpha1.ChartInfo{
				InsecureSkipVerifyTLS: true,
				Version:               "1.1.10",
				Repo:                  "fireworks-app",
				Url:                   "https://charts.krateo.io",
				Credentials: &v1alpha1.Credentials{
					Username: "admin",
					PasswordRef: rtv1.SecretKeySelector{
						Key: "password",
						Reference: rtv1.Reference{
							Name:      "test",
							Namespace: "default",
						},
					},
				},
			},
			SkipCRD:       true,
			DynamicClient: dynamic.NewForConfigOrDie(cfg.Client().RESTConfig()),
			GVR: schema.GroupVersionResource{
				Group:    "compositions.krateo.io",
				Version:  "v1alpha1",
				Resource: "fireworksapps",
			},
		}

		err = Undeploy(context.Background(), opts.KubeClient, opts)
		assert.NoError(t, err)

		return ctx
	},
	).Feature()

	testenv.Test(t, f)
}
func TestLookup(t *testing.T) {
	os.Setenv("DEBUG", "1")

	f := features.New("Lookup").
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			return ctx
		}).Assess("Lookup Deployment State", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		cli, err := client.New(cfg.Client().RESTConfig(), client.Options{})
		if err != nil {
			t.Fatalf("failed to create client: %v", err)
			return ctx
		}
		dyn := dynamic.NewForConfigOrDie(cfg.Client().RESTConfig())

		opts := DeployOptions{
			DiscoveryClient:        memory.NewMemCacheClient(discovery.NewDiscoveryClientForConfigOrDie(cfg.Client().RESTConfig())),
			RBACFolderPath:         "testdata",
			DeploymentTemplatePath: "testdata/deploy.yaml",
			ConfigmapTemplatePath:  "testdata/cm.yaml",
			JsonSchemaTemplatePath: "testdata/configmap_jsonschema.yaml",
			ServiceTemplatePath:    "testdata/service.yaml",
			JsonSchemaBytes:        []byte(`{"type": "object", "properties": {"key": {"type": "string"}}}`),
			KubeClient:             cli,
			DynClient:              dyn,
			Namespace:              namespace,
			GVR: schema.GroupVersionResource{
				Group:    "compositions.krateo.io",
				Version:  "v1alpha1",
				Resource: "fireworksapps",
			},
			Spec: &v1alpha1.ChartInfo{
				InsecureSkipVerifyTLS: true,
				Version:               "1.1.10",
				Repo:                  "fireworks-app",
				Url:                   "https://charts.krateo.io",
				Credentials: &v1alpha1.Credentials{
					Username: "admin",
					PasswordRef: rtv1.SecretKeySelector{
						Key: "password",
						Reference: rtv1.Reference{
							Name:      "test-secret",
							Namespace: "default",
						},
					},
				},
			},
		}

		// Deploy first to ensure resources exist for lookup
		ddig, err := Deploy(context.Background(), cli, opts)
		assert.NoError(t, err)
		assert.NotEmpty(t, ddig)

		// Perform the lookup
		digest, err := Lookup(context.Background(), cli, opts)
		assert.NoError(t, err)
		assert.NotEmpty(t, digest)

		assert.Equal(t, ddig, digest)

		return ctx
	}).Feature()

	testenv.Test(t, f)
}

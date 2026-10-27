//go:build integration
// +build integration

package getter

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/krateo-platformops/plumbing/e2e"
	xenv "github.com/krateo-platformops/plumbing/env"
	rtv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

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
	testdataPath = "../../testdata"
)

func TestMain(m *testing.M) {
	xenv.SetTestMode(true)

	// Every cluster-starting suite pins KUBECONFIG first: kind writes to AND switches
	// ~/.kube/config otherwise, and DestroyCluster then removes that context again, leaving the
	// developer pointed at nothing. Invisible on CI, where there is no kubeconfig worth
	// clobbering. See e2e.PinKubeconfig.
	restore := e2e.PinKubeconfig("ci-getter")

	namespace = "demo-system"
	altNamespace := "krateo-system"
	clusterName = "krateo"
	testenv = env.New()

	// kubeconfig := filepath.Join(os.Getenv("HOME"), ".kube", "config")

	testenv.Setup(
		envfuncs.CreateCluster(kind.NewProvider(), clusterName),
		e2e.CreateNamespace(namespace),
		e2e.CreateNamespace(altNamespace),

		// func(ctx context.Context, c *envconf.Config) (context.Context, error) {

		// 	// update envconfig  with kubeconfig
		// 	c.WithKubeconfigFile(kubeconfig)

		// 	return ctx, nil
		// },

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

func TestGetCompositionDefinition(t *testing.T) {
	os.Setenv("DEBUG", "1")

	f := features.New("Setup").
		Setup(e2e.Logger("test")).
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			r, err := resources.New(cfg.Client().RESTConfig())
			if err != nil {
				t.Fail()
			}

			// apis.AddToScheme(r.GetScheme())

			err = decoder.DecodeEachFile(
				ctx, os.DirFS(filepath.Join(testdataPath, "crds")), "*.yaml",
				decoder.CreateIgnoreAlreadyExists(r),
			)
			if err != nil {
				t.Log("Error decoding CRDs: ", err)
				t.Fail()
			}

			resli, err := decoder.DecodeAllFiles(ctx, os.DirFS(filepath.Join(testdataPath, "crds")), "*.yaml")
			if err != nil {
				t.Log("Error decoding CRDs: ", err)
				t.Fail()
			}

			ress := unstructured.UnstructuredList{}
			for _, res := range resli {
				if u, ok := res.(*unstructured.Unstructured); ok {
					ress.Items = append(ress.Items, *u)
				} else {
					t.Log("Error casting resource to unstructured.Unstructured")
					t.Fail()
				}
			}
			err = wait.For(
				conditions.New(r).ResourcesFound(&ress),
				wait.WithInterval(100*time.Millisecond),
			)
			if err != nil {
				t.Log("Error waiting for CRD: ", err)
				t.Fail()
			}

			err = decoder.DecodeEachFile(
				ctx, os.DirFS(filepath.Join(testdataPath, "compositiondefinitions")), "*.yaml",
				decoder.CreateIgnoreAlreadyExists(r),
			)
			if err != nil {
				t.Log("Error decoding CompositionDefinitions: ", err)
				t.Fail()
			}

			r.WithNamespace(namespace)
			return ctx
		}).Assess("Testing CompositionDefinitions", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		r, err := resources.New(c.Client().RESTConfig())
		if err != nil {
			t.Fail()
		}
		r.WithNamespace(namespace)

		// apis.AddToScheme(r.GetScheme())

		objs, err := decoder.DecodeAllFiles(ctx, os.DirFS(filepath.Join(testdataPath, "compositiondefinitions")), "*.yaml")
		if err != nil {
			t.Log("Error decoding CompositionDefinitions: ", err)
			t.Fail()
		}

		dynamic := dynamic.NewForConfigOrDie(c.Client().RESTConfig())
		cli := NewClient(dynamic)

		for _, obj := range objs {
			comp := k8s.Object(obj)
			err = r.Get(ctx, comp.GetName(), comp.GetNamespace(), comp)
			if err != nil {
				t.Log("Error getting composition definition: ", err)
				t.Fail()
			}
			t.Log("Composition Definition ID", comp.GetUID())

			res, err := cli.GetCompositionDefinition(string(comp.GetUID()), comp.GetNamespace())
			if err != nil {
				t.Log("Error getting composition definition by ID: ", err)
				t.Fail()
			}

			if res.GetUID() != comp.GetUID() {
				t.Log("Composition Definition ID mismatch")
				t.Fail()
			}
		}

		return ctx
	}).Assess("Testing Not Found", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		dynamic := dynamic.NewForConfigOrDie(c.Client().RESTConfig())
		cli := NewClient(dynamic)

		_, err := cli.GetCompositionDefinition("notfound", namespace)
		if !errors.IsNotFound(err) {
			t.Log("Expected not found error")
			t.Fail()
		}

		return ctx
	}).Feature()

	testenv.Test(t, f)
}

func TestGetSecret(t *testing.T) {
	os.Setenv("DEBUG", "1")

	f := features.New("Setup").
		Setup(e2e.Logger("test")).
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			r, err := resources.New(cfg.Client().RESTConfig())
			if err != nil {
				t.Fail()
			}

			// apis.AddToScheme(r.GetScheme())

			err = decoder.DecodeEachFile(
				ctx, os.DirFS(filepath.Join(testdataPath, "secrets")), "*.yaml",
				decoder.CreateIgnoreAlreadyExists(r),
			)
			if err != nil {
				t.Log("Error decoding Secrets: ", err)
				t.Fail()
			}

			r.WithNamespace(namespace)
			return ctx
		}).Assess("Testing Secrets", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		r, err := resources.New(c.Client().RESTConfig())
		if err != nil {
			t.Fail()
		}
		r.WithNamespace(namespace)

		// apis.AddToScheme(r.GetScheme())

		objs, err := decoder.DecodeAllFiles(ctx, os.DirFS(filepath.Join(testdataPath, "secrets")), "*.yaml")
		if err != nil {
			t.Log("Error decoding Secrets: ", err)
			t.Fail()
		}

		for _, obj := range objs {
			sec := k8s.Object(obj).(*corev1.Secret)
			err = r.Get(ctx, sec.GetName(), sec.GetNamespace(), sec)
			if err != nil {
				t.Log("Error getting secret: ", err)
				t.Fail()
			}
			t.Log("Secret ID", sec.GetUID())
			key := sec.Data["token"]

			dynamic := dynamic.NewForConfigOrDie(c.Client().RESTConfig())
			cli := NewClient(dynamic)

			retr, err := cli.GetSecret(rtv1.SecretKeySelector{
				Key: "token",
				Reference: rtv1.Reference{
					Name:      sec.GetName(),
					Namespace: sec.GetNamespace(),
				},
			})
			if err != nil {
				t.Log("Error getting secret by key: ", err)
				t.Fail()
			}

			if retr != string(key) {
				t.Log("Secret key mismatch")
				t.Fail()
			}
		}

		return ctx
	}).Feature()

	testenv.Test(t, f)
}

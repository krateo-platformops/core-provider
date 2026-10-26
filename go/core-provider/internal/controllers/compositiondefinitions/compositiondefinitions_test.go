//go:build integration
// +build integration

package compositiondefinitions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/krateo-platformops/plumbing/cache"

	"github.com/go-logr/logr"
	"github.com/krateo-platformops/provider-runtime/pkg/controller"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	"github.com/krateo-platformops/provider-runtime/pkg/ratelimiter"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/krateo-platformops/core-provider/apis"
	"github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/tools/kube/watcher"
	"github.com/krateo-platformops/plumbing/e2e"
	xenv "github.com/krateo-platformops/plumbing/env"
	rtv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	clientsetscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"sigs.k8s.io/e2e-framework/klient/decoder"
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
)

const (
	crdPath       = "../../../crds"
	testdataPath  = "../../../testdata/test"
	manifestsPath = "./testdata/manifests"

	namespace = "test-system"
)

func TestMain(m *testing.M) {
	xenv.SetTestMode(true)

	clusterName = "krateo-core-provider-controller"
	testenv = env.New()
	kindCluster := kind.NewCluster(clusterName)

	_ = apiextensionsv1.AddToScheme(clientsetscheme.Scheme)
	_ = apis.AddToScheme(clientsetscheme.Scheme)

	cleanAssetFolder := func(ctx context.Context, c *envconf.Config) (context.Context, error) {
		err := os.RemoveAll(filepath.Join(os.TempDir(), "assets"))
		return ctx, err
	}

	// Cluster creation uses kind's default node image (what CI runs on). Local runs on Docker Desktop /
	// Apple Silicon — where that default (currently kindest/node:v1.36.1) fails to boot with a systemd
	// "Reached target Multi-User System" error — can override it via KIND_NODE_IMAGE, e.g.
	// KIND_NODE_IMAGE=kindest/node:v1.32.2. CI leaves it unset and keeps the default.
	createCluster := envfuncs.CreateCluster(kindCluster, clusterName)
	if img := os.Getenv("KIND_NODE_IMAGE"); img != "" {
		createCluster = envfuncs.CreateClusterWithOpts(kindCluster, clusterName, kind.WithImage(img))
	}

	testenv.Setup(
		cleanAssetFolder,
		createCluster,
		e2e.CreateNamespace(namespace),

		func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
			r, err := resources.New(cfg.Client().RESTConfig())
			if err != nil {
				return ctx, err
			}
			r.WithNamespace(namespace)

			err = os.MkdirAll(filepath.Join(os.TempDir(), "assets", "cdc-deployment"), os.ModePerm)
			if err != nil {
				return ctx, err
			}
			err = os.Link(filepath.Join(manifestsPath, "deployment.yaml"), filepath.Join(os.TempDir(), "assets", "cdc-deployment", "deployment.yaml"))
			if err != nil {
				return ctx, err
			}

			err = os.MkdirAll(filepath.Join(os.TempDir(), "assets", "cdc-configmap"), os.ModePerm)
			if err != nil {
				return ctx, err
			}
			err = os.Link(filepath.Join(manifestsPath, "configmap.yaml"), filepath.Join(os.TempDir(), "assets", "cdc-configmap", "configmap.yaml"))
			if err != nil {
				return ctx, err
			}

			err = os.MkdirAll(filepath.Join(os.TempDir(), "assets", "json-schema-configmap"), os.ModePerm)
			if err != nil {
				return ctx, err
			}
			err = os.Link(filepath.Join(manifestsPath, "json-schema-configmap.yaml"), filepath.Join(os.TempDir(), "assets", "json-schema-configmap", "configmap.yaml"))
			if err != nil {
				return ctx, err
			}

			err = os.MkdirAll(filepath.Join(os.TempDir(), "assets", "cdc-service"), os.ModePerm)
			if err != nil {
				return ctx, err
			}
			err = os.Link(filepath.Join(manifestsPath, "service.yaml"), filepath.Join(os.TempDir(), "assets", "cdc-service", "service.yaml"))
			if err != nil {
				return ctx, err
			}

			err = os.MkdirAll(filepath.Join(os.TempDir(), "assets", "cdc-rbac"), os.ModePerm)
			if err != nil {
				return ctx, err
			}

			err = os.Link(filepath.Join(manifestsPath, "rbac", "clusterrole.yaml"), filepath.Join(os.TempDir(), "assets", "cdc-rbac", "clusterrole.yaml"))
			if err != nil {
				return ctx, err
			}
			err = os.Link(filepath.Join(manifestsPath, "rbac", "clusterrolebinding.yaml"), filepath.Join(os.TempDir(), "assets", "cdc-rbac", "clusterrolebinding.yaml"))
			if err != nil {
				return ctx, err
			}
			err = os.Link(filepath.Join(manifestsPath, "rbac", "serviceaccount.yaml"), filepath.Join(os.TempDir(), "assets", "cdc-rbac", "serviceaccount.yaml"))
			if err != nil {
				return ctx, err
			}
			err = os.Link(filepath.Join(manifestsPath, "rbac", "secret-role.yaml"), filepath.Join(os.TempDir(), "assets", "cdc-rbac", "secret-role.yaml"))
			if err != nil {
				return ctx, err
			}
			err = os.Link(filepath.Join(manifestsPath, "rbac", "secret-rolebinding.yaml"), filepath.Join(os.TempDir(), "assets", "cdc-rbac", "secret-rolebinding.yaml"))
			if err != nil {
				return ctx, err
			}
			err = os.Link(filepath.Join(manifestsPath, "rbac", "compositiondefinition-role.yaml"), filepath.Join(os.TempDir(), "assets", "cdc-rbac", "compositiondefinition-role.yaml"))
			if err != nil {
				return ctx, err
			}
			err = os.Link(filepath.Join(manifestsPath, "rbac", "compositiondefinition-rolebinding.yaml"), filepath.Join(os.TempDir(), "assets", "cdc-rbac", "compositiondefinition-rolebinding.yaml"))
			if err != nil {
				return ctx, err
			}

			// Install CRDs
			err = decoder.DecodeEachFile(
				ctx, os.DirFS(filepath.Join(crdPath)), "*.yaml",
				decoder.CreateIgnoreAlreadyExists(r),
			)

			time.Sleep(2 * time.Second) // wait for the compositiondefinition CRD to be registered

			return ctx, nil
		},
	).Finish(
		envfuncs.DeleteNamespace(namespace),
		envfuncs.TeardownCRDs(crdPath, "core.krateo.io_compositiondefinitions.yaml"),
		envfuncs.DestroyCluster(clusterName),
		cleanAssetFolder,
		func(ctx context.Context, c *envconf.Config) (context.Context, error) {
			if v := ctx.Value(stopKey{}); v != nil {
				if stop, ok := v.(context.CancelFunc); ok {
					fmt.Println("Stopping controller manager at ", time.Now().String())
					stop() // stops mgr.Start and the background goroutine
				}
			}
			return ctx, nil
		},
	)

	os.Exit(testenv.Run(m))
}

type stopKey struct{} // key for storing stop func in ctx

func TestController(t *testing.T) {
	os.Setenv("DEBUG", "1")
	restoreStderr := captureStderr(t)
	defer func() {
		logs := restoreStderr()
		if strings.Contains(logs, "Observed a panic") {
			t.Fatalf("controller panic detected in test logs:\n%s", logs)
		}
	}()

	setupController := func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
		// stdlib slog text handler for test output (replaces plumbing/slogs/pretty, removed on the
		// plumbing 1.10.x baseline).
		lh := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
			Level:     slog.LevelDebug,
			AddSource: false,
		})

		logrlog := logr.FromSlogHandler(slog.New(lh).Handler())
		log := logging.NewLogrLogger(logrlog)

		pluralizer := NewTestPluralizer(false)
		ctrl.SetLogger(logrlog)

		mgr, err := ctrl.NewManager(cfg.Client().RESTConfig(), ctrl.Options{
			Metrics: server.Options{
				BindAddress: "0", // disable metrics for tests
			},
		})
		if err != nil {
			return ctx, err
		}

		o := controller.Options{
			Logger:                  log,
			MaxConcurrentReconciles: 1,
			PollInterval:            20 * time.Second,
			GlobalRateLimiter:       ratelimiter.NewGlobalExponential(1*time.Second, 1*time.Minute),
		}

		// if err := apis.AddToScheme(mgr.GetScheme()); err != nil {
		// 	log.Info("Cannot add APIs to scheme", "error", err)
		// 	os.Exit(1)
		// }

		if err := Setup(mgr, Options{
			ControllerOptions: o,
			Pluralizer:        pluralizer,
		}); err != nil {
			log.Info("Cannot setup controllers", "error", err)
			os.Exit(1)
		}
		if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
			log.Info("Cannot start controller manager", "error", err)
			os.Exit(1)
		}
		return ctx, nil
	}
	getCDLI := func(ctx context.Context) []v1alpha1.CompositionDefinition {
		fli, err := decoder.DecodeAllFiles(ctx, os.DirFS(testdataPath), "*.yaml", decoder.MutateNamespace(namespace))
		if err != nil {
			t.Fatal(err)
		}
		var cdli []v1alpha1.CompositionDefinition
		for _, fi := range fli {
			switch v := fi.(type) {
			case *v1alpha1.CompositionDefinition:
				cdli = append(cdli, *v)
			case *unstructured.Unstructured:
				var cd v1alpha1.CompositionDefinition
				if err := runtime.DefaultUnstructuredConverter.FromUnstructured(v.Object, &cd); err != nil {
					t.Fatalf("unable to convert unstructured to CompositionDefinition: %v", err)
				}
				cdli = append(cdli, cd)
			default:
				t.Fatalf("expected CompositionDefinition or Unstructured, got %T", fi)
			}
		}
		return cdli
	}

	var r *resources.Resources

	f := features.New("Setup").
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			stopCtx, stop := context.WithCancel(context.Background())

			var err error
			r, err = resources.New(cfg.Client().RESTConfig())
			if err != nil {
				t.Fatal(err)
			}

			err = apis.AddToScheme(r.GetScheme())
			if err != nil {
				t.Fatal(err)
			}
			err = apiextensionsv1.AddToScheme(r.GetScheme())
			if err != nil {
				t.Fatal(err)
			}

			go func() {
				_, err := setupController(stopCtx, cfg)
				if err != nil {
					fmt.Printf("Error starting controller manager: %v\n", err)
				}
			}()

			// store cancel func in returned context so callers can stop the manager
			ctx = context.WithValue(ctx, stopKey{}, stop)
			return ctx
		}).
		Assess("Test Create", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			// r, err := resources.New(cfg.Client().RESTConfig())
			// if err != nil {
			// 	t.Fail()
			// }

			// apis.AddToScheme(r.GetScheme())
			r.WithNamespace(namespace)

			cdli := getCDLI(ctx)
			for _, res := range cdli {
				// Create CompositionDefinition
				err := r.Create(ctx, &res)
				if err != nil {
					t.Fatal(err)
				}
			}

			time.Sleep(5 * time.Second) // wait for the controller to pick up the new resources

			dynamic := dynamic.NewForConfigOrDie(cfg.Client().RESTConfig())
			for _, res := range cdli {
				//wait for resource to be created

				if err := watcher.NewWatcher(
					dynamic,
					schema.GroupVersionResource{
						Group:    v1alpha1.Group,
						Version:  v1alpha1.Version,
						Resource: "compositiondefinitions",
					},
					5*time.Minute,
					func(cd *v1alpha1.CompositionDefinition) bool {
						return cd.GetCondition(rtv1.TypeReady).Reason == rtv1.ReasonAvailable && cd.GetCondition(rtv1.TypeReady).Status == metav1.ConditionTrue
					},
				).WatchResource(ctx, namespace, res.Name); err != nil {
					obj := v1alpha1.CompositionDefinition{}
					r.Get(ctx, res.Name, namespace, &obj)
					b, _ := json.MarshalIndent(obj.Status, "", "  ")
					t.Logf("CompositionDefinition Status: %s", string(b))
					t.Fatal(err)
				}
			}

			return ctx
		}).Assess("Test Change Version", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		toUpgrade := []struct {
			filename       string
			currentVersion string
			newVersion     string
		}{
			{
				filename:       "fireworksapp.yaml",
				currentVersion: "2.0.3",
				newVersion:     "2.0.4",
			},
		}

		// r, err := resources.New(cfg.Client().RESTConfig())
		// if err != nil {
		// 	t.Fail()
		// }
		// apis.AddToScheme(r.GetScheme())
		r.WithNamespace(namespace)

		for _, test := range toUpgrade {
			var res v1alpha1.CompositionDefinition
			err := decoder.DecodeFile(
				os.DirFS(filepath.Join(testdataPath)), test.filename,
				&res,
				decoder.MutateNamespace(namespace),
			)
			if err != nil {
				t.Fatal(err)
			}

			err = r.Get(ctx, res.Name, namespace, &res)
			if err != nil {
				t.Fatal(err)
			}

			res.Spec.Chart.Version = test.newVersion
			// Update CompositionDefinition
			err = r.Update(ctx, &res)
			if err != nil {
				t.Fatal(err)
			}
		}

		for _, test := range toUpgrade {
			oldVersionNormalized := normalizeVersion(test.currentVersion, '-')
			newVersionNormalized := normalizeVersion(test.newVersion, '-')
			var res v1alpha1.CompositionDefinition
			err := decoder.DecodeFile(
				os.DirFS(filepath.Join(testdataPath)), test.filename,
				&res,
				decoder.MutateNamespace(namespace),
			)
			if err != nil {
				t.Fatal(err)
			}

			//wait for resource to be created

			watcher := watcher.NewWatcher(
				dynamic.NewForConfigOrDie(cfg.Client().RESTConfig()),
				schema.GroupVersionResource{
					Group:    v1alpha1.Group,
					Version:  v1alpha1.Version,
					Resource: "compositiondefinitions",
				},
				3*time.Minute,
				func(cd *v1alpha1.CompositionDefinition) bool {
					// VersionInfo is a projection of the live CRD (#234A): the served-version prune
					// drops the stale old version from BOTH the CRD and VersionInfo, so after the
					// 2.0.3 -> 2.0.4 upgrade it settles at 2 ([<new> (served), vacuum (storage)]),
					// not the old append-only 3. Waiting on the settled 2 also skips the transient
					// pre-prune 3, which never coincides with Ready=Available.
					return cd.GetCondition(rtv1.TypeReady).Reason == rtv1.ReasonAvailable &&
						len(cd.Status.Managed.VersionInfo) == 2 &&
						slices.ContainsFunc(cd.Status.Managed.VersionInfo, func(v v1alpha1.VersionDetail) bool {
							return v.Version == newVersionNormalized
						})
				},
			)
			if err := watcher.WatchResource(ctx, namespace, res.Name); err != nil {
				obj := v1alpha1.CompositionDefinition{}
				r.Get(ctx, res.Name, namespace, &obj)
				b, _ := json.MarshalIndent(obj.Status, "", "  ")
				t.Logf("CompositionDefinition Status: %s", string(b))
				t.Fatal(err)
			}

			err = r.Get(ctx, res.Name, namespace, &res)
			if err != nil {
				t.Fatal(err)
			}

			var crd apiextensionsv1.CustomResourceDefinition

			// apiextensionsv1.AddToScheme(r.GetScheme())

			gv, err := schema.ParseGroupVersion(res.Status.ApiVersion)
			if err != nil {
				t.Fatal(err)
			}
			crdName := fmt.Sprintf("%s.%s", res.Status.Resource, gv.Group)

			hasVersion := func(name string) bool {
				return slices.ContainsFunc(crd.Spec.Versions, func(v apiextensionsv1.CustomResourceDefinitionVersion) bool {
					return v.Name == name
				})
			}

			// The served-version prune (#103) removes the now-stale old version once nothing
			// references it (no other CompositionDefinition is on it and no Composition instance
			// carries its version label), so after the 2.0.3 -> 2.0.4 upgrade the CRD converges to
			// [vacuum (storage), <new> (served)] — the old version is dropped. The prune is driven
			// from Observe and applied over several reconciles, so poll the CRD until it converges.
			//
			// NOTE: Status.Managed.VersionInfo is a projection of the live CRD (#234A) — the pruned
			// version is dropped from it too, so it settles at 2 in lockstep with the CRD (this is
			// what the watcher condition above waits for).
			deadline := time.Now().Add(2 * time.Minute)
			for {
				if err = r.Get(ctx, crdName, "", &crd); err != nil {
					t.Fatal(err)
				}
				if len(crd.Spec.Versions) == 2 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("CRD did not converge to 2 versions after the served-version prune; got %d: %v",
						len(crd.Spec.Versions), crd.Spec.Versions)
				}
				time.Sleep(5 * time.Second)
			}

			// new version is served, storage (vacuum) is retained, stale old version is pruned.
			if !hasVersion(newVersionNormalized) {
				t.Fatalf("Expected new version %s to be served, got %v", newVersionNormalized, crd.Spec.Versions)
			}
			if !hasVersion("vacuum") {
				t.Fatalf("Expected storage version vacuum to be retained, got %v", crd.Spec.Versions)
			}
			if hasVersion(oldVersionNormalized) {
				t.Fatalf("Expected stale old version %s to be pruned, but it is still present: %v",
					oldVersionNormalized, crd.Spec.Versions)
			}

		}

		return ctx
	}).Assess("Test Re-home: the second definition follows", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		return assessRehomeFollower(ctx, t, cfg, r)
	}).Assess("Test Delete", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		// r, err := resources.New(cfg.Client().RESTConfig())
		// if err != nil {
		// 	t.Fail()
		// }
		// apis.AddToScheme(r.GetScheme())
		r.WithNamespace(namespace)

		cdli := getCDLI(ctx)

		for _, res := range cdli {
			// Delete CompositionDefinition
			err := r.Delete(ctx, &res)
			if err != nil {
				t.Fatal(err)
			}
		}

		for _, res := range cdli {
			//wait for resource to be deleted
			if err := wait.For(
				conditions.New(r).ResourceDeleted(&res),
				wait.WithTimeout(5*time.Minute),
				wait.WithInterval(5*time.Second),
			); err != nil {
				t.Fatal(err)
			}
		}

		return ctx
	}).Assess("Test Re-home: deleting the original hands the controller over", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		return assessRehomeHandover(ctx, t, cfg, r)
	}).Feature()

	testenv.Test(t, f)
}

// This is needed to avoid avoid running the controller in a pod as required by the original plumbings/plurals package
type TestPluralizer struct {
	cache *cache.TTLCache[string, Info]
}

func NewTestPluralizer(cached bool) *TestPluralizer {
	if cached {
		return &TestPluralizer{
			cache: cache.NewTTL[string, Info](),
		}
	}

	return &TestPluralizer{}
}

func (p TestPluralizer) GVKtoGVR(gvk schema.GroupVersionKind) (schema.GroupVersionResource, error) {
	info, err := Get(gvk, GetOptions{
		Cache: p.cache,
	})
	if err != nil {
		return schema.GroupVersionResource{}, err
	}

	return schema.GroupVersionResource{
		Group:    gvk.Group,
		Version:  gvk.Version,
		Resource: info.Plural,
	}, nil
}

type GetOptions struct {
	Logger       *slog.Logger
	Cache        *cache.TTLCache[string, Info]
	ResolverFunc func(schema.GroupVersionKind) (Info, error)
}

func Get(gvk schema.GroupVersionKind, opts GetOptions) (Info, error) {
	if opts.ResolverFunc == nil {
		opts.ResolverFunc = ResolveAPINames
	}

	var (
		tmp Info
		ok  bool
	)

	if opts.Cache != nil {
		tmp, ok = opts.Cache.Get(gvk.String())
		if ok && opts.Logger != nil {
			opts.Logger.Debug("cache hit", slog.String("gvk", gvk.String()))
		}
	}

	if !ok {
		if opts.Logger != nil {
			opts.Logger.Debug("cache miss", slog.String("gvk", gvk.String()))
		}

		var err error
		tmp, err = opts.ResolverFunc(gvk)
		if err != nil {
			if opts.Logger != nil {
				opts.Logger.Error("unable to discover API names",
					slog.String("gvk", gvk.String()), slog.Any("err", err))
			}
			return Info{}, err
		}

		if opts.Cache != nil {
			opts.Cache.Set(gvk.String(), tmp, time.Hour*48)
		}
	}

	if len(tmp.Plural) == 0 {
		err := &apierrors.StatusError{
			ErrStatus: metav1.Status{
				Status: metav1.StatusFailure,
				Code:   http.StatusNotFound,
				Reason: metav1.StatusReasonNotFound,
				Details: &metav1.StatusDetails{
					Group: gvk.Group,
					Kind:  gvk.Kind,
				},
				Message: fmt.Sprintf("no names found for %q", gvk.GroupVersion().String()),
			}}

		if opts.Logger != nil {
			opts.Logger.Warn(err.ErrStatus.Message)
		}
		return tmp, err
	}

	return tmp, nil
}

func ResolveAPINames(gvk schema.GroupVersionKind) (Info, error) {
	rc, err := rest.InClusterConfig()
	if err != nil {
		if errors.Is(err, rest.ErrNotInCluster) {
			// For testing purposes, use the default kubeconfig loading rules: $KUBECONFIG when set
			// (a run against a disposable cluster that must not touch ~/.kube), else ~/.kube/config.
			rc, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
				clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{}).ClientConfig()
			if err != nil {
				return Info{}, err
			}
		} else {
			return Info{}, err
		}
	}

	dc, err := discovery.NewDiscoveryClientForConfig(rc)
	if err != nil {
		return Info{}, err
	}

	list, err := dc.ServerResourcesForGroupVersion(gvk.GroupVersion().String())
	if err != nil {
		return Info{}, err
	}

	if list == nil || len(list.APIResources) == 0 {
		return Info{}, nil
	}

	var tmp Info
	for _, el := range list.APIResources {
		if el.Kind != gvk.Kind {
			continue
		}

		tmp = Info{
			Plural:   el.Name,
			Singular: el.SingularName,
			Shorts:   el.ShortNames,
		}
		break
	}

	return tmp, nil
}

type Info struct {
	Plural   string   `json:"plural"`
	Singular string   `json:"singular"`
	Shorts   []string `json:"shorts"`
}

// For go package and k8s version must complain to this pattern:
//
// [a-z]([-a-z0-9]*[a-z0-9])?
//
// Go package folders allow only underscore char ('_')
// K8s CRD version allow only dash char ('-')
// This version is taken from https://github.com/krateo-platformops/crdgen/blob/bf775894a752cc14d45d0b1f2a9dc080e0277517/internal/coders/support.go
func normalizeVersion(ver string, replaceChar rune) string {
	ver = strings.ToLower(ver)

	// Sostituisce tutti i caratteri non alfanumerici con replaceChar
	re := regexp.MustCompile(`[^a-z0-9]+`)
	ver = re.ReplaceAllString(ver, string(replaceChar))

	// Rimuove caratteri speciali all'inizio e alla fine
	ver = strings.Trim(ver, string(replaceChar))

	// Assicura che inizi con una lettera
	if len(ver) > 0 && ver[0] >= '0' && ver[0] <= '9' {
		ver = "v" + ver
	}

	return ver
}

func captureStderr(t *testing.T) func() string {
	t.Helper()

	originalStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("failed to create stderr pipe: %v", err)
	}

	os.Stderr = w

	var buf bytes.Buffer
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.MultiWriter(originalStderr, &buf), r)
		close(done)
	}()

	return func() string {
		_ = w.Close()
		<-done
		os.Stderr = originalStderr
		_ = r.Close()
		return buf.String()
	}
}

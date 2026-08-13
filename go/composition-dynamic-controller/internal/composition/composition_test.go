//go:build integration
// +build integration

package composition

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	rbacv1 "k8s.io/api/rbac/v1"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	"github.com/gobuffalo/flect"
	"github.com/krateo-platformops/composition-dynamic-controller/internal/tools/archive"
	compositionMeta "github.com/krateo-platformops/composition-dynamic-controller/pkg/meta"
	"github.com/krateo-platformops/plumbing/kubeutil/event"
	"github.com/krateo-platformops/plumbing/kubeutil/eventrecorder"
	"github.com/krateo-platformops/unstructured-runtime/pkg/controller"
	"github.com/krateo-platformops/unstructured-runtime/pkg/pluralizer"

	mapper "github.com/krateo-platformops/composition-dynamic-controller/internal/tools/dynamic"
	"github.com/krateo-platformops/plumbing/e2e"
	xenv "github.com/krateo-platformops/plumbing/env"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/e2e-framework/klient/decoder"
	"sigs.k8s.io/e2e-framework/klient/k8s"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
	"sigs.k8s.io/e2e-framework/pkg/env"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/envfuncs"
	"sigs.k8s.io/e2e-framework/pkg/features"
	"sigs.k8s.io/e2e-framework/support/kind"
)

type FakePluralizer struct {
}

var _ pluralizer.PluralizerInterface = &FakePluralizer{}

func (p FakePluralizer) GVKtoGVR(gvk schema.GroupVersionKind) (schema.GroupVersionResource, error) {
	return schema.GroupVersionResource{
		Group:    gvk.Group,
		Version:  gvk.Version,
		Resource: flect.Pluralize(strings.ToLower(gvk.Kind)),
	}, nil
}

var (
	testenv     env.Environment
	clusterName string
)

const (
	testdataPath = "../../testdata"
	namespace    = "demo-system"
	altNamespace = "krateo-system"
)

func TestMain(m *testing.M) {
	xenv.SetTestMode(true)

	clusterName = "kind"
	testenv = env.New()

	testenv.Setup(
		envfuncs.CreateCluster(kind.NewProvider(), clusterName),
		e2e.CreateNamespace(namespace),
		e2e.CreateNamespace(altNamespace),
	).Finish(
		envfuncs.DeleteNamespace(namespace),
		envfuncs.DestroyCluster(clusterName),
	)

	os.Exit(testenv.Run(m))
}

func TestController(t *testing.T) {
	var handler controller.ExternalClient
	// var labelselector labels.Selector
	var c *rest.Config
	f := features.New("Setup").
		Setup(e2e.Logger("test")).
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			c = SetSAToken(ctx, t, cfg)
			r, err := resources.New(cfg.Client().RESTConfig())
			if err != nil {
				t.Error("Creating resource client.", "error", err)
				return ctx
			}

			err = decoder.ApplyWithManifestDir(ctx, r, filepath.Join(testdataPath, "crds", "core"), "*.yaml", nil)
			if err != nil {
				t.Error("Applying crds composition manifests.", "error", err)
				return ctx
			}

			err = decoder.ApplyWithManifestDir(ctx, r, filepath.Join(testdataPath, "crds", "finops"), "*.yaml", nil)
			if err != nil {
				t.Error("Applying crds composition manifests.", "error", err)
				return ctx
			}

			time.Sleep(2 * time.Second)

			err = decoder.ApplyWithManifestDir(ctx, r, filepath.Join(testdataPath, "compositions"), "*.yaml", nil, decoder.MutateNamespace(namespace))
			if err != nil {
				t.Error("Applying composition manifests.", "error", err)
				return ctx
			}

			err = decoder.ApplyWithManifestDir(ctx, r, filepath.Join(testdataPath, "compositiondefinitions"), "*.yaml", nil, decoder.MutateNamespace(namespace))
			if err != nil {
				t.Error("Applying composition definition manifests.", "error", err)
				return ctx
			}

			var pig archive.Getter
			pluralizer := FakePluralizer{}

			pig, err = archive.Dynamic(cfg.Client().RESTConfig(), pluralizer)
			if err != nil {
				t.Error("Creating chart url info getter.", "error", err)
				return ctx
			}

			rec, err := eventrecorder.Create(ctx, cfg.Client().RESTConfig(), "test", nil)
			if err != nil {
				t.Error("Creating event recorder.", "error", err)
				return ctx
			}

			// --- start chart-inspector mock server ---
			mux := http.NewServeMux()
			mux.HandleFunc("/resources", func(w http.ResponseWriter, r *http.Request) {
				resources := []map[string]string{
					{
						"group":     "finops.krateo.io",
						"version":   "v1alpha1",
						"resource":  "datapresentationazures",
						"name":      "focus-1-focus-data-presentation-azure",
						"namespace": "demo-system",
					},
					{
						"group":     "finops.krateo.io",
						"version":   "v1alpha1",
						"resource":  "datapresentationazures",
						"name":      "focus-1-focus-data-presentation-azure",
						"namespace": "demo-system",
					},
				}
				enc := json.NewEncoder(w)
				w.Header().Set("Content-Type", "application/json")
				_ = enc.Encode(resources)
			})

			// Use httptest.Server so the test gets a reliable ephemeral port/URL
			ts := httptest.NewServer(mux)
			// intentionally NOT deferring ts.Close() here because Setup returns
			// and we want the server available for the entire test lifecycle.
			chartInspectorMockURL := ts.URL
			// --- end chart-inspector mock server ---

			pig, err = archive.Dynamic(cfg.Client().RESTConfig(), pluralizer)
			if err != nil {
				t.Error("Creating chart url info getter.", "error", err)
				return ctx
			}

			rec, err = eventrecorder.Create(ctx, cfg.Client().RESTConfig(), "test", nil)
			if err != nil {
				t.Error("Creating event recorder.", "error", err)
				return ctx
			}

			mapper, err := mapper.NewRESTMapper(cfg.Client().RESTConfig())
			if err != nil {
				t.Error("Creating REST mapper.", "error", err)
				return ctx
			}

			handler = NewHandler(&HandlerOptions{
				Kubeconfig:        cfg.Client().RESTConfig(),
				PackageInfoGetter: pig,
				EventRecorder:     *event.NewAPIRecorder(rec),
				Pluralizer:        pluralizer,
				ChartInspectorUrl: chartInspectorMockURL,
				SaName:            "test-sa",
				SaNamespace:       altNamespace,
				SafeReleaseName:   true,
				Mapper:            mapper,
			})
			resli, err := decoder.DecodeAllFiles(ctx, os.DirFS(filepath.Join(testdataPath, "compositiondefinitions")), "*.yaml")
			if err != nil {
				t.Log("Error decoding CRDs: ", err)
				t.Fail()
			}

			for _, res := range resli {
				uns, err := runtime.DefaultUnstructuredConverter.ToUnstructured(res)
				if err != nil {
					t.Log("Error converting CRD: ", err)
					t.Fail()
				}

				apiVersion, ok, err := unstructured.NestedString(uns, "status", "apiVersion")
				if !ok || err != nil {
					t.Log("Error getting apiVersion: ", err)
					t.Fail()
				}
				kind, ok, err := unstructured.NestedString(uns, "status", "kind")
				if !ok || err != nil {
					t.Log("Error getting kind: ", err)
					t.Fail()
				}
				err = r.PatchStatus(ctx, res, k8s.Patch{
					PatchType: types.MergePatchType,
					Data:      []byte(fmt.Sprintf(`{"status": {"apiVersion": "%s", "kind": "%s"}}`, apiVersion, kind)),
				})
				if err != nil {
					t.Log("Error patching Composition: ", err)
					t.Fail()
					return ctx
				}
			}
			return ctx
		}).Assess("Create", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		dynamic := dynamic.NewForConfigOrDie(c)
		var obj unstructured.Unstructured
		err := decoder.DecodeFile(os.DirFS(filepath.Join(testdataPath, "compositions")), "focus.yaml", &obj)
		if err != nil {
			t.Error("Decoding composition manifests.", "error", err)
			return ctx
		}

		version := obj.GetLabels()["krateo.io/composition-version"]
		cli := dynamic.Resource(schema.GroupVersionResource{
			Group:    "composition.krateo.io",
			Version:  version,
			Resource: flect.Pluralize(strings.ToLower(obj.GetObjectKind().GroupVersionKind().Kind)),
		}).Namespace(obj.GetNamespace())

		u, err := cli.Get(ctx, obj.GetName(), metav1.GetOptions{})
		if err != nil {
			t.Error("Getting composition.", "error", err)
			return ctx
		}

		observation, err := handler.Observe(ctx, u)
		if err != nil {
			t.Error("Observing composition.", "error", err)
			return ctx
		}

		u, err = cli.Get(ctx, obj.GetName(), metav1.GetOptions{})
		if err != nil {
			t.Error("Getting composition.", "error", err)
			return ctx
		}

		ctx, err = handleObservation(t, ctx, handler, observation, u)
		if err != nil {
			t.Error("Handling observation.", "error", err)
			return ctx
		}
		return ctx
	}).Assess("Update", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		r, err := resources.New(cfg.Client().RESTConfig())
		if err != nil {
			t.Error("Creating resource client.", "error", err)
			return ctx
		}
		dy := dynamic.NewForConfigOrDie(c)
		var obj unstructured.Unstructured
		err = decoder.DecodeFile(os.DirFS(filepath.Join(testdataPath, "compositions")), "focus.yaml", &obj)
		if err != nil {
			t.Error("Decoding composition manifests.", "error", err)
			return ctx
		}

		version := obj.GetLabels()["krateo.io/composition-version"]
		cli := dy.Resource(schema.GroupVersionResource{
			Group:    "composition.krateo.io",
			Version:  version,
			Resource: flect.Pluralize(strings.ToLower(obj.GetObjectKind().GroupVersionKind().Kind)),
		}).Namespace(obj.GetNamespace())
		u, err := cli.Get(ctx, obj.GetName(), metav1.GetOptions{})
		if err != nil {
			t.Error("Getting composition.", "error", err)
			return ctx
		}

		observation, err := handler.Observe(ctx, u)
		if err != nil {
			t.Error("Observing composition.", "error", err)
			return ctx
		}

		u, err = cli.Get(ctx, obj.GetName(), metav1.GetOptions{})
		if err != nil {
			t.Error("Getting composition.", "error", err)
			return ctx
		}

		ctx, err = handleObservation(t, ctx, handler, observation, u)
		if err != nil {
			t.Error("Handling observation.", "error", err)
			return ctx
		}

		u, err = cli.Get(ctx, obj.GetName(), metav1.GetOptions{})
		if err != nil {
			t.Error("Getting composition.", "error", err)
			return ctx
		}

		observation, err = handler.Observe(ctx, u)
		if err != nil {
			t.Error("Observing composition.", "error", err)
			return ctx
		}

		u, err = cli.Get(ctx, obj.GetName(), metav1.GetOptions{})
		if err != nil {
			t.Error("Getting composition.", "error", err)
			return ctx
		}

		managedResources, _, err := unstructured.NestedSlice(u.Object, "status", "managed")
		if err != nil {
			t.Error("Setting managed resources.", "error", err)
			return ctx
		}

		dyn2 := dynamic.NewForConfigOrDie(cfg.Client().RESTConfig())

		res, err := dyn2.Resource(schema.GroupVersionResource{
			Resource: "datapresentationazures",
			Group:    "finops.krateo.io",
			Version:  "v1alpha1",
		}).Namespace(obj.GetNamespace()).Get(ctx, "focus-1-focus-data-presentation-azure", metav1.GetOptions{})
		if err != nil {
			t.Error("Getting datapresentationazure.", "error", err)
			return ctx
		}

		err = r.PatchStatus(ctx, res, k8s.Patch{
			PatchType: types.MergePatchType,
			Data: []byte(`{
			  "status": {
				"armRegionName": "example-region",
				"armSkuName": "example-sku",
				"currencyCode": "USD",
				"effectiveStartDate": "2025-01-01",
				"isPrimaryMeterRegion": "true",
				"location": "example-location",
				"meterId": "example-meter-id",
				"meterName": "example-meter-name",
				"productId": "example-product-id",
				"productName": "example-product-name",
				"retailPrice": "100.00",
				"serviceFamily": "Networking",
				"serviceId": "example-service-id",
				"serviceName": "example-service-name",
				"skuId": "example-sku-id",
				"skuName": "example-sku-name",
				"tierMinimumUnits": "1",
				"type": "example-type",
				"unitOfMeasure": "example-unit",
				"unitPrice": "10.00"
			  }
			}`),
		})
		if err != nil {
			t.Error("Patching composition.", "error", err)
			return ctx
		}

		u, err = cli.Get(ctx, obj.GetName(), metav1.GetOptions{})
		if err != nil {
			t.Error("Getting composition.", "error", err)
			return ctx
		}

		observation, err = handler.Observe(ctx, u)
		if err != nil {
			t.Error("Observing composition.", "error", err)
			return ctx
		}

		u, err = cli.Get(ctx, obj.GetName(), metav1.GetOptions{})
		if err != nil {
			t.Error("Getting composition.", "error", err)
			return ctx
		}

		ctx, err = handleObservation(t, ctx, handler, observation, u)
		if err != nil {
			t.Error("Handling observation.", "error", err)
			return ctx
		}

		u, err = cli.Get(ctx, obj.GetName(), metav1.GetOptions{})
		if err != nil {
			t.Error("Getting composition.", "error", err)
			return ctx
		}

		tmpRes, _, err := unstructured.NestedSlice(u.Object, "status", "managed")
		if err != nil {
			t.Error("Setting managed resources.", "error", err)
			return ctx
		}

		if len(tmpRes) <= len(managedResources) {
			t.Error("Managed resources not updated.")
		}

		return ctx
	}).Assess("Break: Verify Observe Side Effects", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		dy := dynamic.NewForConfigOrDie(c)
		var obj unstructured.Unstructured
		if err := decoder.DecodeFile(os.DirFS(filepath.Join(testdataPath, "compositions")), "focus.yaml", &obj); err != nil {
			t.Error("Decoding composition manifests.", "error", err)
			return ctx
		}

		// 1. Get the Composition
		version := obj.GetLabels()["krateo.io/composition-version"]
		gvr := schema.GroupVersionResource{
			Group:    "composition.krateo.io",
			Version:  version,
			Resource: flect.Pluralize(strings.ToLower(obj.GetObjectKind().GroupVersionKind().Kind)),
		}

		cli := dy.Resource(gvr).Namespace(obj.GetNamespace())
		u, err := cli.Get(ctx, obj.GetName(), metav1.GetOptions{})
		if err != nil {
			t.Error("Getting composition.", "error", err)
			return ctx
		}

		// 2. Inspect the underlying Helm Secret to get the INITIAL Revision number
		// We look for secrets with owner=helm and name=release-name
		releaseName := compositionMeta.GetReleaseName(u)
		// secretClient := cfg.Client().Resources().GetControllerRuntimeClient()

		// Helper to count revisions
		countRevisions := func() (int, error) {
			// var secrets corev1.SecretList
			// // Helm stores releases in secrets with type 'helm.sh/release.v1'
			// // We verify by label "name" which equals the release name
			// labels := map[string]string{
			// 	"name":  releaseName,
			// 	"owner": "helm",
			// }
			// Note: In e2e-framework, we might need to use the dynamic client or typed client for listing with selectors
			// Using standard client-go for list here to be safe within the test closure
			clientset, _ := kubernetes.NewForConfig(c)
			list, err := clientset.CoreV1().Secrets(u.GetNamespace()).List(ctx, metav1.ListOptions{
				LabelSelector: fmt.Sprintf("name=%s,owner=helm", releaseName),
			})
			if err != nil {
				return 0, err
			}
			return len(list.Items), nil
		}

		initialRevisionCount, err := countRevisions()
		if err != nil {
			t.Error("Failed to count helm revisions", err)
			return ctx
		}
		t.Logf("Initial Helm Revision Count: %d", initialRevisionCount)

		// 3. The "Stress" Loop
		// We call Observe multiple times. In a correct controller, Observe is Read-Only.
		// It should NOT trigger a new Helm Release.
		t.Log("Starting Observe Loop (Simulating controller reconciliation)...")
		for i := 0; i < 5; i++ {
			// Refetch to ensure we have latest resourceVersion for internal logic
			u, _ = cli.Get(ctx, obj.GetName(), metav1.GetOptions{})

			// CALL OBSERVE
			_, err := handler.Observe(ctx, u)
			if err != nil {
				t.Logf("Observe iteration %d failed: %v", i, err)
			}
		}

		// 4. Check Revisions again
		finalRevisionCount, err := countRevisions()
		if err != nil {
			t.Error("Failed to count helm revisions after loop", err)
			return ctx
		}
		t.Logf("Final Helm Revision Count: %d", finalRevisionCount)

		if finalRevisionCount > initialRevisionCount {
			t.Errorf("CRITICAL FAILURE: The Observe method is not idempotent! "+
				"Helm revisions increased from %d to %d without any Spec changes. "+
				"This implies 'hc.Upgrade' is running on every reconciliation loop.",
				initialRevisionCount, finalRevisionCount)

			// Fail immediately to prevent cleaning up evidence if debugging
			t.FailNow()
		}

		return ctx
	}).Assess("Regression: Observe Tolerates Stale ResourceVersion", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		// Regression for the external-create wedge (unstructured-runtime incomplete-create recovery).
		// The runtime re-Observes with the copy of the CR it began the reconcile with; a concurrent
		// writer (an umbrella re-applying the instance, a kubectl edit, or the reconcile's own
		// external-create annotation write) may have bumped its resourceVersion in the meantime.
		// Observe previously did a NON-retrying tools.Update (release name + CD def-ref labels) that
		// 409'd on such a stale version and returned an error; the recovery then treats an Observe
		// error as "cannot determine creation result" and refuses forever, wedging the composition.
		// Observe must now be a pure read that tolerates a stale resourceVersion.
		dy := dynamic.NewForConfigOrDie(c)
		var obj unstructured.Unstructured
		if err := decoder.DecodeFile(os.DirFS(filepath.Join(testdataPath, "compositions")), "focus.yaml", &obj); err != nil {
			t.Error("Decoding composition manifests.", "error", err)
			return ctx
		}
		version := obj.GetLabels()["krateo.io/composition-version"]
		cli := dy.Resource(schema.GroupVersionResource{
			Group:    "composition.krateo.io",
			Version:  version,
			Resource: flect.Pluralize(strings.ToLower(obj.GetObjectKind().GroupVersionKind().Kind)),
		}).Namespace(obj.GetNamespace())

		// Snapshot the CR — this is the (soon-to-be-stale) object a reconcile would hold.
		stale, err := cli.Get(ctx, obj.GetName(), metav1.GetOptions{})
		if err != nil {
			t.Error("Getting composition.", "error", err)
			return ctx
		}

		// A concurrent writer bumps the CR's resourceVersion out from under `stale`.
		fresh, err := cli.Get(ctx, obj.GetName(), metav1.GetOptions{})
		if err != nil {
			t.Error("Getting composition.", "error", err)
			return ctx
		}
		ann := fresh.GetAnnotations()
		if ann == nil {
			ann = map[string]string{}
		}
		ann["krateo.io/test-concurrent-write"] = "1"
		fresh.SetAnnotations(ann)
		if _, err := cli.Update(ctx, fresh, metav1.UpdateOptions{}); err != nil {
			t.Error("Concurrent update failed.", "error", err)
			return ctx
		}

		// Observe with the STALE object. A pure-read Observe must not 409 here; the pre-fix code did,
		// which is exactly what wedged the external-create recovery.
		if _, err := handler.Observe(ctx, stale); err != nil {
			t.Errorf("Observe must tolerate a concurrently-bumped resourceVersion (pure read), got error: %v", err)
		}
		return ctx
	}).Assess("SelfHeal: Recreate Deleted Child", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		// (b) SELF-HEAL DELETE: delete a finops.krateo.io/datapresentationazures child
		// out-of-band. The next reconcile (Observe -> Update) must recreate it, and the
		// helm revision must bump exactly once (then remain stable).
		dy := dynamic.NewForConfigOrDie(c)
		var obj unstructured.Unstructured
		if err := decoder.DecodeFile(os.DirFS(filepath.Join(testdataPath, "compositions")), "focus.yaml", &obj); err != nil {
			t.Error("Decoding composition manifests.", "error", err)
			return ctx
		}

		version := obj.GetLabels()["krateo.io/composition-version"]
		gvr := schema.GroupVersionResource{
			Group:    "composition.krateo.io",
			Version:  version,
			Resource: flect.Pluralize(strings.ToLower(obj.GetObjectKind().GroupVersionKind().Kind)),
		}
		cli := dy.Resource(gvr).Namespace(obj.GetNamespace())

		u, err := cli.Get(ctx, obj.GetName(), metav1.GetOptions{})
		if err != nil {
			t.Error("Getting composition.", "error", err)
			return ctx
		}
		releaseName := compositionMeta.GetReleaseName(u)

		// Use the admin config (cfg) for out-of-band cluster inspection/mutation; the
		// SA-scoped config `c` cannot read finops children or list helm secrets.
		adminCfg := cfg.Client().RESTConfig()
		clientset, _ := kubernetes.NewForConfig(adminCfg)
		// latestRevision returns the HIGHEST helm release revision (from the "version"
		// label of the owner=helm secrets). We track the max revision NUMBER rather than
		// the secret COUNT because MaxHistory caps the number of retained secrets, which
		// makes a raw count useless once at the cap.
		latestRevision := func() int {
			list, err := clientset.CoreV1().Secrets(u.GetNamespace()).List(ctx, metav1.ListOptions{
				LabelSelector: fmt.Sprintf("name=%s,owner=helm", releaseName),
			})
			if err != nil {
				t.Fatalf("listing helm revisions: %v", err)
			}
			max := 0
			for _, s := range list.Items {
				if v := s.Labels["version"]; v != "" {
					if n, err := strconv.Atoi(v); err == nil && n > max {
						max = n
					}
				}
			}
			return max
		}

		childGVR := schema.GroupVersionResource{
			Resource: "datapresentationazures",
			Group:    "finops.krateo.io",
			Version:  "v1alpha1",
		}
		childName := "focus-1-focus-data-presentation-azure"
		childCli := dynamic.NewForConfigOrDie(adminCfg).Resource(childGVR).Namespace(obj.GetNamespace())

		// Confirm the child exists before we delete it.
		if _, err := childCli.Get(ctx, childName, metav1.GetOptions{}); err != nil {
			t.Fatalf("expected child %q to exist before delete: %v", childName, err)
		}

		revBefore := latestRevision()
		t.Logf("[selfheal-delete] latest revision before out-of-band delete: %d", revBefore)

		// Delete the child out-of-band.
		if err := childCli.Delete(ctx, childName, metav1.DeleteOptions{}); err != nil {
			t.Fatalf("deleting child out-of-band: %v", err)
		}
		// Wait for the delete to settle.
		for i := 0; i < 20; i++ {
			if _, err := childCli.Get(ctx, childName, metav1.GetOptions{}); errors.IsNotFound(err) {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		if _, err := childCli.Get(ctx, childName, metav1.GetOptions{}); !errors.IsNotFound(err) {
			t.Fatalf("child %q was not deleted out-of-band (err=%v)", childName, err)
		}
		t.Logf("[selfheal-delete] child %q deleted out-of-band", childName)

		// A single Observe runs the self-healing Reconcile internally: kc.Update recreates
		// the deleted child, Reconcile detects the create as a real change and writes ONE
		// new revision. We assert exactly-once by driving the heal through Observe alone
		// (not the Observe->Update route, which would apply a second time).
		u, _ = cli.Get(ctx, obj.GetName(), metav1.GetOptions{})
		observation, err := handler.Observe(ctx, u)
		if err != nil {
			t.Fatalf("observe after out-of-band delete: %v", err)
		}
		if observation.ResourceUpToDate {
			t.Errorf("expected Observe to report drift (ResourceUpToDate=false) after child deletion, got up-to-date")
		}

		// Child must be back (recreated by the reconcile's 3-way merge).
		if _, err := childCli.Get(ctx, childName, metav1.GetOptions{}); err != nil {
			t.Errorf("child %q was NOT recreated by reconcile: %v", childName, err)
		} else {
			t.Logf("[selfheal-delete] child %q recreated by reconcile", childName)
		}

		revAfterHeal := latestRevision()
		t.Logf("[selfheal-delete] latest revision after heal: %d (was %d)", revAfterHeal, revBefore)
		if revAfterHeal != revBefore+1 {
			t.Errorf("expected revision to bump exactly once (%d -> %d), got %d", revBefore, revBefore+1, revAfterHeal)
		}

		// Subsequent steady-state reconciles (via Observe) must NOT bump the revision again.
		for i := 0; i < 3; i++ {
			u, _ = cli.Get(ctx, obj.GetName(), metav1.GetOptions{})
			obs, err := handler.Observe(ctx, u)
			if err != nil {
				t.Fatalf("steady-state observe #%d: %v", i, err)
			}
			if !obs.ResourceUpToDate {
				t.Errorf("steady-state reconcile #%d unexpectedly reported drift", i)
			}
		}
		revSteady := latestRevision()
		t.Logf("[selfheal-delete] latest revision after 3 steady reconciles: %d (was %d)", revSteady, revAfterHeal)
		if revSteady != revAfterHeal {
			t.Errorf("steady-state reconciles bumped revision: %d -> %d", revAfterHeal, revSteady)
		}

		return ctx
	}).Assess("SelfHeal: Patch Field Drift", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		// (c) SELF-HEAL FIELD DRIFT: mutate a CHILD resource's spec field out-of-band
		// (kubectl-edit equivalent), then run one reconcile via Observe and assert the
		// 3-way merge PATCHED the field BACK to the chart-declared value, that the helm
		// revision bumped exactly once, and that subsequent steady-state reconciles do
		// NOT bump it again. This is the case the existence-only hybrid could not do.
		dy := dynamic.NewForConfigOrDie(c)
		var obj unstructured.Unstructured
		if err := decoder.DecodeFile(os.DirFS(filepath.Join(testdataPath, "compositions")), "focus.yaml", &obj); err != nil {
			t.Error("Decoding composition manifests.", "error", err)
			return ctx
		}
		version := obj.GetLabels()["krateo.io/composition-version"]
		gvr := schema.GroupVersionResource{
			Group:    "composition.krateo.io",
			Version:  version,
			Resource: flect.Pluralize(strings.ToLower(obj.GetObjectKind().GroupVersionKind().Kind)),
		}
		cli := dy.Resource(gvr).Namespace(obj.GetNamespace())
		u, err := cli.Get(ctx, obj.GetName(), metav1.GetOptions{})
		if err != nil {
			t.Fatalf("getting composition: %v", err)
		}
		releaseName := compositionMeta.GetReleaseName(u)
		adminCfg := cfg.Client().RESTConfig()
		clientset, _ := kubernetes.NewForConfig(adminCfg)
		latestRevision := func() int {
			list, err := clientset.CoreV1().Secrets(u.GetNamespace()).List(ctx, metav1.ListOptions{
				LabelSelector: fmt.Sprintf("name=%s,owner=helm", releaseName),
			})
			if err != nil {
				t.Fatalf("listing helm revisions: %v", err)
			}
			max := 0
			for _, s := range list.Items {
				if v := s.Labels["version"]; v != "" {
					if n, err := strconv.Atoi(v); err == nil && n > max {
						max = n
					}
				}
			}
			return max
		}

		childGVR := schema.GroupVersionResource{
			Resource: "datapresentationazures",
			Group:    "finops.krateo.io",
			Version:  "v1alpha1",
		}
		childName := "focus-1-focus-data-presentation-azure"
		childCli := dynamic.NewForConfigOrDie(adminCfg).Resource(childGVR).Namespace(obj.GetNamespace())

		child, err := childCli.Get(ctx, childName, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("getting child %q: %v", childName, err)
		}
		spec, _, _ := unstructured.NestedMap(child.Object, "spec")
		if spec == nil {
			t.Fatalf("child %q has no spec to drift", childName)
		}
		// Drift spec.$filter — a top-level scalar the focus CHART renders (from the
		// composition's spec.filter). It is chart-declared (present in the helm release
		// manifest), so the 3-way-with-live merge has a declared target to revert to. We
		// pick it explicitly (not by map iteration) so the test is deterministic. NOTE: on
		// this platform datapresentationazure's data fields are normally operator-populated;
		// in this harness there is no finops operator running, so the field's value comes
		// purely from the chart render — a clean chart-declared drift target.
		const driftField = "$filter"
		declaredVal, found, _ := unstructured.NestedString(child.Object, "spec", driftField)
		if !found {
			t.Fatalf("child %q has no chart-declared spec.%s to drift; spec=%v", childName, driftField, spec)
		}
		t.Logf("[selfheal-drift] child field spec.%s declared value = %q", driftField, declaredVal)

		revBefore := latestRevision()
		t.Logf("[selfheal-drift] revision before out-of-band drift: %d", revBefore)

		const driftedVal = "DRIFTED-OUT-OF-BAND"
		if declaredVal == driftedVal {
			t.Fatalf("declared value already equals the drift sentinel; pick another field")
		}
		if err := unstructured.SetNestedField(child.Object, driftedVal, "spec", driftField); err != nil {
			t.Fatalf("setting drifted field: %v", err)
		}
		if _, err := childCli.Update(ctx, child, metav1.UpdateOptions{}); err != nil {
			t.Fatalf("applying out-of-band drift: %v", err)
		}
		got, err := childCli.Get(ctx, childName, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("re-getting drifted child: %v", err)
		}
		if cur, _, _ := unstructured.NestedString(got.Object, "spec", driftField); cur != driftedVal {
			t.Fatalf("drift did not land: spec.%s = %q, want %q", driftField, cur, driftedVal)
		}
		t.Logf("[selfheal-drift] out-of-band set spec.%s = %q", driftField, driftedVal)

		u, _ = cli.Get(ctx, obj.GetName(), metav1.GetOptions{})
		observation, err := handler.Observe(ctx, u)
		if err != nil {
			t.Fatalf("observe after field drift: %v", err)
		}
		if observation.ResourceUpToDate {
			t.Errorf("expected Observe to report drift (ResourceUpToDate=false) after field drift, got up-to-date")
		}

		healed, err := childCli.Get(ctx, childName, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("getting child after heal: %v", err)
		}
		restored, _, _ := unstructured.NestedString(healed.Object, "spec", driftField)
		if restored != declaredVal {
			t.Errorf("field drift NOT healed: spec.%s = %q, want declared %q", driftField, restored, declaredVal)
		} else {
			t.Logf("[selfheal-drift] spec.%s patched back to declared %q", driftField, declaredVal)
		}

		revAfterHeal := latestRevision()
		t.Logf("[selfheal-drift] revision after heal: %d (was %d)", revAfterHeal, revBefore)
		if revAfterHeal != revBefore+1 {
			t.Errorf("expected revision to bump exactly once (%d -> %d), got %d", revBefore, revBefore+1, revAfterHeal)
		}

		for i := 0; i < 3; i++ {
			u, _ = cli.Get(ctx, obj.GetName(), metav1.GetOptions{})
			obs, err := handler.Observe(ctx, u)
			if err != nil {
				t.Fatalf("steady-state observe #%d: %v", i, err)
			}
			if !obs.ResourceUpToDate {
				t.Errorf("steady-state reconcile #%d unexpectedly reported drift", i)
			}
		}
		revSteady2 := latestRevision()
		t.Logf("[selfheal-drift] revision after 3 steady reconciles: %d (was %d)", revSteady2, revAfterHeal)
		if revSteady2 != revAfterHeal {
			t.Errorf("steady-state reconciles bumped revision: %d -> %d", revAfterHeal, revSteady2)
		}

		return ctx
	}).Assess("Delete", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		dy := dynamic.NewForConfigOrDie(c)
		var obj unstructured.Unstructured
		err := decoder.DecodeFile(os.DirFS(filepath.Join(testdataPath, "compositions")), "focus.yaml", &obj)
		if err != nil {
			t.Error("Decoding composition manifests.", "error", err)
			return ctx
		}

		version := obj.GetLabels()["krateo.io/composition-version"]
		cli := dy.Resource(schema.GroupVersionResource{
			Group:    "composition.krateo.io",
			Version:  version,
			Resource: flect.Pluralize(strings.ToLower(obj.GetObjectKind().GroupVersionKind().Kind)),
		}).Namespace(obj.GetNamespace())
		u, err := cli.Get(ctx, obj.GetName(), metav1.GetOptions{})
		if err != nil {
			t.Error("Getting composition.", "error", err)
			return ctx
		}

		observation, err := handler.Observe(ctx, u)
		if err != nil {
			t.Error("Observing composition.", "error", err)
			return ctx
		}

		u, err = cli.Get(ctx, obj.GetName(), metav1.GetOptions{})
		if err != nil {
			t.Error("Getting composition.", "error", err)
			return ctx
		}

		ctx, err = handleObservation(t, ctx, handler, observation, u)
		if err != nil {
			t.Error("Handling observation.", "error", err)
			return ctx
		}

		u, err = cli.Get(ctx, obj.GetName(), metav1.GetOptions{})
		if err != nil {
			t.Error("Getting composition.", "error", err)
			return ctx
		}

		u.SetFinalizers([]string{
			"composition.krateo.io/finalizer",
		})

		u, err = cli.Update(ctx, u, metav1.UpdateOptions{})
		if err != nil {
			t.Error("Updating composition.", "error", err)
			return ctx
		}

		err = cli.Delete(ctx, obj.GetName(), metav1.DeleteOptions{})
		if err != nil {
			t.Error("Deleting composition.", "error", err)
			return ctx
		}

		u, err = cli.Get(ctx, obj.GetName(), metav1.GetOptions{})
		if err != nil {
			t.Error("Getting composition.", "error", err)
			return ctx
		}

		err = handler.Delete(ctx, u)
		if err != nil {
			t.Error("Deleting composition.", "error", err)
			return ctx
		}

		u, err = cli.Get(ctx, obj.GetName(), metav1.GetOptions{})
		if err != nil {
			t.Error("Getting composition.", "error", err)
			return ctx
		}

		u.SetFinalizers([]string{})
		u, err = cli.Update(ctx, u, metav1.UpdateOptions{})
		if err != nil {
			t.Error("Updating composition.", "error", err)
			return ctx
		}

		// Check if the helm release is deleted
		tmp, err := dy.Resource(schema.GroupVersionResource{
			Group:    "",
			Version:  "v1",
			Resource: "secrets",
		}).Namespace(obj.GetNamespace()).List(ctx, metav1.ListOptions{
			LabelSelector: "name=" + compositionMeta.GetReleaseName(u) + ",owner=helm",
		})
		if tmp != nil && len(tmp.Items) > 0 {
			t.Error("Helm release secret still exists after deletion.")
			return ctx
		}

		_, err = cli.Get(ctx, obj.GetName(), metav1.GetOptions{})
		if err == nil {
			t.Error("Composition still exists after deletion.")
			return ctx
		}
		if !errors.IsNotFound(err) {
			t.Error("Getting composition.", "error", err)
			return ctx
		}

		return ctx
	}).Feature()

	testenv.Test(t, f)
}

func handleObservation(t *testing.T, ctx context.Context, handler controller.ExternalClient, observation controller.ExternalObservation, u *unstructured.Unstructured) (context.Context, error) {
	var err error
	if observation.ResourceExists == true && observation.ResourceUpToDate == true {
		observation, err = handler.Observe(ctx, u)
		if err != nil {
			t.Error("Observing composition.", "error", err)
			return ctx, err
		}
		if observation.ResourceExists == true && observation.ResourceUpToDate == true {
			t.Log("Composition already exists and is ready.")
			return ctx, nil
		}
	} else if observation.ResourceExists == false && observation.ResourceUpToDate == true {
		err = handler.Delete(ctx, u)
		if err != nil {
			t.Error("Deleting composition.", "error", err)
			return ctx, err
		}
	} else if observation.ResourceExists == true && observation.ResourceUpToDate == false {
		err = handler.Update(ctx, u)
		if err != nil {
			t.Error("Updating composition.", "error", err)
			return ctx, err
		}
	} else if observation.ResourceExists == false && observation.ResourceUpToDate == false {
		err = handler.Create(ctx, u)
		if err != nil {
			t.Error("Creating composition.", "error", err)
			return ctx, err
		}
	}
	return ctx, nil
}

func SetSAToken(ctx context.Context, t *testing.T, cfg *envconf.Config) *rest.Config {
	clientset, err := kubernetes.NewForConfig(cfg.Client().RESTConfig())
	if err != nil {
		t.Fatal(err)
	}

	_, err = clientset.CoreV1().ServiceAccounts(namespace).Create(ctx, &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sa",
			Namespace: namespace,
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// Create a Role and RoleBinding for the ServiceAccount
	_, err = clientset.RbacV1().Roles(namespace).Create(ctx, &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sa-role",
			Namespace: namespace,
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{"composition.krateo.io"},
				Resources: []string{"*"},
				Verbs:     []string{"*"},
			},
			{
				APIGroups: []string{"core.krateo.io"},
				Resources: []string{"compositiondefinitions"},
				Verbs:     []string{"get", "list", "watch"},
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	_, err = clientset.RbacV1().RoleBindings(namespace).Create(ctx, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sa-role-binding",
			Namespace: namespace,
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      "test-sa",
				Namespace: namespace,
			},
		},
		RoleRef: rbacv1.RoleRef{
			Kind:     "Role",
			Name:     "test-sa-role",
			APIGroup: "rbac.authorization.k8s.io",
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	_, err = clientset.RbacV1().ClusterRoles().Create(ctx, &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-sa-cluster-role",
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{"rbac.authorization.k8s.io"},
				Resources: []string{"roles", "rolebindings", "clusterroles", "clusterrolebindings"},
				Verbs:     []string{"*"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"secrets"},
				Verbs:     []string{"*"},
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	_, err = clientset.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-sa-cluster-role-binding",
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      "test-sa",
				Namespace: namespace,
			},
		},
		RoleRef: rbacv1.RoleRef{
			Kind:     "ClusterRole",
			Name:     "test-sa-cluster-role",
			APIGroup: "rbac.authorization.k8s.io",
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	_, err = clientset.CoreV1().Secrets(namespace).Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sa-token",
			Namespace: namespace,
			Annotations: map[string]string{
				"kubernetes.io/service-account.name": "test-sa",
			},
		},
		Type: corev1.SecretTypeServiceAccountToken,
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(2 * time.Second)

	tokenSecret, err := clientset.CoreV1().Secrets(namespace).Get(ctx, "test-sa-token", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}

	saToken := string(tokenSecret.Data["token"])
	cacrt := tokenSecret.Data["ca.crt"]

	if saToken == "" {
		t.Fatal("ServiceAccount token not found")
	}

	// Create a new REST config with the ServiceAccount token
	restConfig := cfg.Client().RESTConfig()
	restConfig.BearerToken = saToken
	restConfig.BearerTokenFile = ""

	config := &rest.Config{
		Host:        restConfig.Host,
		BearerToken: string(saToken),
		TLSClientConfig: rest.TLSClientConfig{
			CertData: cacrt,
			Insecure: true,
		},
	}

	return config
}

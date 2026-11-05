//go:build integration
// +build integration

package composition

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"io"
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
	"github.com/krateo-platformops/unstructured-runtime/pkg/tools"

	mapper "github.com/krateo-platformops/composition-dynamic-controller/internal/tools/dynamic"
	"github.com/krateo-platformops/plumbing/e2e"
	xenv "github.com/krateo-platformops/plumbing/env"
	helmconfig "github.com/krateo-platformops/plumbing/helm"
	helmv3 "github.com/krateo-platformops/plumbing/helm/v3"
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

	// Every cluster-starting suite pins KUBECONFIG first: kind writes to AND switches
	// ~/.kube/config otherwise, and DestroyCluster then removes that context again, leaving the
	// developer pointed at nothing. Invisible on CI, where there is no kubeconfig worth
	// clobbering. See e2e.PinKubeconfig.
	restore := e2e.PinKubeconfig("cdc-composition")

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

	code := testenv.Run(m)
	restore() // not deferred: os.Exit does not run defers
	os.Exit(code)
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
	}).Assess("Regression: A Stale ResourceVersion Is A Quiet Requeue, Never A Wedge", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		// A reconcile holds the copy of the CR it began with, and a concurrent writer (an umbrella
		// re-applying the instance, a kubectl edit, the reconcile's own external-create annotation
		// write) may bump its resourceVersion meanwhile.
		//
		// WHAT THIS USED TO ASSERT, AND WHY IT CHANGED. It required Observe to return NO error for the
		// stale copy, because the runtime's incomplete-create recovery once treated any Observe error
		// as "cannot determine creation result" and refused forever. Two things have since moved:
		//   - the cdc implements controller.ExistenceChecker (existence.go, #110/#130), so recovery
		//     asks Exists, a pure read, and never Observe;
		//   - #359: Observe's status write with a stale resourceVersion used to be force-re-submitted
		//     over the newer write (a lost update that wiped status.managed). It now RETURNS the
		//     conflict, which unstructured-runtime >= v1.9.0 requeues quietly after 1s.
		// So the property that matters is no longer "no error". It is: the error is the quiet-requeue
		// kind and nothing else, the newer write survives, the next pass converges, and recovery's
		// own question is still answerable from the stale copy.
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
		// ...and a newer STATUS write, which is what #359's lost update destroyed. A condition of a
		// type the cdc never writes: the stale copy does not have it, so the old force-re-submit
		// (re-read, put the stale-computed status on the live object, write) would remove it, while
		// a fresh pass upserts its own conditions by type and leaves it alone.
		fresh2, err := cli.Get(ctx, obj.GetName(), metav1.GetOptions{})
		if err != nil {
			t.Error("Getting composition.", "error", err)
			return ctx
		}
		conds, _, _ := unstructured.NestedSlice(fresh2.Object, "status", "conditions")
		conds = append(conds, map[string]any{
			"type": "ConcurrentWriter", "status": "True", "reason": "Test",
			"message": "written by a concurrent writer", "lastTransitionTime": time.Now().UTC().Format(time.RFC3339),
		})
		if err := unstructured.SetNestedSlice(fresh2.Object, conds, "status", "conditions"); err != nil {
			t.Fatal(err)
		}
		if _, err := cli.UpdateStatus(ctx, fresh2, metav1.UpdateOptions{}); err != nil {
			t.Error("Concurrent status update failed.", "error", err)
			return ctx
		}
		hasConcurrentCondition := func(u *unstructured.Unstructured) bool {
			cs, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
			for _, c := range cs {
				if m, ok := c.(map[string]any); ok && m["type"] == "ConcurrentWriter" {
					return true
				}
			}
			return false
		}

		// 1. Observe with the STALE copy: either nothing to write (nil), or the status write lost the
		//    race. Any OTHER error is a failure the runtime would record, which is the wedge class.
		if _, err := handler.Observe(ctx, stale.DeepCopy()); err != nil && !stderrors.Is(err, tools.ErrStatusConflict) {
			t.Errorf("a stale resourceVersion must end Observe as a status conflict (a quiet requeue) or "+
				"not at all; got an ordinary error the runtime would record: %v", err)
		}

		// 2. The concurrent write survived: a stale pass must never replace a newer write.
		after, err := cli.Get(ctx, obj.GetName(), metav1.GetOptions{})
		if err != nil {
			t.Error("Getting composition.", "error", err)
			return ctx
		}
		if after.GetAnnotations()["krateo.io/test-concurrent-write"] != "1" {
			t.Errorf("the concurrent write was lost after a stale Observe: annotations=%v", after.GetAnnotations())
		}
		if !hasConcurrentCondition(after) {
			t.Errorf("the newer STATUS write was replaced by the stale pass's status (#359's lost update): " +
				"condition ConcurrentWriter is gone")
		}

		// 3. The next pass reads the current object, as the requeued pass would, and converges.
		if _, err := handler.Observe(ctx, after); err != nil {
			t.Errorf("the pass after a status conflict must converge from a fresh read, got: %v", err)
		}
		if converged, err := cli.Get(ctx, obj.GetName(), metav1.GetOptions{}); err == nil && !hasConcurrentCondition(converged) {
			t.Errorf("the converging pass removed a condition it does not own (ConcurrentWriter)")
		}

		// 4. Recovery's question is answerable from the STALE copy: Exists is a pure read.
		checker, ok := handler.(controller.ExistenceChecker)
		if !ok {
			t.Fatal("the handler no longer implements controller.ExistenceChecker, so incomplete-create " +
				"recovery would fall back to Observe, whose status conflict it would then have to handle")
		}
		if exists, err := checker.Exists(ctx, stale.DeepCopy()); err != nil || !exists {
			t.Errorf("Exists must answer from a stale copy without error: exists=%v err=%v", exists, err)
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
		// new revision. This case drives the heal through Observe ALONE.
		//
		// It used to say "not the Observe->Update route, which would apply a second time",
		// which is how #175 hid in plain sight: the harness knew the full route doubled and
		// stepped around it rather than asserting on it. The route it avoided is now covered
		// by "SelfHeal: the full Observe->Update route writes ONE revision" below.
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

		// THE RUNTIME'S NEXT STEP, which this assessment used to skip. The healing pass wrote a NEW
		// revision, so the recorded inventory is now stamped with the PREVIOUS release's digest, and
		// Observe correctly keeps reporting drift ("the recorded set was decoded from release digest
		// X, the current release is Y", #273) until Update re-records it. The runtime dispatches that
		// Update on drift; calling only Observe in a loop models a runtime that does not exist, and
		// that loop failed for exactly that reason once the fixture CRDs stopped pruning
		// managedDigest. Update must re-record WITHOUT writing another revision (#175).
		u, _ = cli.Get(ctx, obj.GetName(), metav1.GetOptions{})
		if err := handler.Update(ctx, u); err != nil {
			t.Fatalf("[selfheal-delete] the Update the runtime dispatches after the healing pass: %v", err)
		}
		if r := latestRevision(); r != revAfterHeal {
			t.Errorf("[selfheal-delete] re-recording the inventory wrote a revision (%d -> %d); #175 is back", revAfterHeal, r)
		}

		// Subsequent steady-state reconciles (via Observe) must NOT report drift or bump the revision.
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
	}).Assess("SelfHeal: the full Observe->Update route writes ONE revision", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		// #175's acceptance test, on the route the case above deliberately avoided.
		//
		// The runtime does not stop at Observe. Observe converges the cluster with
		// hc.Reconcile and returns ResourceUpToDate:false, which makes the runtime dispatch
		// Update — and Update used to perform an unconditional hc.Upgrade, writing a SECOND
		// revision for drift the first call had already healed. Measured on krateo-057 as
		// strictly bimodal pairs ~3s apart against a 60s cadence; git-provider reached 62,783
		// revisions in 28 days.
		//
		// Two arms, and the second is what makes the first mean anything:
		//
		//   A. drift -> Observe -> Update   must bump the revision EXACTLY ONCE
		//   B. drift -> Update alone        must still bump it exactly once, and heal
		//
		// Without B, a "fix" that simply made Update do nothing would pass A. #175 names this
		// explicitly: a fix that MOVED the write rather than removing it passes the first
		// check and fails the second.
		dy := dynamic.NewForConfigOrDie(c)
		var obj unstructured.Unstructured
		if err := decoder.DecodeFile(os.DirFS(filepath.Join(testdataPath, "compositions")), "focus.yaml", &obj); err != nil {
			t.Fatalf("decoding composition manifest: %v", err)
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

		dropChild := func(tag string) {
			t.Helper()
			if err := childCli.Delete(ctx, childName, metav1.DeleteOptions{}); err != nil {
				t.Fatalf("[%s] deleting child out-of-band: %v", tag, err)
			}
			for i := 0; i < 20; i++ {
				if _, err := childCli.Get(ctx, childName, metav1.GetOptions{}); errors.IsNotFound(err) {
					return
				}
				time.Sleep(200 * time.Millisecond)
			}
			t.Fatalf("[%s] child %q was not deleted out-of-band", tag, childName)
		}

		// --- arm A: the full route ---
		revBefore := latestRevision()
		dropChild("A")

		u, _ = cli.Get(ctx, obj.GetName(), metav1.GetOptions{})
		observation, err := handler.Observe(ctx, u)
		if err != nil {
			t.Fatalf("[A] observe after out-of-band delete: %v", err)
		}
		if observation.ResourceUpToDate {
			t.Fatalf("[A] Observe did not report drift, so the route under test is not exercised")
		}

		// Exactly what the runtime does next.
		u, _ = cli.Get(ctx, obj.GetName(), metav1.GetOptions{})
		if err := handler.Update(ctx, u); err != nil {
			t.Fatalf("[A] update after drift: %v", err)
		}

		revAfterRoute := latestRevision()
		t.Logf("[175-A] revision %d -> %d across Observe+Update", revBefore, revAfterRoute)
		if revAfterRoute != revBefore+1 {
			t.Errorf("[A] the Observe->Update route wrote %d revisions for ONE drift (%d -> %d), want 1. "+
				"This is #175: Observe's reconcile heals the drift and Update then writes again for it.",
				revAfterRoute-revBefore, revBefore, revAfterRoute)
		}
		if _, err := childCli.Get(ctx, childName, metav1.GetOptions{}); err != nil {
			t.Errorf("[A] child %q not present after the route: %v", childName, err)
		}

		// --- arm B: the control. Update alone must still converge and write once ---
		revBeforeB := latestRevision()
		dropChild("B")

		u, _ = cli.Get(ctx, obj.GetName(), metav1.GetOptions{})
		if err := handler.Update(ctx, u); err != nil {
			t.Fatalf("[B] update on a drifted cluster: %v", err)
		}

		revAfterB := latestRevision()
		t.Logf("[175-B] revision %d -> %d across Update alone", revBeforeB, revAfterB)
		if _, err := childCli.Get(ctx, childName, metav1.GetOptions{}); err != nil {
			t.Errorf("[B] Update alone did not heal the drift; child %q still missing: %v. "+
				"Arm A would then pass for the wrong reason — a no-op Update writes no second "+
				"revision either.", childName, err)
		}
		if revAfterB != revBeforeB+1 {
			t.Errorf("[B] Update alone wrote %d revisions (%d -> %d), want exactly 1: it must still "+
				"converge when nothing converged before it", revAfterB-revBeforeB, revBeforeB, revAfterB)
		}

		// --- steady state: neither phase writes when there is nothing to do ---
		revSteadyBefore := latestRevision()
		u, _ = cli.Get(ctx, obj.GetName(), metav1.GetOptions{})
		if obs, err := handler.Observe(ctx, u); err != nil {
			t.Fatalf("steady-state observe: %v", err)
		} else if !obs.ResourceUpToDate {
			t.Errorf("steady-state Observe reported drift after the cluster was converged")
		}
		u, _ = cli.Get(ctx, obj.GetName(), metav1.GetOptions{})
		if err := handler.Update(ctx, u); err != nil {
			t.Fatalf("steady-state update: %v", err)
		}
		if revSteady := latestRevision(); revSteady != revSteadyBefore {
			t.Errorf("a converged cluster still wrote %d revision(s) (%d -> %d); apply-if-changed "+
				"is not holding on either phase", revSteady-revSteadyBefore, revSteadyBefore, revSteady)
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
	}).Assess("Adoption: a recreated CR with no release-name label upgrades its existing release (#322)", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		// #322 END TO END, against real helm releases. The unit decision tables
		// (releaseadopt_test.go) prove which release pickAdoptableRelease chooses; nothing proved
		// the whole path: a CR recreated with a NEW UID and NO release-name label, beside the release
		// its predecessor installed. Before #322 the minted name (from the new UID) matched no
		// release, Observe reported ResourceExists=false, and the runtime installed a SECOND release
		// beside the first. #329 then showed that choosing right is not enough: the reconcile must
		// also COMPLETE. So this asserts the outcome: one release, not a second one, and a converged
		// second pass.
		//
		// The label is stripped on purpose. resolveReleaseName returns at its first branch when the
		// label exists, so a recreate that kept it would never reach adoption. That is why a live
		// cluster whose CR still carries the label (a0-openstack-d3) could not verify this.
		dy := dynamic.NewForConfigOrDie(c)
		var obj unstructured.Unstructured
		if err := decoder.DecodeFile(os.DirFS(filepath.Join(testdataPath, "compositions")), "focus.yaml", &obj); err != nil {
			t.Fatalf("decoding composition manifest: %v", err)
		}
		version := obj.GetLabels()["krateo.io/composition-version"]
		cli := dy.Resource(schema.GroupVersionResource{
			Group:    "composition.krateo.io",
			Version:  version,
			Resource: flect.Pluralize(strings.ToLower(obj.GetObjectKind().GroupVersionKind().Kind)),
		}).Namespace(obj.GetNamespace())

		u, err := cli.Get(ctx, obj.GetName(), metav1.GetOptions{})
		if err != nil {
			t.Fatalf("getting composition: %v", err)
		}
		original := compositionMeta.GetReleaseName(u)
		if original == "" {
			t.Fatalf("setup: the composition carries no release-name label, so there is no release to adopt")
		}
		oldUID := u.GetUID()

		clientset := kubernetes.NewForConfigOrDie(cfg.Client().RESTConfig())
		// release name -> latest revision, for every helm release in the namespace.
		releases := func() map[string]int {
			t.Helper()
			list, err := clientset.CoreV1().Secrets(obj.GetNamespace()).List(ctx, metav1.ListOptions{LabelSelector: "owner=helm"})
			if err != nil {
				t.Fatalf("listing helm release secrets: %v", err)
			}
			out := map[string]int{}
			for _, sec := range list.Items {
				if n, err := strconv.Atoi(sec.Labels["version"]); err == nil && n > out[sec.Labels["name"]] {
					out[sec.Labels["name"]] = n
				}
			}
			return out
		}
		before := releases()
		if before[original] == 0 {
			t.Fatalf("setup: release %q has no revision in %s (releases: %v)", original, obj.GetNamespace(), before)
		}

		// Orphan the release, the precondition the field produced: the CR goes, its release stays.
		if _, err := cli.Patch(ctx, u.GetName(), types.MergePatchType, []byte(`{"metadata":{"finalizers":null}}`), metav1.PatchOptions{}); err != nil {
			t.Fatalf("dropping finalizers: %v", err)
		}
		if err := cli.Delete(ctx, u.GetName(), metav1.DeleteOptions{}); err != nil {
			t.Fatalf("deleting the composition: %v", err)
		}
		gone := false
		for i := 0; i < 50 && !gone; i++ {
			if _, err := cli.Get(ctx, u.GetName(), metav1.GetOptions{}); errors.IsNotFound(err) {
				gone = true
			} else {
				time.Sleep(200 * time.Millisecond)
			}
		}
		if !gone {
			t.Fatalf("setup: the composition was not deleted")
		}
		// CONTROL: the release must have survived, or there is nothing to adopt and the assertions
		// below pass against a cluster with no release at all.
		if got := releases()[original]; got != before[original] {
			t.Fatalf("setup: deleting the CR changed release %q (revision %d -> %d)", original, before[original], got)
		}

		recreated := obj.DeepCopy()
		labels := recreated.GetLabels()
		delete(labels, compositionMeta.ReleaseNameLabel)
		recreated.SetLabels(labels)
		created, err := cli.Create(ctx, recreated, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("recreating the composition: %v", err)
		}
		// PRECONDITIONS that make the adoption path the one under test.
		if created.GetUID() == oldUID {
			t.Fatalf("precondition: the recreated CR kept UID %s", oldUID)
		}
		if got := compositionMeta.GetReleaseName(created); got != "" {
			t.Fatalf("precondition: the recreated CR carries release-name %q, so resolveReleaseName returns before adoption", got)
		}
		minted := compositionMeta.CalculateReleaseName(created, true)
		if minted == original {
			t.Fatalf("precondition: the name minted from the new UID equals %q, so adoption is not needed", original)
		}

		obs, err := handler.Observe(ctx, created)
		if err != nil {
			t.Fatalf("Observe on the recreated CR failed: %v. #329 was exactly this: the right release chosen, the reconcile never completing", err)
		}
		if !obs.ResourceExists {
			t.Fatalf("Observe reported ResourceExists=false: the runtime would install a SECOND release %q beside %q (#322)", minted, original)
		}
		cur, err := cli.Get(ctx, created.GetName(), metav1.GetOptions{})
		if err != nil {
			t.Fatalf("getting the recreated composition: %v", err)
		}
		if _, err := handleObservation(t, ctx, handler, obs, cur); err != nil {
			t.Fatalf("handling the observation: %v", err)
		}

		after := releases()
		if _, ok := after[minted]; ok {
			t.Fatalf("a SECOND release %q was installed beside %q: %v", minted, original, after)
		}
		if len(after) != len(before) {
			t.Errorf("the namespace's release set changed: before %v, after %v", before, after)
		}
		if after[original] < before[original] {
			t.Errorf("release %q went backwards: revision %d -> %d", original, before[original], after[original])
		}

		// CONVERGED: a second pass must report the resource present and up to date.
		cur, err = cli.Get(ctx, created.GetName(), metav1.GetOptions{})
		if err != nil {
			t.Fatalf("getting the recreated composition: %v", err)
		}
		obs2, err := handler.Observe(ctx, cur)
		if err != nil || !obs2.ResourceExists || !obs2.ResourceUpToDate {
			t.Errorf("the adopted composition did not converge: observation %+v, err %v", obs2, err)
		}
		t.Logf("adopted %q (the minted name would have been %q); releases before %v, after %v; label after: %q",
			original, minted, before, after, compositionMeta.GetReleaseName(cur))

		return context.WithValue(ctx, adoptedReleaseKey{}, original)
	}).Assess("Adoption: two releases claiming the composition (one failed, one deployed) is refused, never a third install (the d3 shape)", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		// a0-openstack-d3's measured state, reproduced: TWO releases recorded as belonging to one
		// composition, one FAILED and one deployed. pickAdoptableRelease must refuse
		// (ErrAmbiguousRelease) rather than choose either, and must never mint a THIRD release beside
		// them. The unit table proves the decision; this proves the whole path against real helm
		// storage, including that a FAILED release counts as a claimant (adoptableStates lists it).
		adopted, _ := ctx.Value(adoptedReleaseKey{}).(string)
		if adopted == "" {
			t.Fatal("setup: the adoption step did not hand over a release name")
		}
		dy := dynamic.NewForConfigOrDie(c)
		var obj unstructured.Unstructured
		if err := decoder.DecodeFile(os.DirFS(filepath.Join(testdataPath, "compositions")), "focus.yaml", &obj); err != nil {
			t.Fatalf("decoding composition manifest: %v", err)
		}
		version := obj.GetLabels()["krateo.io/composition-version"]
		cli := dy.Resource(schema.GroupVersionResource{
			Group:    "composition.krateo.io",
			Version:  version,
			Resource: flect.Pluralize(strings.ToLower(obj.GetObjectKind().GroupVersionKind().Kind)),
		}).Namespace(obj.GetNamespace())
		clientset := kubernetes.NewForConfigOrDie(cfg.Client().RESTConfig())
		releases := func() map[string]string { // release name -> status of its latest revision
			t.Helper()
			list, err := clientset.CoreV1().Secrets(obj.GetNamespace()).List(ctx, metav1.ListOptions{LabelSelector: "owner=helm"})
			if err != nil {
				t.Fatalf("listing helm release secrets: %v", err)
			}
			latest := map[string]int{}
			out := map[string]string{}
			for _, sec := range list.Items {
				n, _ := strconv.Atoi(sec.Labels["version"])
				if n >= latest[sec.Labels["name"]] {
					latest[sec.Labels["name"]] = n
					out[sec.Labels["name"]] = sec.Labels["status"]
				}
			}
			return out
		}
		orphanAndRecreate := func(label string) *unstructured.Unstructured {
			t.Helper()
			if _, err := cli.Patch(ctx, obj.GetName(), types.MergePatchType, []byte(`{"metadata":{"finalizers":null}}`), metav1.PatchOptions{}); err != nil {
				t.Fatalf("dropping finalizers: %v", err)
			}
			if err := cli.Delete(ctx, obj.GetName(), metav1.DeleteOptions{}); err != nil {
				t.Fatalf("deleting the composition: %v", err)
			}
			for i := 0; i < 50; i++ {
				if _, err := cli.Get(ctx, obj.GetName(), metav1.GetOptions{}); errors.IsNotFound(err) {
					break
				}
				time.Sleep(200 * time.Millisecond)
			}
			re := obj.DeepCopy()
			labels := re.GetLabels()
			delete(labels, compositionMeta.ReleaseNameLabel)
			if label != "" {
				labels[compositionMeta.ReleaseNameLabel] = label
			}
			re.SetLabels(labels)
			created, err := cli.Create(ctx, re, metav1.CreateOptions{})
			if err != nil {
				t.Fatalf("recreating the composition: %v", err)
			}
			return created
		}

		// 1. A SECOND release for the same composition, installed by the real Create path: the CR
		//    is recreated with its label naming a release that does not exist yet.
		const second = "focus-2"
		cr := orphanAndRecreate(second)
		obs, err := handler.Observe(ctx, cr)
		if err != nil {
			t.Fatalf("observe before installing %q: %v", second, err)
		}
		cur, _ := cli.Get(ctx, obj.GetName(), metav1.GetOptions{})
		if _, err := handleObservation(t, ctx, handler, obs, cur); err != nil {
			t.Fatalf("installing %q: %v", second, err)
		}
		if st := releases()[second]; st != "deployed" {
			t.Fatalf("setup: %q is %q, want deployed (releases: %v)", second, st, releases())
		}

		// 2. Make it FAILED in helm's own storage: the latest revision's payload and its label.
		markLatestRevisionFailed(t, ctx, clientset, obj.GetNamespace(), second)
		got := releases()
		if got[second] != "failed" || got[adopted] != "deployed" {
			t.Fatalf("setup: want %s=deployed and %s=failed, got %v", adopted, second, got)
		}
		before := len(got)

		// 3. Recreate with NO label: two releases claim it.
		recreated := orphanAndRecreate("")
		minted := compositionMeta.CalculateReleaseName(recreated, true)
		_, err = handler.Observe(ctx, recreated)
		if err == nil {
			t.Fatalf("Observe chose a release although %q (deployed) and %q (failed) both claim the composition", adopted, second)
		}
		if !stderrors.Is(err, ErrAmbiguousRelease) {
			t.Fatalf("Observe failed, but not with ErrAmbiguousRelease: %v", err)
		}
		for _, name := range []string{adopted, second} {
			if !strings.Contains(err.Error(), name) {
				t.Errorf("the refusal does not name %q, so an operator cannot see which releases collide: %v", name, err)
			}
		}
		after := releases()
		if _, ok := after[minted]; ok || len(after) != before {
			t.Fatalf("a release was installed during the refusal (minted %q): before %d releases, after %v", minted, before, after)
		}
		t.Logf("refused as ambiguous: %v; releases unchanged: %v", err, after)

		// Cleanup for the Delete step: remove the second release and point the CR at the first.
		hc, err := helmv3.NewClient(cfg.Client().RESTConfig(), helmv3.WithNamespace(obj.GetNamespace()))
		if err != nil {
			t.Fatalf("helm client: %v", err)
		}
		if err := hc.Uninstall(ctx, second, &helmconfig.UninstallConfig{IgnoreNotFound: true}); err != nil {
			t.Fatalf("uninstalling %q: %v", second, err)
		}
		patch := fmt.Sprintf(`{"metadata":{"labels":{%q:%q}}}`, compositionMeta.ReleaseNameLabel, adopted)
		if _, err := cli.Patch(ctx, obj.GetName(), types.MergePatchType, []byte(patch), metav1.PatchOptions{}); err != nil {
			t.Fatalf("restoring the release-name label: %v", err)
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
		// The release the deletion must remove. An ADOPTED composition need not carry the label:
		// Observe is a pure read, and nothing persists the adopted name on a pass with no drift. So
		// the adoption step hands the name over. Without that, an empty label made the check below
		// select "name=" and pass against a release that was never removed.
		expectedRelease := compositionMeta.GetReleaseName(u)
		if adopted, ok := ctx.Value(adoptedReleaseKey{}).(string); ok && expectedRelease == "" {
			expectedRelease = adopted
		}
		if expectedRelease == "" {
			t.Fatal("no release name to check the deletion against")
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
			LabelSelector: "name=" + expectedRelease + ",owner=helm",
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

// markLatestRevisionFailed sets a release's latest revision to FAILED in helm's own storage: the
// "status" label on the secret and info.status in the payload. The payload is decoded the way helm's
// storage driver encodes it: base64, then gzip if the magic header is present, then JSON.
func markLatestRevisionFailed(t *testing.T, ctx context.Context, cs kubernetes.Interface, ns, name string) {
	t.Helper()
	list, err := cs.CoreV1().Secrets(ns).List(ctx, metav1.ListOptions{LabelSelector: "owner=helm,name=" + name})
	if err != nil || len(list.Items) == 0 {
		t.Fatalf("finding release %q: %v (%d secrets)", name, err, len(list.Items))
	}
	latest := list.Items[0]
	for _, sec := range list.Items {
		a, _ := strconv.Atoi(sec.Labels["version"])
		b, _ := strconv.Atoi(latest.Labels["version"])
		if a > b {
			latest = sec
		}
	}
	raw, err := base64.StdEncoding.DecodeString(string(latest.Data["release"]))
	if err != nil {
		t.Fatalf("decoding release payload: %v", err)
	}
	gzipped := len(raw) > 2 && raw[0] == 0x1f && raw[1] == 0x8b
	if gzipped {
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("gunzip: %v", err)
		}
		if raw, err = io.ReadAll(zr); err != nil {
			t.Fatalf("gunzip: %v", err)
		}
	}
	var rel map[string]any
	if err := json.Unmarshal(raw, &rel); err != nil {
		t.Fatalf("parsing release: %v", err)
	}
	info, _ := rel["info"].(map[string]any)
	if info == nil {
		t.Fatalf("release %q has no info", name)
	}
	info["status"] = "failed"
	out, err := json.Marshal(rel)
	if err != nil {
		t.Fatal(err)
	}
	if gzipped {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		_, _ = zw.Write(out)
		_ = zw.Close()
		out = buf.Bytes()
	}
	latest.Data["release"] = []byte(base64.StdEncoding.EncodeToString(out))
	latest.Labels["status"] = "failed"
	if _, err := cs.CoreV1().Secrets(ns).Update(ctx, &latest, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("writing release %q back: %v", name, err)
	}
}

// adoptedReleaseKey carries the adopted release name from the adoption step to the deletion step.
type adoptedReleaseKey struct{}

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

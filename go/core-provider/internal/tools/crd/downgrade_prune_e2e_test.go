//go:build e2e

// Does a composition-version DOWNGRADE lose spec fields?
//
// The version-migration step (compositiondefinitions.Update -> getters.UpdateCompositionsVersion)
// relabels every owned instance by GET-ing it through the TARGET version's endpoint, setting the
// krateo.io/composition-version label, and UPDATE-ing it back through that same endpoint.
//
// On an UPGRADE the target schema is the newer, usually wider one, so the round trip is lossless.
// On a DOWNGRADE the target schema is the older, possibly NARROWER one — and a CRD write is pruned
// against the schema of the version in the request. The question this answers is whether that
// prunes spec fields the instance legitimately carries from the newer version, silently, as a side
// effect of a relabel that was only ever meant to touch metadata.
//
// The "vacuum" storage version preserves unknown fields, which is what makes the answer
// non-obvious: if pruning happened only at the storage layer the data would survive.
//
//	Run: KUBEBUILDER_ASSETS=$(setup-envtest use -p path 1.36.0) \
//	     go test -tags e2e ./internal/tools/crd/ -run Downgrade -v
package crd

import (
	"context"
	"os"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"time"
)

func strptr(b bool) *bool { return &b }

// wideThenNarrow builds a CRD shaped like a generated composition CRD mid-downgrade: two served
// versions whose spec schemas differ, plus the non-served preserve-unknown "vacuum" storage
// version that AppendVersion installs.
func wideThenNarrowCRD() *apiextensionsv1.CustomResourceDefinition {
	strObj := func(props map[string]apiextensionsv1.JSONSchemaProps) *apiextensionsv1.CustomResourceValidation {
		return &apiextensionsv1.CustomResourceValidation{
			OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
				Type: "object",
				Properties: map[string]apiextensionsv1.JSONSchemaProps{
					"apiVersion": {Type: "string"},
					"kind":       {Type: "string"},
					"metadata":   {Type: "object"},
					"spec":       {Type: "object", Properties: props},
					"status":     {Type: "object", XPreserveUnknownFields: strptr(true)},
				},
			},
		}
	}

	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "widgets.composition.krateo.io"},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "composition.krateo.io",
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural: "widgets", Singular: "widget", Kind: "Widget", ListKind: "WidgetList",
			},
			Scope:      apiextensionsv1.NamespaceScoped,
			Conversion: &apiextensionsv1.CustomResourceConversion{Strategy: apiextensionsv1.NoneConverter},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
				{
					// The OLDER chart: spec has replicas only.
					Name: "v1-0-0", Served: true, Storage: false,
					Schema: strObj(map[string]apiextensionsv1.JSONSchemaProps{
						"replicas": {Type: "integer"},
					}),
				},
				{
					// The NEWER chart: adds ingress, which v1-0-0 knows nothing about.
					Name: "v2-0-0", Served: true, Storage: false,
					Schema: strObj(map[string]apiextensionsv1.JSONSchemaProps{
						"replicas": {Type: "integer"},
						"ingress": {Type: "object", Properties: map[string]apiextensionsv1.JSONSchemaProps{
							"host": {Type: "string"},
							"tls":  {Type: "boolean"},
						}},
					}),
				},
				{
					// Exactly what generation.AppendVersion installs.
					Name: "vacuum", Served: false, Storage: true,
					Schema: &apiextensionsv1.CustomResourceValidation{
						OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
							Type: "object",
							Properties: map[string]apiextensionsv1.JSONSchemaProps{
								"apiVersion": {Type: "string"},
								"kind":       {Type: "string"},
								"metadata":   {Type: "object"},
								"spec":       {Type: "object", XPreserveUnknownFields: strptr(true)},
								"status":     {Type: "object", XPreserveUnknownFields: strptr(true)},
							},
						},
					},
				},
			},
		},
	}
}

func TestE2E_DowngradeMigrationPrunesSpecFields(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via setup-envtest")
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest apiserver: %v", err)
	}
	defer func() { _ = env.Stop() }()

	ctx := context.Background()
	extc, err := apiextensionsclient.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("apiextensions client: %v", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("dynamic client: %v", err)
	}

	if _, err := extc.ApiextensionsV1().CustomResourceDefinitions().
		Create(ctx, wideThenNarrowCRD(), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create CRD: %v", err)
	}

	wide := schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v2-0-0", Resource: "widgets"}
	narrow := schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1-0-0", Resource: "widgets"}

	// Wait for both endpoints to be established.
	if err := wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 60*time.Second, true,
		func(ctx context.Context) (bool, error) {
			if _, err := dyn.Resource(wide).Namespace("default").List(ctx, metav1.ListOptions{}); err != nil {
				return false, nil
			}
			if _, err := dyn.Resource(narrow).Namespace("default").List(ctx, metav1.ListOptions{}); err != nil {
				return false, nil
			}
			return true, nil
		}); err != nil {
		t.Fatalf("CRD endpoints never became available: %v", err)
	}

	// An instance created at the NEWER version, carrying a field only that version declares.
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "composition.krateo.io/v2-0-0",
		"kind":       "Widget",
		"metadata": map[string]any{
			"name":      "w1",
			"namespace": "default",
			"labels":    map[string]any{"krateo.io/composition-version": "v2-0-0"},
		},
		"spec": map[string]any{
			"replicas": int64(3),
			"ingress":  map[string]any{"host": "app.example.com", "tls": true},
		},
	}}
	if _, err := dyn.Resource(wide).Namespace("default").Create(ctx, obj, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create widget at v2-0-0: %v", err)
	}

	// Sanity: the instrument can see the field at all. Without this the test could "pass" by
	// finding nothing because nothing was ever stored.
	got, err := dyn.Resource(wide).Namespace("default").Get(ctx, "w1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get at v2-0-0: %v", err)
	}
	if host, found, _ := unstructured.NestedString(got.Object, "spec", "ingress", "host"); !found || host != "app.example.com" {
		t.Fatalf("precondition failed: spec.ingress.host not stored at the wide version (found=%v host=%q) — "+
			"this test proves nothing unless the field exists before the downgrade", found, host)
	}

	// ---- Exactly what getters.updateCompositionWithRetry does, with gvr = the DOWNGRADE target.
	cur, err := dyn.Resource(narrow).Namespace("default").Get(ctx, "w1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get through the narrow (downgrade target) endpoint: %v", err)
	}
	labels := cur.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels["krateo.io/composition-version"] = "v1-0-0"
	cur.SetLabels(labels)
	if _, err := dyn.Resource(narrow).Namespace("default").Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("relabel update through the narrow endpoint: %v", err)
	}
	// ---- end migration replica

	after, err := dyn.Resource(wide).Namespace("default").Get(ctx, "w1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get at v2-0-0 after the downgrade relabel: %v", err)
	}
	host, found, _ := unstructured.NestedString(after.Object, "spec", "ingress", "host")
	replicas, _, _ := unstructured.NestedInt64(after.Object, "spec", "replicas")

	t.Logf("after downgrade relabel: spec.replicas=%d spec.ingress.host=%q present=%v", replicas, host, found)

	if !found {
		t.Errorf("DATA LOSS: spec.ingress was pruned by the version-migration relabel.\n"+
			"The migration only set a label, but it did so with a read-modify-write through the "+
			"DOWNGRADE target endpoint (v1-0-0), whose schema does not declare spec.ingress. The "+
			"apiserver prunes unknown fields against the schema of the version in the request, so "+
			"the field is gone from storage — the vacuum storage version's "+
			"x-kubernetes-preserve-unknown-fields does not save it, because pruning happens before "+
			"storage. spec.replicas survived (=%d) because v1-0-0 declares it.", replicas)
	}
}

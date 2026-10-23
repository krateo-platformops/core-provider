//go:build e2e

// #188 apiserver-level proof. Runs only with -tags e2e against a real cluster (KUBECONFIG).
// Reuses e2eClients/waitCRDEstablished/createEventually/compositionGroup from e2e_test.go.
//
// The resolution chosen for #188 is "allow the transition, report the loss" — a narrowing
// migration must NOT be blocked, because the usual narrowing transition is a rollback and the
// fields being dropped are ones the older chart cannot use. What was unacceptable was the
// silence. So this asserts BOTH halves:
//
//   - the transition still happens and still prunes (not a regression: the documented, accepted
//     behaviour), and
//   - the dropped subtree is recorded, with its VALUES, on the object itself — in the same write
//     that drops it — so an operator can put it back by hand.
//
// It lives under internal/tools/crd/ because that is the only tree the CI e2e job runs, and in
// an EXTERNAL test package because getters -> deploy -> crd would otherwise be an import cycle.
package fieldloss_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/krateo-platformops/core-provider/internal/controllers/compositiondefinitions/helpers/getters"
	"github.com/krateo-platformops/core-provider/internal/tools/crd/fieldloss"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	versionLabel     = "krateo.io/composition-version"
	compositionGroup = "composition.krateo.io"
)

// e2eClients mirrors the helper in internal/tools/crd/e2e_test.go, which is unexported and in a
// package this one cannot import (the cycle noted above).
func e2eClients(t *testing.T) (client.Client, dynamic.Interface) {
	t.Helper()
	rc, err := clientcmd.BuildConfigFromFlags("", os.Getenv("KUBECONFIG"))
	if err != nil {
		t.Fatalf("rest config: %v", err)
	}
	sc := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(sc)
	_ = apiextensionsv1.AddToScheme(sc)
	cl, err := client.New(rc, client.Options{Scheme: sc})
	if err != nil {
		t.Fatalf("kube client: %v", err)
	}
	dyn, err := dynamic.NewForConfig(rc)
	if err != nil {
		t.Fatalf("dynamic client: %v", err)
	}
	return cl, dyn
}

func waitCRDEstablished(t *testing.T, cl client.Client, name string) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := cl.Get(context.Background(), types.NamespacedName{Name: name}, crd); err == nil {
			for _, c := range crd.Status.Conditions {
				if c.Type == apiextensionsv1.Established && c.Status == apiextensionsv1.ConditionTrue {
					return
				}
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("CRD %s not established", name)
}

// createEventually tolerates the window between a CRD reporting Established and its resource
// actually being servable (#138). Only discovery lag is retried: a schema rejection is a real
// result and must fail immediately rather than after a minute.
func createEventually(t *testing.T, ctx context.Context, ri dynamic.ResourceInterface, obj *unstructured.Unstructured) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if _, err := ri.Create(ctx, obj, metav1.CreateOptions{}); err == nil {
			return
		} else if !isDiscoveryLag(err) {
			t.Fatalf("create: %v", err)
		} else {
			lastErr = err
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("create never succeeded: %v", lastErr)
}

func isDiscoveryLag(err error) bool {
	var nkme *meta.NoKindMatchError
	if errors.As(err, &nkme) || meta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
		return true
	}
	return strings.Contains(err.Error(), "There can be a delay between when CustomResourceDefinitions are created")
}

func narrowingCRD() *apiextensionsv1.CustomResourceDefinition {
	b := func(v bool) *bool { return &v }
	root := func(props map[string]apiextensionsv1.JSONSchemaProps) *apiextensionsv1.CustomResourceValidation {
		return &apiextensionsv1.CustomResourceValidation{
			OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
				Type: "object",
				Properties: map[string]apiextensionsv1.JSONSchemaProps{
					"apiVersion": {Type: "string"},
					"kind":       {Type: "string"},
					"metadata":   {Type: "object"},
					"spec":       {Type: "object", Properties: props},
					"status":     {Type: "object", XPreserveUnknownFields: b(true)},
				},
			},
		}
	}
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "gizmos." + compositionGroup},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: compositionGroup,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural: "gizmos", Singular: "gizmo", Kind: "Gizmo", ListKind: "GizmoList",
			},
			Scope:      apiextensionsv1.NamespaceScoped,
			Conversion: &apiextensionsv1.CustomResourceConversion{Strategy: apiextensionsv1.NoneConverter},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
				{Name: "v1-0-0", Served: true, Storage: false, // the OLDER, narrower chart
					Schema: root(map[string]apiextensionsv1.JSONSchemaProps{"replicas": {Type: "integer"}})},
				{Name: "v2-0-0", Served: true, Storage: false, // the newer chart
					Schema: root(map[string]apiextensionsv1.JSONSchemaProps{
						"replicas": {Type: "integer"},
						"ingress": {Type: "object", Properties: map[string]apiextensionsv1.JSONSchemaProps{
							"host": {Type: "string"},
							"tls":  {Type: "boolean"},
						}},
					})},
				{Name: "vacuum", Served: false, Storage: true,
					Schema: &apiextensionsv1.CustomResourceValidation{
						OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
							Type: "object",
							Properties: map[string]apiextensionsv1.JSONSchemaProps{
								"apiVersion": {Type: "string"},
								"kind":       {Type: "string"},
								"metadata":   {Type: "object"},
								"spec":       {Type: "object", XPreserveUnknownFields: b(true)},
								"status":     {Type: "object", XPreserveUnknownFields: b(true)},
							},
						},
					}},
			},
		},
	}
}

func TestE2E_NarrowingMigrationRecordsWhatItDrops(t *testing.T) {
	cl, dyn := e2eClients(t)
	ctx := context.Background()

	crd := narrowingCRD()
	if err := cl.Create(ctx, crd); err != nil {
		t.Fatalf("create CRD: %v", err)
	}
	t.Cleanup(func() { _ = cl.Delete(ctx, crd) })
	waitCRDEstablished(t, cl, crd.Name)

	wide := schema.GroupVersionResource{Group: compositionGroup, Version: "v2-0-0", Resource: "gizmos"}
	narrow := schema.GroupVersionResource{Group: compositionGroup, Version: "v1-0-0", Resource: "gizmos"}

	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": compositionGroup + "/v2-0-0",
		"kind":       "Gizmo",
		"metadata": map[string]any{
			"name": "g1", "namespace": "default",
			"labels": map[string]any{versionLabel: "v2-0-0"},
		},
		"spec": map[string]any{
			"replicas": int64(3),
			"ingress":  map[string]any{"host": "app.example.com", "tls": true},
		},
	}}
	createEventually(t, ctx, dyn.Resource(wide).Namespace("default"), obj)

	// Precondition: the field is really there before the migration. Without this the test could
	// "pass" by finding nothing because nothing was ever stored.
	got, err := dyn.Resource(wide).Namespace("default").Get(ctx, "g1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get at the wide version: %v", err)
	}
	if host, found, _ := unstructured.NestedString(got.Object, "spec", "ingress", "host"); !found || host != "app.example.com" {
		t.Fatalf("precondition failed: spec.ingress.host absent before the migration (found=%v host=%q) — "+
			"this test proves nothing unless the field exists first", found, host)
	}

	// Arm detection exactly as the controller does: the structural schema of the version being
	// migrated TO.
	var target *apiextensionsv1.JSONSchemaProps
	for _, v := range crd.Spec.Versions {
		if v.Name == "v1-0-0" {
			target = v.Schema.OpenAPIV3Schema
		}
	}
	st, err := fieldloss.Structural(target)
	if err != nil {
		t.Fatalf("Structural: %v", err)
	}

	var reported []fieldloss.Report
	var stashedFlags []bool
	opts := getters.MigrationOptions{
		TargetSchema: st,
		OnLoss: func(_ *unstructured.Unstructured, rep fieldloss.Report, stashed bool) {
			reported = append(reported, rep)
			stashedFlags = append(stashedFlags, stashed)
		},
	}

	// The migration itself: relabel v2-0-0 -> v1-0-0 through the narrow (target) endpoint.
	if err := getters.UpdateCompositionsVersion(ctx, dyn, narrow, "v2-0-0", "v1-0-0",
		getters.DefinitionRef{}, opts); err != nil {
		t.Fatalf("UpdateCompositionsVersion: %v", err)
	}

	// --- Half 1: the transition happened and is NOT blocked.
	after, err := dyn.Resource(wide).Namespace("default").Get(ctx, "g1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get after migration: %v", err)
	}
	if lbl := after.GetLabels()[versionLabel]; lbl != "v1-0-0" {
		t.Errorf("migration did not relabel the instance: %s=%q, want v1-0-0", versionLabel, lbl)
	}
	if replicas, _, _ := unstructured.NestedInt64(after.Object, "spec", "replicas"); replicas != 3 {
		t.Errorf("a field the target version DOES declare was lost: spec.replicas=%d, want 3", replicas)
	}

	// The prune still happens. This is the accepted cost of the chosen resolution, asserted so a
	// future change that silently starts preserving (or blocking) is caught rather than welcomed.
	if _, found, _ := unstructured.NestedString(after.Object, "spec", "ingress", "host"); found {
		t.Errorf("expected the narrowing write to prune spec.ingress; it survived — the premise " +
			"of #188 no longer holds and this test needs rewriting, not deleting")
	}

	// --- Half 2: the loss was recorded, with values, on the object itself.
	raw, ok := after.GetAnnotations()[getters.PrunedFieldsAnnotation]
	if !ok {
		t.Fatalf("no %s annotation: the loss happened silently, which is the whole defect (#188). "+
			"annotations=%v", getters.PrunedFieldsAnnotation, after.GetAnnotations())
	}
	if after.GetAnnotations()[getters.PrunedFieldsTruncatedAnnotation] == "true" {
		t.Errorf("a two-field subtree was reported as too large to record; the size guard is wrong")
	}

	var back map[string]any
	if err := json.Unmarshal([]byte(raw), &back); err != nil {
		t.Fatalf("%s is not valid JSON (%v): %s", getters.PrunedFieldsAnnotation, err, raw)
	}
	spec, _ := back["spec"].(map[string]any)
	ing, _ := spec["ingress"].(map[string]any)
	if ing["host"] != "app.example.com" {
		t.Errorf("the recorded subtree lost the VALUE, which is what makes the loss recoverable: %s", raw)
	}
	if ing["tls"] != true {
		t.Errorf("the recorded subtree is incomplete (tls missing): %s", raw)
	}

	// --- Half 3: the caller was told, so the controller can raise an Event.
	if len(reported) != 1 {
		t.Fatalf("OnLoss called %d times, want exactly 1", len(reported))
	}
	if len(reported[0].Paths) != 1 || reported[0].Paths[0] != "spec.ingress" {
		t.Errorf("reported paths = %v, want [spec.ingress]", reported[0].Paths)
	}
	if !stashedFlags[0] {
		t.Errorf("OnLoss reported the subtree as not stashed, but the annotation is present")
	}
}

// A widening migration must stay completely silent: no annotation, no event, no noise on the
// overwhelmingly common path. Without this, the feature would tag every ordinary upgrade.
func TestE2E_WideningMigrationReportsNothing(t *testing.T) {
	cl, dyn := e2eClients(t)
	ctx := context.Background()

	crd := narrowingCRD()
	crd.Name = "widenings." + compositionGroup
	crd.Spec.Names = apiextensionsv1.CustomResourceDefinitionNames{
		Plural: "widenings", Singular: "widening", Kind: "Widening", ListKind: "WideningList",
	}
	if err := cl.Create(ctx, crd); err != nil {
		t.Fatalf("create CRD: %v", err)
	}
	t.Cleanup(func() { _ = cl.Delete(ctx, crd) })
	waitCRDEstablished(t, cl, crd.Name)

	narrow := schema.GroupVersionResource{Group: compositionGroup, Version: "v1-0-0", Resource: "widenings"}
	wide := schema.GroupVersionResource{Group: compositionGroup, Version: "v2-0-0", Resource: "widenings"}

	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": compositionGroup + "/v1-0-0",
		"kind":       "Widening",
		"metadata": map[string]any{
			"name": "w1", "namespace": "default",
			"labels": map[string]any{versionLabel: "v1-0-0"},
		},
		"spec": map[string]any{"replicas": int64(2)},
	}}
	createEventually(t, ctx, dyn.Resource(narrow).Namespace("default"), obj)

	var target *apiextensionsv1.JSONSchemaProps
	for _, v := range crd.Spec.Versions {
		if v.Name == "v2-0-0" {
			target = v.Schema.OpenAPIV3Schema
		}
	}
	st, err := fieldloss.Structural(target)
	if err != nil {
		t.Fatalf("Structural: %v", err)
	}

	called := 0
	if err := getters.UpdateCompositionsVersion(ctx, dyn, wide, "v1-0-0", "v2-0-0",
		getters.DefinitionRef{}, getters.MigrationOptions{
			TargetSchema: st,
			OnLoss:       func(*unstructured.Unstructured, fieldloss.Report, bool) { called++ },
		}); err != nil {
		t.Fatalf("UpdateCompositionsVersion: %v", err)
	}

	after, err := dyn.Resource(wide).Namespace("default").Get(ctx, "w1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get after migration: %v", err)
	}
	if called != 0 {
		t.Errorf("OnLoss fired %d times on a WIDENING migration; it must be silent", called)
	}
	if _, ok := after.GetAnnotations()[getters.PrunedFieldsAnnotation]; ok {
		t.Errorf("a widening migration left a %s annotation: %v",
			getters.PrunedFieldsAnnotation, after.GetAnnotations())
	}
	if lbl := after.GetLabels()[versionLabel]; lbl != "v2-0-0" {
		t.Errorf("widening migration did not relabel: %s=%q", versionLabel, lbl)
	}
}

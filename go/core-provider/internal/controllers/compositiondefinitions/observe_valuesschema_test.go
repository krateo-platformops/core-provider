package compositiondefinitions

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/cdreason"
	"github.com/krateo-platformops/core-provider/internal/tools/chart"
	"github.com/krateo-platformops/core-provider/internal/tools/chart/chartfs"
	crdutils "github.com/krateo-platformops/core-provider/internal/tools/crd/generation"
	rtv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	"github.com/krateo-platformops/provider-runtime/pkg/reconciler"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// observeSchemaless runs Observe on a NEW definition of a chart with NO values.schema.json, carrying
// annotations, optionally being deleted.
func observeSchemaless(t *testing.T, annotations map[string]string, deleting bool) (*compositiondefinitionsv1alpha1.CompositionDefinition, reconciler.ExternalObservation, error) {
	t.Helper()
	tgz := chartTGZ(t, noRootSchema)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(tgz) }))
	t.Cleanup(srv.Close)

	cd := &compositiondefinitionsv1alpha1.CompositionDefinition{}
	cd.SetName("contradicting")
	cd.SetNamespace("krateo-system")
	cd.SetAnnotations(annotations)
	cd.Spec.Chart = &compositiondefinitionsv1alpha1.ChartInfo{Url: srv.URL + "/contradicting-0.1.0.tgz", Version: "0.1.0"}
	if deleting {
		now := metav1.Now()
		cd.SetDeletionTimestamp(&now)
		cd.SetFinalizers([]string{"finalizer.managedresource.krateo.io"})
	}
	kc := clientfake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(cd).WithStatusSubresource(cd).Build()
	e := &external{
		kube: kc, mgmt: kc, apiReader: kc,
		mgmtDynamic: dynfake.NewSimpleDynamicClient(scheme(t)),
		client:      k8sfake.NewSimpleClientset(),
		log:         logging.NewNopLogger(),
		pluralizer:  notFoundPluralizer{},
	}
	obs, err := e.Observe(context.Background(), cd)
	return cd, obs, err
}

// #568 child 6 (D1: opt-in, annotation krateo.io/values-schema: infer), through Observe.
func TestObserve_ValuesSchemaOptIn(t *testing.T) {
	t.Run("control: no file and no opt-in is ValuesSchemaMissing, and its message names the opt-in", func(t *testing.T) {
		cd, _, err := observeSchemaless(t, nil, false)
		got := cd.GetCondition(rtv1.TypeReady)
		if !cdreason.ValuesSchemaMissing.Is(got) {
			t.Fatalf("Ready = %s/%s (%q), want False/ValuesSchemaMissing (err %v)", got.Status, got.Reason, got.Message, err)
		}
		if !strings.Contains(got.Message, chart.ValuesSchemaAnnotation+": "+chart.ValuesSchemaInfer) {
			t.Errorf("message %q does not name the opt-in", got.Message)
		}
	})

	t.Run("a mistyped opt-in is not one, and the message names the value and the accepted one", func(t *testing.T) {
		cd, _, _ := observeSchemaless(t, map[string]string{chart.ValuesSchemaAnnotation: "Infer"}, false)
		got := cd.GetCondition(rtv1.TypeReady)
		if !cdreason.ValuesSchemaMissing.Is(got) {
			t.Fatalf("Ready = %s/%s, want False/ValuesSchemaMissing", got.Status, got.Reason)
		}
		if !strings.Contains(got.Message, `"Infer"`) || !strings.Contains(got.Message, `"infer"`) {
			t.Errorf("message %q does not name the mistyped value and the accepted one", got.Message)
		}
	})

	t.Run("opted in: the schema is inferred and Observe reaches CRD-not-found, which Create follows", func(t *testing.T) {
		cd, obs, err := observeSchemaless(t, map[string]string{chart.ValuesSchemaAnnotation: chart.ValuesSchemaInfer}, false)
		got := cd.GetCondition(rtv1.TypeReady)
		if err != nil || obs.ResourceExists || !strings.Contains(got.Message, "does not exists yet") {
			t.Fatalf("Observe = exists %v, err %v, Ready %s/%s (%q): want the CRD-not-found return", obs.ResourceExists, err, got.Status, got.Reason, got.Message)
		}
		if cdreason.ValuesSchemaMissing.Is(got) {
			t.Error("an opted-in definition still reads ValuesSchemaMissing")
		}
	})

	t.Run("a definition being deleted is unaffected: no inference, the CRD-not-found teardown answer", func(t *testing.T) {
		cd, obs, err := observeSchemaless(t, map[string]string{chart.ValuesSchemaAnnotation: chart.ValuesSchemaInfer}, true)
		// The same answer TestObserveReportsAMissingValuesSchema's deleting case gets with no opt-in: the
		// CRD is absent, so the external resource is gone and the finalizer can be removed.
		if err != nil || obs.ResourceExists {
			t.Fatalf("a deleting definition: exists %v, err %v; want the CRD-not-found deletion answer", obs.ResourceExists, err)
		}
		if got := cd.GetCondition(rtv1.TypeReady); cdreason.ValuesSchemaMissing.Is(got) {
			t.Fatalf("a deleting definition stopped on ValuesSchemaMissing: %q", got.Message)
		}
	})
}

// The inferred schema becomes a CRD through core-provider's OWN generator (which normalises on top of
// crdgen), with no constraint-losing warning: what Create would apply for an opted-in definition.
func TestInferredSchemaGeneratesACRD(t *testing.T) {
	pkg, err := chartfs.FromReader(bytes.NewReader(chartTGZ(t, noRootSchema)), "test://contradicting-0.1.0.tgz")
	if err != nil {
		t.Fatal(err)
	}
	s, err := chart.SpecSchema(pkg.FS(), pkg.RootDir(), true)
	if err != nil || s.Source != chart.SchemaInferred {
		t.Fatalf("SpecSchema = %v, %v; want an inferred schema", s.Source, err)
	}
	gen, err := crdutils.GenerateCRDWithWarnings(s.Bytes, schema.GroupVersionKind{Group: "composition.krateo.io", Version: "v0-1-0", Kind: "Contradicting"})
	if err != nil {
		t.Fatalf("generating the CRD from the inferred schema: %v", err)
	}
	if gen.CRD == nil {
		t.Fatal("no CRD generated")
	}
	for _, w := range gen.Warnings {
		if w.Kind.LosesEnforcement() {
			t.Errorf("an inferred schema lost a constraint it never had: %+v", w)
		}
	}
}

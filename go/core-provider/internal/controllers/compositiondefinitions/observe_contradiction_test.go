package compositiondefinitions

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/cdreason"
	rtv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	"github.com/krateo-platformops/provider-runtime/pkg/reconciler"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// noRootSchema, passed as chartTGZ's schema, builds a chart with no values.schema.json at all.
const noRootSchema = ""

// schemaIsADirectory, passed as chartTGZ's schema, builds a chart whose values.schema.json is a
// directory: the path exists, and reading it fails with an error that is not fs.ErrNotExist.
const schemaIsADirectory = "\x00directory"

// chartTGZ is a minimal chart named "contradicting", version 0.1.0, with values.schema.json schema.
func chartTGZ(t *testing.T, valuesSchema string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range map[string]string{
		"contradicting/Chart.yaml":         "apiVersion: v2\nname: contradicting\nversion: 0.1.0\n",
		"contradicting/values.yaml":        "{}\n",
		"contradicting/values.schema.json": valuesSchema,
		"contradicting/templates/cm.yaml":  "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: {{ .Release.Name }}\n",
	} {
		if strings.HasSuffix(name, "values.schema.json") && body == noRootSchema {
			continue // a chart that ships no root schema at all (#566)
		}
		if strings.HasSuffix(name, "values.schema.json") && body == schemaIsADirectory {
			if err := tw.WriteHeader(&tar.Header{Name: name + "/", Mode: 0o755, Typeflag: tar.TypeDir}); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// notFoundPluralizer answers as the management cluster does for a Kind it does not serve yet.
type notFoundPluralizer struct{}

func (notFoundPluralizer) GVKtoGVR(gvk schema.GroupVersionKind) (schema.GroupVersionResource, error) {
	return schema.GroupVersionResource{}, apierrors.NewNotFound(schema.GroupResource{Group: gvk.Group, Resource: gvk.Kind}, gvk.Kind)
}

// observeChart runs Observe on a NEW definition (no status) whose chart, served over HTTP, carries
// valuesSchema. Nothing else exists: no CRD, no controller.
func observeChart(t *testing.T, valuesSchema string) (*compositiondefinitionsv1alpha1.CompositionDefinition, reconciler.ExternalObservation, error) {
	t.Helper()
	return observeChartDeleting(t, valuesSchema, false)
}

func observeChartDeleting(t *testing.T, valuesSchema string, deleting bool) (*compositiondefinitionsv1alpha1.CompositionDefinition, reconciler.ExternalObservation, error) {
	t.Helper()
	tgz := chartTGZ(t, valuesSchema)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(tgz) }))
	t.Cleanup(srv.Close)

	cd := &compositiondefinitionsv1alpha1.CompositionDefinition{}
	cd.SetName("contradicting")
	cd.SetNamespace("krateo-system")
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

// Frontend's point 2, against Observe itself rather than the helper: a NEW definition whose chart
// carries a contradicting default reports Ready=False/SchemaDefaultContradiction with the pointer,
// returns a ReadyAssessed error (so Synced=False/ReconcileError carries it and the runtime keeps this
// Ready), and has no status.kind: nothing was generated. Without the early check, this definition
// fell through CRD-not-found to Create, which reports Creating with no end.
//
// The control is the same chart without the contradiction. It must reach the CRD-not-found return
// (the message only that return writes), so a pass on the first case cannot be an earlier failure,
// say an unreadable chart, that happens to leave some other Ready.
func TestObserveReportsASchemaDefaultContradiction(t *testing.T) {
	t.Run("contradicting default: Ready=False/SchemaDefaultContradiction", func(t *testing.T) {
		cd, _, err := observeChart(t, contradictingSchema)
		got := cd.GetCondition(rtv1.TypeReady)
		if !cdreason.SchemaDefaultContradiction.Is(got) {
			t.Fatalf("Ready = %s/%s (%q), want False/SchemaDefaultContradiction (Observe err: %v)", got.Status, got.Reason, got.Message, err)
		}
		if !strings.Contains(got.Message, "#/properties/security") {
			t.Errorf("Ready message %q does not name the JSON pointer", got.Message)
		}
		if err == nil || !reconciler.IsReadyAssessed(err) {
			t.Errorf("Observe err = %v, want a ReadyAssessed error", err)
		}
		if cd.Status.Kind != "" || cd.Status.ApiVersion != "" || cd.Status.Resource != "" {
			t.Errorf("status names %q %q %q: nothing was generated, a new definition has none", cd.Status.ApiVersion, cd.Status.Kind, cd.Status.Resource)
		}
	})
	t.Run("a definition being deleted is not stopped: its teardown must not depend on its chart", func(t *testing.T) {
		cd, obs, err := observeChartDeleting(t, contradictingSchema, true)
		got := cd.GetCondition(rtv1.TypeReady)
		if cdreason.SchemaDefaultContradiction.Is(got) || (err != nil && strings.Contains(err.Error(), "cannot become a CRD")) {
			t.Fatalf("a deleting definition stopped on its chart's schema: Ready %s/%s, err %v", got.Status, got.Reason, err)
		}
		// Reached the deletion answer: no CRD, so the external resource is gone and Delete's finalizer
		// can be removed. Asserted, so this case cannot pass on an unrelated early error.
		if err != nil || obs.ResourceExists {
			t.Fatalf("Observe = exists %v, err %v: want the CRD-not-found answer for a deleting definition", obs.ResourceExists, err)
		}
	})
	t.Run("control: the same chart without it reaches CRD-not-found", func(t *testing.T) {
		clean := strings.Replace(contradictingSchema, `"required":["authentication"],`, "", 1)
		cd, obs, err := observeChart(t, clean)
		got := cd.GetCondition(rtv1.TypeReady)
		if err != nil || obs.ResourceExists || !strings.Contains(got.Message, "does not exists yet") {
			t.Fatalf("Observe = exists %v, err %v, Ready %s/%s (%q): the control did not reach the CRD-not-found return, so the case above proves nothing",
				obs.ResourceExists, err, got.Status, got.Reason, got.Message)
		}
		if !cdreason.Creating.Is(got) {
			t.Errorf("Ready = %s/%s, want False/Creating: Create follows in the same pass", got.Status, got.Reason)
		}
	})
}

// #566 (Diego, 2026-11-17: refuse explicitly): a NEW definition whose chart has no root
// values.schema.json reports Ready=False/ValuesSchemaMissing naming the chart, with a ReadyAssessed
// error and no status. It used to reach Create and read Ready=False/Creating for ever. A definition
// being deleted is not stopped (teardown must not depend on the chart). Control: the same chart WITH
// a schema reaches the CRD-not-found return.
func TestObserveReportsAMissingValuesSchema(t *testing.T) {
	t.Run("no root schema: Ready=False/ValuesSchemaMissing", func(t *testing.T) {
		cd, _, err := observeChart(t, noRootSchema)
		got := cd.GetCondition(rtv1.TypeReady)
		if !cdreason.ValuesSchemaMissing.Is(got) {
			t.Fatalf("Ready = %s/%s (%q), want False/ValuesSchemaMissing (Observe err: %v)", got.Status, got.Reason, got.Message, err)
		}
		if !strings.Contains(got.Message, "contradicting-0.1.0.tgz") || !strings.Contains(got.Message, "no values.schema.json at its root") {
			t.Errorf("Ready message %q does not name the chart and the cause", got.Message)
		}
		if err == nil || !reconciler.IsReadyAssessed(err) {
			t.Errorf("Observe err = %v, want a ReadyAssessed error", err)
		}
		if cd.Status.Kind != "" || cd.Status.ApiVersion != "" {
			t.Errorf("status names %q %q: nothing was generated", cd.Status.ApiVersion, cd.Status.Kind)
		}
	})
	t.Run("a definition being deleted is not stopped", func(t *testing.T) {
		cd, _, err := observeChartDeleting(t, noRootSchema, true)
		if got := cd.GetCondition(rtv1.TypeReady); cdreason.ValuesSchemaMissing.Is(got) {
			t.Fatalf("a deleting definition stopped on its chart's missing schema (err %v)", err)
		}
	})
	// Only a schema that is ABSENT is ValuesSchemaMissing. A root schema that exists but cannot be
	// read is some other failure, and keeps the unreadable-chart path rather than telling the chart's
	// owner to add a file that is already there.
	t.Run("a schema that exists but cannot be read is not reported missing", func(t *testing.T) {
		cd, _, err := observeChart(t, schemaIsADirectory)
		got := cd.GetCondition(rtv1.TypeReady)
		if cdreason.ValuesSchemaMissing.Is(got) {
			t.Fatalf("an unreadable schema read as missing: %q (err %v)", got.Message, err)
		}
		if !cdreason.Unavailable.Is(got) || !strings.Contains(got.Message, "getting spec schema") {
			t.Fatalf("control: Ready = %s/%s %q, want the unreadable-chart Unavailable from the schema read, so this case reached the branch", got.Status, got.Reason, got.Message)
		}
	})
	t.Run("control: the same chart with a schema reaches CRD-not-found", func(t *testing.T) {
		cd, obs, err := observeChart(t, `{"type":"object","properties":{"x":{"type":"string"}}}`)
		got := cd.GetCondition(rtv1.TypeReady)
		if err != nil || obs.ResourceExists || !strings.Contains(got.Message, "does not exists yet") {
			t.Fatalf("control did not reach CRD-not-found: exists %v, err %v, Ready %s/%s %q", obs.ResourceExists, err, got.Status, got.Reason, got.Message)
		}
	})
}

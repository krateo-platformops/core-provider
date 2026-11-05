package resources

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/krateo-platformops/chart-inspector/internal/handlers"
	helmconfig "github.com/krateo-platformops/plumbing/helm"
	helmv3 "github.com/krateo-platformops/plumbing/helm/v3"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/rest"
)

// #325 THROUGH THE REAL CHAIN. These tests use plumbing's real helm client, not a fake that
// returns a hand-built error: a fake that wraps helmconfig.ErrValuesSchema would be this test
// reconstructing the very input the classification depends on, and would stay green if plumbing
// ever stopped returning the sentinel from Install (a wrapper with %s instead of %w, a retry layer
// that re-wraps). Here the chart is served over HTTP as a .tgz, plumbing fetches and loads it, and
// its own pre-install validation produces the error.
//
// NO CLUSTER IS CONTACTED. The API server is https://127.0.0.1:1 — a closed port. A chart that
// fails schema validation never reaches it (plumbing validates before the install resolves
// anything). A chart that PASSES validation does reach it and fails with connection refused, and
// that is the 500 control: a genuine infrastructure failure on the very same path.

// chartFile is one file inside a chart archive, path relative to the archive root.
type chartFile struct{ path, body string }

func chartArchive(t *testing.T, files ...chartFile) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{Name: f.path, Mode: 0o644, Size: int64(len(f.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(f.body)); err != nil {
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

const configMapTemplate = `apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .Release.Name }}-cm
data:
  k: v
`

func chartYAML(name string) string {
	return "apiVersion: v2\nname: " + name + "\nversion: 0.1.0\n"
}

// serveChart serves archive at <srv>/chart.tgz, or 404 when archive is nil.
func serveChart(t *testing.T, archive []byte) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if archive == nil || r.URL.Path != "/chart.tgz" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(archive)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/chart.tgz"
}

// realChainFixture is the handler with plumbing's REAL helm client, a CompositionDefinition
// pointing at chartURL, and a composition whose spec (the chart's values) is spec.
func realChainFixture(t *testing.T, chartURL string, spec map[string]any) http.Handler {
	t.Helper()
	// Keep helm away from the developer's kubeconfig and any real cluster: settings.Namespace()
	// reads KUBECONFIG, and the memory driver keeps release storage in-process.
	t.Setenv("KUBECONFIG", t.TempDir()+"/absent")
	t.Setenv("HELM_DRIVER", "memory")
	t.Setenv("HELM_CACHE_HOME", t.TempDir())
	t.Setenv("HELM_CONFIG_HOME", t.TempDir())
	t.Setenv("HELM_DATA_HOME", t.TempDir())

	restCfg := &rest.Config{Host: "https://127.0.0.1:1"}
	hc, err := helmv3.NewClient(restCfg)
	if err != nil {
		t.Fatalf("building plumbing's helm client: %v", err)
	}

	cd := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "core.krateo.io/v1alpha1", "kind": "CompositionDefinition",
		"metadata": map[string]any{"name": "fireworksapp", "namespace": "krateo-system"},
		"spec": map[string]any{"chart": map[string]any{
			"url": chartURL, "version": "0.1.0",
		}},
	}}
	comp := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "composition.krateo.io/v1-0-0", "kind": "Firework",
		"metadata": map[string]any{"name": "fw", "namespace": "krateo-system"},
		"spec":     spec,
	}}

	scheme := runtime.NewScheme()
	gvrToKind := map[schema.GroupVersionResource]string{
		{Group: "core.krateo.io", Version: "v1alpha1", Resource: "compositiondefinitions"}: "CompositionDefinitionList",
		{Group: "composition.krateo.io", Version: "v1-0-0", Resource: "fireworks"}:         "FireworksList",
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, gvrToKind, cd, comp)

	return GetResources(handlers.HandlerOptions{
		Log:             slog.Default(),
		DynamicClient:   dyn,
		KrateoNamespace: "krateo-system",
		Plurarizer:      stubPluralizer{},
		RestConfig:      restCfg,
		HelmClient:      hc,
	})
}

type statusBody struct {
	Code    int    `json:"code"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

func decodeStatus(t *testing.T, rec *httptest.ResponseRecorder) statusBody {
	t.Helper()
	var b statusBody
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatalf("response body is not a JSON Status: %v (%s)", err, rec.Body.String())
	}
	return b
}

// Charts. Each is the smallest chart that puts one input state in front of the classification.

const strictSchema = `{"type":"object","additionalProperties":false,"properties":{"replicas":{"type":"integer"}}}`

// subchartStrict: the case measured in the field. The PARENT has no schema; its dependency forbids
// additional properties and so rejects the `global` helm injects into every dependency's values.
func subchartStrict(t *testing.T) []byte {
	return chartArchive(t,
		chartFile{"parent/Chart.yaml", chartYAML("parent")},
		chartFile{"parent/values.yaml", "sub:\n  replicas: 1\n"},
		chartFile{"parent/templates/cm.yaml", configMapTemplate},
		chartFile{"parent/charts/sub/Chart.yaml", chartYAML("sub")},
		chartFile{"parent/charts/sub/values.yaml", "replicas: 1\n"},
		chartFile{"parent/charts/sub/values.schema.json", strictSchema},
		chartFile{"parent/charts/sub/templates/cm.yaml", configMapTemplate},
	)
}

// topLevel: a single chart with the given schema.
func topLevel(t *testing.T, schemaJSON string) []byte {
	return chartArchive(t,
		chartFile{"top/Chart.yaml", chartYAML("top")},
		chartFile{"top/values.yaml", "replicas: 1\n"},
		chartFile{"top/values.schema.json", schemaJSON},
		chartFile{"top/templates/cm.yaml", configMapTemplate},
	)
}

// The strict schema with `global` declared: what krateoGlobalNote tells the author to write. It is
// also the CONTROL chart: schema-valid, so the install proceeds past validation to the cluster.
const strictSchemaDeclaringGlobal = `{"type":"object","additionalProperties":false,"properties":{` +
	`"replicas":{"type":"integer"},"global":{"type":"object"}}}`

// helm's own words for the rejection. Asserted to prove the body carries HELM's explanation, not
// only a sentence this service or plumbing wrote around it.
const helmRejectsGlobal = "additional properties 'global' not allowed"

func require400WithHelmsMessage(t *testing.T, rec *httptest.ResponseRecorder, helmSays string) statusBody {
	t.Helper()
	b := decodeStatus(t, rec)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. A chart-authoring error answered %d is a claim about "+
			"chart-inspector, and sends the author to its logs instead of their chart (#325). body: %s",
			rec.Code, rec.Code, rec.Body.String())
	}
	// The literal, copied rather than referencing the const: it is a contract with the cdc
	// (internal/chartinspector there pins the same string), and a test that read the const would
	// pass a rename that breaks the other side.
	if b.Code != http.StatusBadRequest || b.Reason != "ValuesSchemaViolation" {
		t.Errorf("body code/reason = %d/%q, want 400/ValuesSchemaViolation. Without the reason a "+
			"caller can tell this 400 from the parameter 400s only by ruling them out", b.Code, b.Reason)
	}
	if !strings.Contains(b.Message, helmSays) {
		t.Errorf("the message does not carry helm's explanation %q; the status is right and the one "+
			"sentence that resolves it is missing. message: %s", helmSays, b.Message)
	}
	return b
}

// #325, the reported case. Status code first: it is what a caller branches on.
func TestHandler_SubchartValuesSchemaViolationIs400WithHelmsMessage(t *testing.T) {
	rec := doRequest(t, realChainFixture(t, serveChart(t, subchartStrict(t)), map[string]any{}))
	b := require400WithHelmsMessage(t, rec, helmRejectsGlobal)

	// The `global` case is named, and the advice is to remove the prohibition rather than to add
	// a property (#325 item 2). This text is plumbing's; asserted so a regression there is seen
	// here, where the author reads it.
	for _, want := range []string{`subchart "sub"`, "remove `additionalProperties: false`"} {
		if !strings.Contains(b.Message, want) {
			t.Errorf("message does not contain %q: %s", want, b.Message)
		}
	}
}

// A TOP-LEVEL strict schema is rejected by Krateo's own injection, not by anything in the spec.
//
// The composition's spec here has no `global` key (the premise, asserted below), and the 400 must
// not leave the author with plumbing's "drop `global` from the values" as the last word: there is
// nothing in their values to drop.
func TestHandler_TopLevelSchemaRejectingKrateosGlobalIs400AndSaysKrateoAddedIt(t *testing.T) {
	spec := map[string]any{"replicas": int64(2)}
	if _, ok := spec["global"]; ok {
		t.Fatal("premise broken: the spec must NOT supply `global`, or this does not test the injected key")
	}

	rec := doRequest(t, realChainFixture(t, serveChart(t, topLevel(t, strictSchema)), spec))
	b := require400WithHelmsMessage(t, rec, helmRejectsGlobal)

	if !strings.Contains(b.Message, krateoGlobalNote) {
		t.Errorf("the 400 does not say Krateo injects `global`, so the author is left with advice to "+
			"drop a key their spec does not contain. message: %s", b.Message)
	}
	// The note FOLLOWS the error and does not replace it.
	if !strings.HasPrefix(b.Message, helmconfig.ErrValuesSchema.Error()) {
		t.Errorf("message does not start with the classification's own sentence: %s", b.Message)
	}
}

// A schema violation that has nothing to do with `global`: still the author's, still 400.
func TestHandler_ValueTypeSchemaViolationIs400(t *testing.T) {
	spec := map[string]any{"replicas": "two"}
	rec := doRequest(t, realChainFixture(t, serveChart(t, topLevel(t, strictSchemaDeclaringGlobal)), spec))
	require400WithHelmsMessage(t, rec, "/replicas")
}

// ORDER. A schema failure whose text ALSO satisfies the REST-mapping detector, which falls back to
// substring matching, must still be 400. The property name is the caller's, and helm quotes it.
// Without this the two arms could be swapped and nothing would notice, because in every other test
// only one of them can fire.
func TestHandler_SchemaViolationWhoseTextLooksLikeAnUnservedKindIs400Not422(t *testing.T) {
	const crdPhrase = "ensure CRDs are installed first"
	spec := map[string]any{"replicas": int64(1), crdPhrase: true}
	rec := doRequest(t, realChainFixture(t, serveChart(t, topLevel(t, strictSchemaDeclaringGlobal)), spec))

	// Calibration: the input really does carry the detector's phrase, or this asserts nothing.
	if !strings.Contains(rec.Body.String(), crdPhrase) {
		t.Fatalf("VACUOUS: helm's message does not quote the property, so the REST-mapping detector "+
			"cannot fire and the order is untested. body: %s", rec.Body.String())
	}
	require400WithHelmsMessage(t, rec, "additional properties '"+crdPhrase+"' not allowed")
}

// CONTROL. The same real chain, with a chart that SATISFIES its schema: validation passes, the
// install proceeds, and the API server is unreachable. That is a genuine infrastructure failure and
// must stay 500. Without this, a handler answering 400 for every install failure passes every test
// above.
func TestHandler_SchemaSatisfiedButClusterUnreachableStillAnswers500(t *testing.T) {
	spec := map[string]any{"replicas": int64(2)}
	rec := doRequest(t, realChainFixture(t, serveChart(t, topLevel(t, strictSchemaDeclaringGlobal)), spec))
	b := decodeStatus(t, rec)

	// Vacuity guard: the response came from the INSTALL, past validation. This is also the
	// measurement that krateoGlobalNote's advice (declare `global` under `properties`) works.
	if !strings.Contains(b.Message, "install failed") || strings.Contains(b.Message, helmconfig.ErrValuesSchema.Error()) {
		t.Fatalf("VACUOUS: the response did not come from an install that passed schema validation. "+
			"status=%d message=%s", rec.Code, b.Message)
	}
	if rec.Code != http.StatusInternalServerError || b.Reason != "InternalError" {
		t.Fatalf("status/reason = %d/%q, want 500/InternalError for an unreachable API server. A real "+
			"chart-inspector fault reported as the caller's is the inverse of #325. message: %s",
			rec.Code, b.Reason, b.Message)
	}
}

// CONTROL. The chart cannot be fetched at all: its server is down. Not the author's chart, 500.
func TestHandler_ChartServerUnreachableStillAnswers500(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL + "/chart.tgz"
	srv.Close()

	rec := doRequest(t, realChainFixture(t, url, map[string]any{}))
	b := decodeStatus(t, rec)
	if !strings.Contains(b.Message, "failed to load chart") {
		t.Fatalf("VACUOUS: the response did not come from fetching the chart. status=%d message=%s",
			rec.Code, b.Message)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 for an unreachable chart server. message: %s", rec.Code, b.Message)
	}
}

// THE DECISION, over inputs the real chain does not produce on demand: wrapping depth, errors.Join,
// and an error that merely QUOTES the sentinel's text without wrapping it.
func TestInstallFailureStatus(t *testing.T) {
	sentinel := helmconfig.ErrValuesSchema
	unserved := fmt.Errorf("install failed: %w", &meta.NoKindMatchError{
		GroupKind:        schema.GroupKind{Group: "kagent.dev", Kind: "RemoteMCPServer"},
		SearchedVersions: []string{"v1alpha2"},
	})
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"the sentinel itself", sentinel, http.StatusBadRequest},
		{"wrapped once, as plumbing returns it", fmt.Errorf("%w: chart \"c\": - at '/replicas': got string", sentinel), http.StatusBadRequest},
		{"wrapped twice", fmt.Errorf("outer: %w", fmt.Errorf("%w: detail", sentinel)), http.StatusBadRequest},
		{"wrapped with the Krateo note", withKrateoGlobalNote(fmt.Errorf("%w: detail", sentinel)), http.StatusBadRequest},
		{"joined with an unrelated error", errors.Join(errors.New("connection refused"), fmt.Errorf("%w: detail", sentinel)), http.StatusBadRequest},
		{"sentinel AND detector text", fmt.Errorf("%w: additional properties 'no matches for kind' not allowed", sentinel), http.StatusBadRequest},

		// CONTROLS.
		{"the sentinel's TEXT without the sentinel", errors.New(sentinel.Error() + ": detail"), http.StatusInternalServerError},
		{"unserved kind", unserved, http.StatusUnprocessableEntity},
		{"connection refused", errors.New("dialing the registry: connection refused"), http.StatusInternalServerError},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := installFailureStatus(c.err); got != c.want {
				t.Errorf("installFailureStatus(%q) = %d, want %d", c.err, got, c.want)
			}
		})
	}
}

// CONTROL for the reason: a 400 that is NOT a values-schema violation must not carry it. Without
// this, the reason assertion above passes against a handler that stamps every 400.
func TestHandler_ParameterBadRequestDoesNotClaimASchemaViolation(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/resources?"+
		"compositionName=fw&compositionNamespace=krateo-system"+
		"&compositionDefinitionName=fireworksapp&compositionDefinitionNamespace=krateo-system"+
		"&compositionVersion=v1-0-0&compositionResource=fireworks", nil) // no releaseName
	rec := httptest.NewRecorder()
	wiringFixture(t, unservedKindHelmClient{}).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("setup: status = %d, want the releaseName 400. body=%s", rec.Code, rec.Body.String())
	}
	if b := decodeStatus(t, rec); b.Reason == "ValuesSchemaViolation" {
		t.Errorf("a missing-parameter 400 carries reason ValuesSchemaViolation; the cdc would tell the " +
			"chart author their values are wrong when the cdc sent an incomplete request")
	}
}

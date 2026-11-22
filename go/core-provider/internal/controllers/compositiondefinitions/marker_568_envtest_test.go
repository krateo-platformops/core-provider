//go:build envtest

package compositiondefinitions

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/cdreason"
	"github.com/krateo-platformops/core-provider/internal/controllers/compositiondefinitions/helpers/status"
	"github.com/krateo-platformops/core-provider/internal/tools/chart"
	"github.com/krateo-platformops/core-provider/internal/tools/chart/chartfs"
	crdutils "github.com/krateo-platformops/core-provider/internal/tools/crd/generation"
	"github.com/krateo-platformops/core-provider/internal/tools/deploy"
	"github.com/krateo-platformops/plumbing/valuesschema"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// markerChartTGZ is a chart "widget" 0.1.0 with a small values.yaml and, unless schema is
// noRootSchema, that values.schema.json.
func markerChartTGZ(t *testing.T, schema string) []byte {
	t.Helper()
	files := map[string]string{
		"widget/Chart.yaml":        "apiVersion: v2\nname: widget\nversion: 0.1.0\n",
		"widget/values.yaml":       "replicas: 2\nimage:\n  repository: nginx\n  tag: \"1.27\"\nlabels: {}\n",
		"widget/templates/cm.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: {{ .Release.Name }}\n",
	}
	if schema != noRootSchema {
		files["widget/values.schema.json"] = schema
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
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

const markerAuthoredSchema = `{
  "$schema": "https://json-schema.org/draft-07/schema#",
  "type": "object",
  "properties": {
    "replicas": {"type": "integer", "default": 2},
    "image": {"type": "object", "properties": {"repository": {"type": "string", "default": "nginx"}, "tag": {"type": "string", "default": "1.27"}}},
    "labels": {"type": "object", "additionalProperties": true}
  }
}`

// The same authored schema, with the inferred-schema marker forged at its root.
const markerForgedSchema = `{
  "$schema": "https://json-schema.org/draft-07/schema#",
  "x-krateo-values-schema": {"source": "inferred", "rules": "1"},
  "type": "object",
  "properties": {
    "replicas": {"type": "integer", "default": 2},
    "image": {"type": "object", "properties": {"repository": {"type": "string", "default": "nginx"}, "tag": {"type": "string", "default": "1.27"}}},
    "labels": {"type": "object", "additionalProperties": true}
  }
}`

// #568 child 7 on a real apiserver (Diego, 2026-11-22: core-provider owns the marker). The portal keys
// its "inferred" banner on the root keyword x-krateo-values-schema in the definition's JSON-schema
// ConfigMap, so that ConfigMap must carry it IF AND ONLY IF ValuesSchemaAuthored is False.
//
// Each case reads the chart through the one seam (chart.SpecSchema, as Observe/Create/Update do),
// generates the CRD and applies it (Create's calls), computes the schema conditions with the function
// Observe writes them with (schemaSourceConditions, compositiondefinitions.go Observe), writes them to
// the definition's status on the apiserver and reads them back, and writes the JSON-schema ConfigMap
// through the renderer every writer shares (deploy.ApplyDefinitionSchemaConfigMap ->
// renderJSONSchemaConfigMap), then GETs it by the portal's name derivation.
//
// With KRATEO_MARKER_SAMPLES_DIR set, each case writes its status.conditions and the ConfigMap's
// values.schema.json there, for frontend's banner work.
func TestEnvtest_568_MarkerPresentIffInferred(t *testing.T) {
	for _, tc := range []struct {
		name         string
		schema       string
		infer        bool
		wantMarker   bool
		wantAuthored cdreason.Reason
	}{
		{"inferred", noRootSchema, true, true, cdreason.ValuesSchemaInferred},
		{"authored", markerAuthoredSchema, false, false, cdreason.ValuesSchemaAuthored},
		{"authored-forged-marker", markerForgedSchema, false, false, cdreason.ValuesSchemaAuthored},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			cl, dyn := startAddEnv(t)
			const ns = "team-568"
			if err := cl.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
				t.Fatal(err)
			}
			pkg, err := chartfs.FromReader(bytes.NewReader(markerChartTGZ(t, tc.schema)), "test://widget-0.1.0.tgz")
			if err != nil {
				t.Fatal(err)
			}
			spec, err := chart.SpecSchema(pkg.FS(), pkg.RootDir(), tc.infer)
			if err != nil {
				t.Fatalf("SpecSchema: %v", err)
			}
			gvk, err := chart.ChartGroupVersionKind(pkg.FS(), pkg.RootDir())
			if err != nil {
				t.Fatal(err)
			}
			gen, err := crdutils.GenerateCRDWithWarnings(spec.Bytes, gvk)
			if err != nil {
				t.Fatalf("GenerateCRDWithWarnings: %v", err)
			}

			cr := &compositiondefinitionsv1alpha1.CompositionDefinition{ObjectMeta: metav1.ObjectMeta{Name: "widget", Namespace: ns}}
			if tc.infer {
				cr.SetAnnotations(map[string]string{chart.ValuesSchemaAnnotation: chart.ValuesSchemaInfer})
			}
			cr.Spec.Chart = &compositiondefinitionsv1alpha1.ChartInfo{Url: "test://widget-0.1.0.tgz", Version: "0.1.0"}
			if err := cl.Create(ctx, cr); err != nil {
				t.Fatal(err)
			}
			gvr, err := newExternal(cl, dyn).applyCompositionCRD(ctx, cr, gvk, gen.CRD.DeepCopy())
			if err != nil {
				t.Fatalf("applyCompositionCRD: %v", err)
			}
			if err := status.RefreshCompositionDefinitionStatus(cr, gen.CRD, gvr, gvk, "test://widget-0.1.0.tgz"); err != nil {
				t.Fatal(err)
			}
			cr.SetConditions(schemaSourceConditions(spec, gen.Warnings)...)
			if err := cl.Status().Update(ctx, cr); err != nil {
				t.Fatalf("writing the definition's status: %v", err)
			}
			stored := &compositiondefinitionsv1alpha1.CompositionDefinition{}
			if err := cl.Get(ctx, types.NamespacedName{Namespace: ns, Name: "widget"}, stored); err != nil {
				t.Fatal(err)
			}
			authored := stored.GetCondition(cdreason.TypeValuesSchemaAuthored)
			if !tc.wantAuthored.Is(authored) {
				t.Fatalf("ValuesSchemaAuthored = %s/%s (%q), want %s", authored.Status, authored.Reason, authored.Message, tc.name)
			}

			root := t.TempDir()
			if _, err := renderChartAssets(root, "krateo-system", "ghcr.io/krateo-platformops/composition-dynamic-controller:568-test"); err != nil {
				t.Fatal(err)
			}
			tmpl := filepath.Join(root, "json-schema-configmap", filepath.Base(JSONSchemaTemplateConfigmapPath))
			if err := deploy.ApplyDefinitionSchemaConfigMap(ctx, dyn, gvr, ns, tmpl, spec.Bytes); err != nil {
				t.Fatalf("ApplyDefinitionSchemaConfigMap: %v", err)
			}
			// The portal's derivation, from status alone.
			seg := stored.Status.ApiVersion[strings.LastIndex(stored.Status.ApiVersion, "/")+1:]
			cmNN := types.NamespacedName{Namespace: ns, Name: stored.Status.Resource + "-" + seg + "-jsonschema-configmap"}
			var cm corev1.ConfigMap
			if err := cl.Get(ctx, cmNN, &cm); err != nil {
				t.Fatalf("portal GET %s: %v", cmNN, err)
			}
			published, ok := cm.Data["values.schema.json"]
			if !ok {
				t.Fatalf("%s has no values.schema.json key: %v", cmNN, cm.Data)
			}
			var doc map[string]json.RawMessage
			if err := json.Unmarshal([]byte(published), &doc); err != nil {
				t.Fatalf("published schema is not a JSON object: %v", err)
			}
			raw, hasMarker := doc[valuesschema.MarkerKey]
			if hasMarker != tc.wantMarker {
				t.Fatalf("published schema carries the root marker = %v, want %v (ValuesSchemaAuthored %s/%s):\n%s", hasMarker, tc.wantMarker, authored.Status, authored.Reason, published)
			}
			if hasMarker {
				var m struct{ Source, Rules string }
				if err := json.Unmarshal(raw, &m); err != nil || m.Source != "inferred" || m.Rules != valuesschema.RulesVersion {
					t.Fatalf("marker = %s (%v), want {source: inferred, rules: %s}", raw, err, valuesschema.RulesVersion)
				}
			}
			// The marker must not be a CRD property either: the CRD's spec has no field of that name.
			if _, ok := gen.CRD.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties[valuesschema.MarkerKey]; ok {
				t.Fatalf("the CRD's spec schema has a %s property", valuesschema.MarkerKey)
			}

			if dir := os.Getenv("KRATEO_MARKER_SAMPLES_DIR"); dir != "" {
				conds, err := json.MarshalIndent(stored.Status.Conditions, "", "  ")
				if err != nil {
					t.Fatal(err)
				}
				for file, body := range map[string][]byte{
					tc.name + ".cd-status-conditions.json":        conds,
					tc.name + ".jsonschema-configmap.values.json": []byte(published),
					tc.name + ".jsonschema-configmap.name.txt":    []byte(cmNN.String() + "\n"),
				} {
					if err := os.WriteFile(filepath.Join(dir, file), body, 0o644); err != nil {
						t.Fatal(err)
					}
				}
			}
		})
	}
}

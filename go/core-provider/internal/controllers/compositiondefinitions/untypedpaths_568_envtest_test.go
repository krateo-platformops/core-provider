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
	"sort"
	"strings"
	"testing"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/controllers/compositiondefinitions/helpers/status"
	"github.com/krateo-platformops/core-provider/internal/tools/chart"
	"github.com/krateo-platformops/core-provider/internal/tools/chart/chartfs"
	crdutils "github.com/krateo-platformops/core-provider/internal/tools/crd/generation"
	"github.com/krateo-platformops/core-provider/internal/tools/deploy"
	"github.com/krateo-platformops/plumbing/valuesschema"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// untypedPathsValues is the inferred chart's values.yaml for the untyped-paths cross-check. Each line
// says what rules v1 (plumbing valuesschema infer.go) does with it, read from the code, not assumed:
//
//	replicas: 2            int leaf: int-or-string, typed                        NEGATIVE
//	labels: {}             empty map: {type: object}, open                        NEGATIVE
//	nothing: null          null leaf: untyped ("null")                            A: nothing
//	ratio: 0.5             float leaf: untyped with a default ("float")           A: ratio
//	mixed: [1, "a"]        int and string are ONE class in itemsNode, so its items
//	                       are int-or-string, NOT untyped (the list asked for as a
//	                       "mixed list" is not one under rules v1)                NEGATIVE
//	mixedKinds: [1, true]  a genuinely mixed list: items untyped ("mixed list")   A: mixedKinds[]
//	lists: [[1], [2]]      list of lists: items untyped ("list of lists")         A: lists[]
//	nested.deep: null      a nested null, for the dotted path                     A: nested.deep
//	empty: []              empty list: itemsNode returns nil items, so crdgen gives items
//	                       anyValue(); since plumbing v1.105.0 (#226) the report records it
//	                       ("empty list")                                         A: empty[]
//
// A list's untyped finding is reported at its ITEMS path ("x[]", infer.go itemsNode is called with
// path+"[]"), while B reports a matching items node under the array's path ("x").
const untypedPathsValues = `replicas: 2
labels: {}
nothing: null
ratio: 0.5
mixed: [1, "a"]
mixedKinds: [1, true]
lists: [[1], [2]]
nested:
  deep: null
empty: []
`

func valuesChartTGZ(t *testing.T, name, values string) []byte {
	t.Helper()
	files := map[string]string{
		name + "/Chart.yaml":        "apiVersion: v2\nname: " + name + "\nversion: 0.1.0\n",
		name + "/values.yaml":       values,
		name + "/templates/cm.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: {{ .Release.Name }}\n",
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for n, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(body))}); err != nil {
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

// crdUntypedPaths walks a CRD spec schema and returns the dotted paths of every node that declares no
// type, carries x-kubernetes-preserve-unknown-fields: true, and is not x-kubernetes-int-or-string: an
// "accepts any value" node. A matching array ITEMS node is reported under its array's path (x, not
// x[]). A map's value schema (additionalProperties) is walked under path.* (rules v1 emits none).
func crdUntypedPaths(s *apiextensionsv1.JSONSchemaProps) []string {
	var out []string
	var walk func(p *apiextensionsv1.JSONSchemaProps, path string)
	matches := func(p *apiextensionsv1.JSONSchemaProps) bool {
		return p.Type == "" && p.XPreserveUnknownFields != nil && *p.XPreserveUnknownFields && !p.XIntOrString
	}
	walk = func(p *apiextensionsv1.JSONSchemaProps, path string) {
		if p == nil {
			return
		}
		if path != "" && matches(p) {
			out = append(out, path)
		}
		for k := range p.Properties {
			v := p.Properties[k]
			child := k
			if path != "" {
				child = path + "." + k
			}
			walk(&v, child)
		}
		if p.Items != nil && p.Items.Schema != nil {
			it := p.Items.Schema
			if matches(it) {
				out = append(out, path) // reported under the array's path
			}
			// Recurse below the items node with the array's path, so nested findings read x.field.
			for k := range it.Properties {
				v := it.Properties[k]
				walk(&v, path+"."+k)
			}
			if it.Items != nil {
				walk(&apiextensionsv1.JSONSchemaProps{Type: "array", Items: it.Items}, path)
			}
		}
		if p.AdditionalProperties != nil && p.AdditionalProperties.Schema != nil {
			walk(p.AdditionalProperties.Schema, path+".*")
		}
	}
	walk(s, "")
	sort.Strings(out)
	return out
}

// #568, for frontend's banner/form work: the inference report's untyped paths (A) against the stored
// CRD's "accepts any value" nodes (B). B is read from the CRD as the APISERVER stores it.
//
// The contract (decided 2026-11-22 from this test's measurement): frontend names the field the USER
// sees, and the report stays verbatim under Details. A names a list's untyped ITEMS (x[]); a form
// shows the list (x). Mapped that way, A and B must be EQUAL, strict in both directions.
//
// Until plumbing v1.105.0 an empty list was the one accepted difference (valuesschema recorded no
// finding for it, though crdgen gives its items "any value"). plumbing#226 reports it (empty[]), so
// the exemption is gone and any difference fails with both sides printed.
func TestEnvtest_568_UntypedPathsReportMatchesTheCRD(t *testing.T) {
	ctx := context.Background()
	cl, dyn := startAddEnv(t)

	pkg, err := chartfs.FromReader(bytes.NewReader(valuesChartTGZ(t, "anyvalue", untypedPathsValues)), "test://anyvalue-0.1.0.tgz")
	if err != nil {
		t.Fatal(err)
	}
	spec, err := chart.SpecSchema(pkg.FS(), pkg.RootDir(), true)
	if err != nil {
		t.Fatalf("SpecSchema: %v", err)
	}
	if spec.Source != chart.SchemaInferred || spec.Report == nil {
		t.Fatalf("Source = %q, report %v: want an inferred schema with its report", spec.Source, spec.Report)
	}
	var a []string
	for _, f := range spec.Report.Findings {
		if f.Kind == valuesschema.KindUntyped {
			a = append(a, f.Path)
		}
	}
	sort.Strings(a)

	gvk, err := chart.ChartGroupVersionKind(pkg.FS(), pkg.RootDir())
	if err != nil {
		t.Fatal(err)
	}
	gen, err := crdutils.GenerateCRDWithWarnings(spec.Bytes, gvk)
	if err != nil {
		t.Fatalf("GenerateCRDWithWarnings: %v", err)
	}
	cr := &compositiondefinitionsv1alpha1.CompositionDefinition{ObjectMeta: metav1.ObjectMeta{Name: "anyvalue", Namespace: "team-568"}}
	cr.Spec.Chart = &compositiondefinitionsv1alpha1.ChartInfo{Url: "test://anyvalue-0.1.0.tgz", Version: "0.1.0"}
	gvr, err := newExternal(cl, dyn).applyCompositionCRD(ctx, cr, gvk, gen.CRD.DeepCopy())
	if err != nil {
		t.Fatalf("applyCompositionCRD: %v", err)
	}
	stored := &apiextensionsv1.CustomResourceDefinition{}
	if err := cl.Get(ctx, types.NamespacedName{Name: gen.CRD.Name}, stored); err != nil {
		t.Fatal(err)
	}
	var specSchema *apiextensionsv1.JSONSchemaProps
	for i := range stored.Spec.Versions {
		if stored.Spec.Versions[i].Name == gvk.Version {
			s := stored.Spec.Versions[i].Schema.OpenAPIV3Schema.Properties["spec"]
			specSchema = &s
		}
	}
	if specSchema == nil {
		t.Fatalf("the stored CRD serves no version %s", gvk.Version)
	}
	b := crdUntypedPaths(specSchema)

	// CONTROLS: the negatives are present in the CRD and typed, so B not naming them is a verdict,
	// not a walk that never reached them.
	for _, neg := range []string{"replicas", "labels", "mixed"} {
		if _, ok := specSchema.Properties[neg]; !ok {
			t.Fatalf("control: the CRD's spec has no %q property, so the walk cannot be judged on it", neg)
		}
	}
	if len(b) == 0 {
		t.Fatal("B is empty: the walk found no accepts-any-value node at all, which the null leaf alone should produce")
	}

	t.Logf("A (inference report, KindUntyped): %v", a)
	t.Logf("B (stored CRD, no type + preserve-unknown + not int-or-string): %v", b)

	// THE PORTAL'S SHAPE, for the record (no assertion): frontend's form draws the JSON-schema
	// ConfigMap's values schema when present and the CRD only as a fallback. Written through the
	// renderer every writer shares (deploy.ApplyDefinitionSchemaConfigMap -> renderJSONSchemaConfigMap)
	// and fetched by the portal's name derivation from status.
	if err := status.RefreshCompositionDefinitionStatus(cr, gen.CRD, gvr, gvk, "test://anyvalue-0.1.0.tgz"); err != nil {
		t.Fatal(err)
	}
	if err := cl.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: cr.Namespace}}); err != nil {
		t.Fatal(err)
	}
	assets := t.TempDir()
	if _, err := renderChartAssets(assets, "krateo-system", "ghcr.io/krateo-platformops/composition-dynamic-controller:568-test"); err != nil {
		t.Fatal(err)
	}
	tmpl := filepath.Join(assets, "json-schema-configmap", filepath.Base(JSONSchemaTemplateConfigmapPath))
	if err := deploy.ApplyDefinitionSchemaConfigMap(ctx, dyn, gvr, cr.Namespace, tmpl, spec.Bytes); err != nil {
		t.Fatalf("ApplyDefinitionSchemaConfigMap: %v", err)
	}
	seg := cr.Status.ApiVersion[strings.LastIndex(cr.Status.ApiVersion, "/")+1:]
	cmNN := types.NamespacedName{Namespace: cr.Namespace, Name: cr.Status.Resource + "-" + seg + "-jsonschema-configmap"}
	var cm corev1.ConfigMap
	if err := cl.Get(ctx, cmNN, &cm); err != nil {
		t.Fatalf("portal GET %s: %v", cmNN, err)
	}
	cmSchema, ok := cm.Data["values.schema.json"]
	if !ok {
		t.Fatalf("%s has no values.schema.json key", cmNN)
	}
	var cmDoc map[string]any
	if err := json.Unmarshal([]byte(cmSchema), &cmDoc); err != nil {
		t.Fatalf("the ConfigMap's values schema is not a JSON object: %v", err)
	}
	cmNode := func(path ...string) string {
		var cur any = cmDoc
		for _, k := range path {
			m, ok := cur.(map[string]any)
			if !ok {
				return "<not reached: parent is not an object>"
			}
			if cur, ok = m[k]; !ok {
				return "<absent>"
			}
		}
		out, _ := json.Marshal(cur)
		return string(out)
	}
	for _, leaf := range []struct {
		name string
		path []string
	}{
		{"nothing (null leaf)", []string{"properties", "nothing"}},
		{"ratio (0.5)", []string{"properties", "ratio"}},
		{"mixedKinds ([1, true])", []string{"properties", "mixedKinds"}},
		{"lists ([[1],[2]])", []string{"properties", "lists"}},
		{"nested.deep (nested null)", []string{"properties", "nested", "properties", "deep"}},
		{"empty ([])", []string{"properties", "empty"}},
		{"NEGATIVE replicas (2)", []string{"properties", "replicas"}},
		{"NEGATIVE labels ({})", []string{"properties", "labels"}},
		{"NEGATIVE mixed ([1, \"a\"])", []string{"properties", "mixed"}},
	} {
		t.Logf("CONFIGMAP %-28s %s", leaf.name, cmNode(leaf.path...))
	}

	if dir := os.Getenv("KRATEO_MARKER_SAMPLES_DIR"); dir != "" {
		sj, err := json.MarshalIndent(specSchema, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		lists, _ := json.MarshalIndent(map[string][]string{"A_reportUntyped": a, "B_crdAnyValue": b}, "", "  ")
		for file, body := range map[string][]byte{
			"inferred.crd-spec-schema.json":                 sj,
			"inferred.untyped-paths.json":                   lists,
			"inferred.untyped-values.yaml":                  []byte(untypedPathsValues),
			"inferred.untyped.configmap-values-schema.json": []byte(cmSchema),
		} {
			if err := os.WriteFile(filepath.Join(dir, file), body, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

	mappedA := make([]string, 0, len(a))
	for _, p := range a {
		mappedA = append(mappedA, strings.TrimSuffix(p, "[]")) // the list's items -> the list the user sees
	}
	sort.Strings(mappedA)
	onlyA, onlyB := diffSorted(mappedA, b)
	t.Logf("A mapped to the field the user sees: %v", mappedA)
	if len(onlyA) > 0 || len(onlyB) > 0 {
		t.Fatalf("the inference report and the CRD disagree:\n  only in A (report, mapped x[] -> x): %v\n  only in B (CRD): %v", onlyA, onlyB)
	}
	// CONTROL: the empty list is now reported by the report itself (plumbing#226), so equality is
	// not reached by both sides happening to omit it.
	if i := sort.SearchStrings(a, "empty[]"); i >= len(a) || a[i] != "empty[]" {
		t.Fatalf("control: the report does not name empty[] (plumbing v1.105.0 reports an empty list's items); A = %v", a)
	}
}

func diffSorted(a, b []string) (onlyA, onlyB []string) {
	in := func(s []string, x string) bool {
		i := sort.SearchStrings(s, x)
		return i < len(s) && s[i] == x
	}
	for _, x := range a {
		if !in(b, x) {
			onlyA = append(onlyA, x)
		}
	}
	for _, x := range b {
		if !in(a, x) {
			onlyB = append(onlyB, x)
		}
	}
	return onlyA, onlyB
}

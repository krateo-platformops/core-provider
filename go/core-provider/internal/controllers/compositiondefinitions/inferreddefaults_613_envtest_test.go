//go:build envtest

package compositiondefinitions

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/tools/chart"
	"github.com/krateo-platformops/core-provider/internal/tools/chart/chartfs"
	crdutils "github.com/krateo-platformops/core-provider/internal/tools/crd/generation"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
)

// inferredDefaultsValues: a decimal leaf, the same decimal nested in an optional parent, and an integer
// leaf as the control. Rules v1 infers a decimal leaf UNTYPED with a default ({default: 0.5}) and an
// integer leaf int-or-string with a default.
const inferredDefaultsValues = `ratio: 0.5
tuning:
  ratio: 0.5
count: 3
`

// #613, end to end through core-provider: an opted-in chart's decimal defaults reach the composition CRD
// AND are applied by the apiserver to a composition that omits them.
//
// Before plumbing v1.105.0 crdgen returned early for an untyped node (transpile.go return anyValue()
// before copyDefault), so `ratio` and `tuning.ratio` had no default in the CRD and an instance omitting
// them reached helm without them, while the integer `count` defaulted (the control, which must hold on
// both versions). On kp/main at plumbing v1.104.0 this test fails at the stored-CRD assertion.
func TestEnvtest_613_InferredDecimalDefaultsAreAppliedByTheApiserver(t *testing.T) {
	ctx := context.Background()
	cl, dyn := startAddEnv(t)

	pkg, err := chartfs.FromReader(bytes.NewReader(valuesChartTGZ(t, "decimals", inferredDefaultsValues)), "test://decimals-0.1.0.tgz")
	if err != nil {
		t.Fatal(err)
	}
	spec, err := chart.SpecSchema(pkg.FS(), pkg.RootDir(), true)
	if err != nil {
		t.Fatalf("SpecSchema: %v", err)
	}
	if spec.Source != chart.SchemaInferred {
		t.Fatalf("Source = %q, want inferred", spec.Source)
	}
	gvk, err := chart.ChartGroupVersionKind(pkg.FS(), pkg.RootDir())
	if err != nil {
		t.Fatal(err)
	}
	gen, err := crdutils.GenerateCRDWithWarnings(spec.Bytes, gvk)
	if err != nil {
		t.Fatalf("GenerateCRDWithWarnings: %v", err)
	}
	cr := &compositiondefinitionsv1alpha1.CompositionDefinition{ObjectMeta: metav1.ObjectMeta{Name: "decimals", Namespace: "team-613"}}
	cr.Spec.Chart = &compositiondefinitionsv1alpha1.ChartInfo{Url: "test://decimals-0.1.0.tgz", Version: "0.1.0"}
	gvr, err := newExternal(cl, dyn).applyCompositionCRD(ctx, cr, gvk, gen.CRD.DeepCopy())
	if err != nil {
		t.Fatalf("applyCompositionCRD: %v", err)
	}

	// 1. The CRD AS STORED by the apiserver carries the defaults.
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
	dflt := func(p apiextensionsv1.JSONSchemaProps) string {
		if p.Default == nil {
			return "<none>"
		}
		return string(p.Default.Raw)
	}
	ratio, tuning, count := specSchema.Properties["ratio"], specSchema.Properties["tuning"], specSchema.Properties["count"]
	tuningRatio := tuning.Properties["ratio"]
	t.Logf("STORED CRD defaults: ratio=%s tuning=%s tuning.ratio=%s count=%s", dflt(ratio), dflt(tuning), dflt(tuningRatio), dflt(count))
	if dflt(count) != "3" {
		t.Fatalf("control: count's default is %s, want 3 (an integer default survived before the fix too)", dflt(count))
	}
	if dflt(ratio) != "0.5" || dflt(tuningRatio) != "0.5" {
		t.Fatalf("decimal defaults missing from the stored CRD: ratio=%s tuning.ratio=%s, want 0.5 (core-provider#613)", dflt(ratio), dflt(tuningRatio))
	}
	if dflt(tuning) != "{}" {
		t.Fatalf("tuning's default is %s, want {} so its leaf default fires when the whole map is omitted", dflt(tuning))
	}

	// 2. The apiserver APPLIES them to a composition that omits ratio and tuning entirely.
	if err := cl.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-613-instances"}}); err != nil {
		t.Fatal(err)
	}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": gvr.GroupVersion().String(),
		"kind":       gvk.Kind,
		"metadata":   map[string]any{"name": "omits-decimals", "namespace": "team-613-instances"},
		"spec":       map[string]any{},
	}}
	var created *unstructured.Unstructured
	if err := wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		c, err := dyn.Resource(gvr).Namespace("team-613-instances").Create(ctx, obj.DeepCopy(), metav1.CreateOptions{})
		if err != nil {
			return false, nil // the CRD may not be serving yet
		}
		created = c
		return true, nil
	}); err != nil {
		t.Fatalf("could not create a composition through %s", gvr)
	}
	got, err := dyn.Resource(gvr).Namespace("team-613-instances").Get(ctx, created.GetName(), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	specOut, _ := json.Marshal(got.Object["spec"])
	t.Logf("STORED composition spec (created with spec: {}): %s", specOut)
	gotCount, _, _ := unstructured.NestedFieldNoCopy(got.Object, "spec", "count")
	gotRatio, _, _ := unstructured.NestedFieldNoCopy(got.Object, "spec", "ratio")
	gotTuningRatio, _, _ := unstructured.NestedFieldNoCopy(got.Object, "spec", "tuning", "ratio")
	if num(gotCount) != 3 {
		t.Fatalf("control: spec.count = %v, want 3", gotCount)
	}
	if num(gotRatio) != 0.5 || num(gotTuningRatio) != 0.5 {
		t.Fatalf("the apiserver did not apply the decimal defaults: spec.ratio = %v, spec.tuning.ratio = %v, want 0.5 (core-provider#613)", gotRatio, gotTuningRatio)
	}
}

func num(v any) float64 {
	switch n := v.(type) {
	case int64:
		return float64(n)
	case float64:
		return n
	case json.Number:
		f, _ := n.Float64()
		return f
	}
	return -1
}

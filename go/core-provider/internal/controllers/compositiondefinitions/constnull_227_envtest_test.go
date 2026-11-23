//go:build envtest

package compositiondefinitions

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/cdreason"
	"github.com/krateo-platformops/core-provider/internal/tools/chart"
	"github.com/krateo-platformops/core-provider/internal/tools/chart/chartfs"
	crdutils "github.com/krateo-platformops/core-provider/internal/tools/crd/generation"
	"github.com/krateo-platformops/plumbing/crdgen"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
)

// constNullSchema is an AUTHORED chart schema with two `const: null` fields:
//   - x is typed and non-nullable: plumbing v1.106.0 (#229, fixing plumbing#227) enforces it as
//     enum: [null], so the field may be omitted but any value is refused;
//   - y is nullable: v1.106.0 drops the constraint and reports it as ConstraintDropped.
//
// replicas is a plain typed field, the control that ordinary values still validate.
const constNullSchema = `{
  "type": "object",
  "properties": {
    "replicas": {"type": "integer"},
    "x": {"type": "string", "const": null},
    "y": {"type": ["string", "null"], "const": null}
  }
}`

// The plumbing v1.106.0 tightening, end to end through core-provider: what an authored chart schema
// with `const: null` turns into, as the apiserver stores and enforces it, and what core-provider's
// SchemaFullyEnforced condition says about it.
//
// Before v1.106.0 crdgen carried `const: null` on a typed field as nothing at all, so the generated
// CRD admitted `x: "a"`. On kp/main at plumbing v1.105.0 this test fails at the x arm.
func TestEnvtest_227_ConstNullThroughCoreProvider(t *testing.T) {
	ctx := context.Background()
	cl, dyn := startAddEnv(t)

	pkg, err := chartfs.FromReader(bytes.NewReader(markerChartTGZ(t, constNullSchema)), "test://widget-0.1.0.tgz")
	if err != nil {
		t.Fatal(err)
	}
	spec, err := chart.SpecSchema(pkg.FS(), pkg.RootDir(), false)
	if err != nil {
		t.Fatalf("SpecSchema: %v", err)
	}
	if spec.Source != chart.SchemaAuthored {
		t.Fatalf("Source = %q, want authored", spec.Source)
	}
	gvk, err := chart.ChartGroupVersionKind(pkg.FS(), pkg.RootDir())
	if err != nil {
		t.Fatal(err)
	}
	gen, err := crdutils.GenerateCRDWithWarnings(spec.Bytes, gvk)
	if err != nil {
		t.Fatalf("GenerateCRDWithWarnings: %v", err)
	}
	for _, w := range gen.Warnings {
		t.Logf("crdgen warning: kind=%s path=%q loses=%v message=%q", w.Kind, w.Path, w.Kind.LosesEnforcement(), w.Message)
	}

	// y: the nullable `const: null` is dropped and REPORTED, and core-provider's condition says so.
	var yDropped bool
	for _, w := range gen.Warnings {
		if w.Kind == crdgen.KindConstraintDropped && strings.Contains(w.Path, "y") {
			yDropped = true
		}
	}
	if !yDropped {
		t.Fatalf("the nullable y's `const: null` was not reported as %s (plumbing#227: it used to vanish silently)", crdgen.KindConstraintDropped)
	}
	enforced := schemaEnforcedCondition(gen.Warnings)
	wantEnforced := cdreason.SchemaConstraintsNotEnforced.WithMessage("")
	t.Logf("SchemaFullyEnforced: %s/%s: %s", enforced.Status, enforced.Reason, enforced.Message)
	if enforced.Status != wantEnforced.Status || enforced.Reason != wantEnforced.Reason {
		t.Fatalf("SchemaFullyEnforced = %s/%s, want %s/%s: a dropped constraint means the cluster accepts values the schema rejects",
			enforced.Status, enforced.Reason, wantEnforced.Status, wantEnforced.Reason)
	}

	cr := &compositiondefinitionsv1alpha1.CompositionDefinition{ObjectMeta: metav1.ObjectMeta{Name: "widget", Namespace: "team-227"}}
	cr.Spec.Chart = &compositiondefinitionsv1alpha1.ChartInfo{Url: "test://widget-0.1.0.tgz", Version: "0.1.0"}
	gvr, err := newExternal(cl, dyn).applyCompositionCRD(ctx, cr, gvk, gen.CRD.DeepCopy())
	if err != nil {
		t.Fatalf("applyCompositionCRD: %v", err)
	}

	// x, as STORED: enum [null].
	stored := &apiextensionsv1.CustomResourceDefinition{}
	if err := cl.Get(ctx, types.NamespacedName{Name: gen.CRD.Name}, stored); err != nil {
		t.Fatal(err)
	}
	var x apiextensionsv1.JSONSchemaProps
	for i := range stored.Spec.Versions {
		if stored.Spec.Versions[i].Name == gvk.Version {
			x = stored.Spec.Versions[i].Schema.OpenAPIV3Schema.Properties["spec"].Properties["x"]
		}
	}
	xJSON, _ := json.Marshal(x)
	t.Logf("STORED spec.x: %s", xJSON)

	if err := cl.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-227-instances"}}); err != nil {
		t.Fatal(err)
	}
	create := func(name string, spec map[string]any) error {
		obj := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": gvr.GroupVersion().String(),
			"kind":       gvk.Kind,
			"metadata":   map[string]any{"name": name, "namespace": "team-227-instances"},
			"spec":       spec,
		}}
		_, err := dyn.Resource(gvr).Namespace("team-227-instances").Create(ctx, obj, metav1.CreateOptions{})
		return err
	}

	// Control first, retried until the CRD serves: omitting x is admitted, and a typed field validates.
	if err := wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		return create("omits-x", map[string]any{"replicas": int64(2)}) == nil, nil
	}); err != nil {
		t.Fatalf("control: a composition omitting x was not admitted through %s", gvr)
	}
	if err := create("bad-replicas", map[string]any{"replicas": "two"}); !apierrors.IsInvalid(err) {
		t.Fatalf("control: replicas: \"two\" should be Invalid (the CRD validates at all), got %v", err)
	}

	// The tightening: any value for x is refused.
	err = create("sets-x", map[string]any{"x": "a"})
	t.Logf("create with x: \"a\" -> %v", err)
	if !apierrors.IsInvalid(err) {
		t.Fatalf("x: \"a\" was admitted (err=%v): the schema says x must be null (const: null), plumbing v1.106.0 enforces it as enum [null]", err)
	}
	if !strings.Contains(err.Error(), "spec.x") {
		t.Fatalf("the refusal does not name spec.x: %v", err)
	}
}

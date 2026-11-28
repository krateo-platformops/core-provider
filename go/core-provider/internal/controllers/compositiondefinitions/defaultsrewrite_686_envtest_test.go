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
	"sigs.k8s.io/yaml"
)

const (
	rewriteSchema = `{"type":"object","properties":{` +
		`"replicas":{"type":"integer"},` +
		`"restdefinitions":{"type":"object","properties":{"quota":{"type":"object","properties":{"enabled":{"type":"boolean","default":false}}}}}}}`
	rewriteValues = "replicas: 3\nrestdefinitions:\n  quota:\n    enabled: true\n"
)

// helmValues is what the cdc hands helm for a composition: its spec as READ through the apiserver,
// merged OVER the chart's values.yaml (helm: user values win, maps merge deeply, lists replace).
// Computed here from the objects and the file, never from the code under test.
func helmValues(t *testing.T, valuesYAML string, spec map[string]any) []byte {
	t.Helper()
	var base map[string]any
	if err := yaml.Unmarshal([]byte(valuesYAML), &base); err != nil {
		t.Fatal(err)
	}
	var merge func(dst, src map[string]any) map[string]any
	merge = func(dst, src map[string]any) map[string]any {
		for k, v := range src {
			if sm, ok := v.(map[string]any); ok {
				if dm, ok := dst[k].(map[string]any); ok {
					dst[k] = merge(dm, sm)
					continue
				}
			}
			dst[k] = v
		}
		return dst
	}
	out, err := json.Marshal(merge(base, spec)) // encoding/json sorts map keys: byte-comparable
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// #686, the proof the in-place rewrite rests on, on a real apiserver. A CRD generated the OLD way
// (schema defaults) is rewritten with values.yaml's defaults by the production path; the decision
// says so without a chart-version change; stored values stay as stored; and the values handed to
// helm for every composition are byte-identical before and after.
func TestEnvtest_686_DefaultsOnlyRewriteChangesNoCompositionsHelmValues(t *testing.T) {
	ctx := context.Background()
	cl, dyn := startAddEnv(t)

	pkg, err := chartfs.FromReader(bytes.NewReader(chartTGZWithSchema(t, "rewrite", rewriteSchema, rewriteValues)), "test://rewrite-0.1.0.tgz")
	if err != nil {
		t.Fatal(err)
	}
	spec, err := chart.SpecSchema(pkg.FS(), pkg.RootDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	gvk, err := chart.ChartGroupVersionKind(pkg.FS(), pkg.RootDir())
	if err != nil {
		t.Fatal(err)
	}

	// 1. The OLD CRD: crdgen's render, schema defaults (quota.enabled false), no values.yaml applied.
	old, err := crdutils.GenerateCRD(spec.Bytes, gvk)
	if err != nil {
		t.Fatal(err)
	}
	cr := &compositiondefinitionsv1alpha1.CompositionDefinition{ObjectMeta: metav1.ObjectMeta{Name: "rewrite", Namespace: "team-686r"}}
	cr.Spec.Chart = &compositiondefinitionsv1alpha1.ChartInfo{Url: "test://rewrite-0.1.0.tgz", Version: "0.1.0"}
	ext := newExternal(cl, dyn)
	gvr, err := ext.applyCompositionCRD(ctx, cr, gvk, old.DeepCopy())
	if err != nil {
		t.Fatalf("applying the old CRD: %v", err)
	}
	if err := cl.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-686r-instances"}}); err != nil {
		t.Fatal(err)
	}
	create := func(name string, spec map[string]any) {
		t.Helper()
		obj := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": gvr.GroupVersion().String(), "kind": gvk.Kind,
			"metadata": map[string]any{"name": name, "namespace": "team-686r-instances"},
			"spec":     spec,
		}}
		if err := wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
			_, err := dyn.Resource(gvr).Namespace("team-686r-instances").Create(ctx, obj.DeepCopy(), metav1.CreateOptions{})
			return err == nil, nil
		}); err != nil {
			t.Fatalf("creating %s", name)
		}
	}
	// 2. "omits": quota.enabled omitted, so the OLD default (false) is stored; replicas omitted and the
	// old CRD has no default for it, so it is stored without it. "explicit": quota.enabled true stored.
	create("omits", map[string]any{})
	create("explicit", map[string]any{"restdefinitions": map[string]any{"quota": map[string]any{"enabled": true}}})

	read := func(name string) map[string]any {
		t.Helper()
		o, err := dyn.Resource(gvr).Namespace("team-686r-instances").Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		s, _ := o.Object["spec"].(map[string]any)
		return s
	}
	specJSON := func(s map[string]any) string { b, _ := json.Marshal(s); return string(b) }
	beforeOmits, beforeExplicit := read("omits"), read("explicit")
	hvBeforeOmits := helmValues(t, rewriteValues, beforeOmits)
	hvBeforeExplicit := helmValues(t, rewriteValues, beforeExplicit)
	t.Logf("BEFORE spec omits=%s explicit=%s", specJSON(beforeOmits), specJSON(beforeExplicit))
	t.Logf("BEFORE helm values omits=%s explicit=%s", hvBeforeOmits, hvBeforeExplicit)
	if v, _, _ := unstructured.NestedBool(beforeOmits, "restdefinitions", "quota", "enabled"); v {
		t.Fatalf("premise: the old default false must be stored for the composition that omitted it; spec=%s", specJSON(beforeOmits))
	}
	if _, found := beforeOmits["replicas"]; found {
		t.Fatalf("premise: the old CRD has no default for replicas, so it is stored without it; spec=%s", specJSON(beforeOmits))
	}

	// 3. The new render (values.yaml's defaults) against the SERVED CRD: the production decision.
	live := &apiextensionsv1.CustomResourceDefinition{}
	if err := cl.Get(ctx, types.NamespacedName{Name: old.Name}, live); err != nil {
		t.Fatal(err)
	}
	gen, _, err := generatedFromChart(spec, gvk)
	if err != nil {
		t.Fatal(err)
	}
	cond, refresh, drift, err := specDriftDecision(live, gen.CRD)
	if err != nil {
		t.Fatal(err)
	}
	if cond.Reason != "LooserSchemaPending" || !refresh || !drift.DefaultsOnly {
		t.Fatalf("decision = %s refresh=%v defaultsOnly=%v (%s), want LooserSchemaPending, true, true", cond.Reason, refresh, drift.DefaultsOnly, cond.Message)
	}
	// Update's write, with the SAME chart version.
	if _, err := ext.applyCompositionCRD(ctx, cr, gvk, gen.CRD.DeepCopy()); err != nil {
		t.Fatalf("the rewrite: %v", err)
	}
	if err := cl.Get(ctx, types.NamespacedName{Name: old.Name}, live); err != nil {
		t.Fatal(err)
	}
	cond, refresh, _, err = specDriftDecision(live, gen.CRD)
	if err != nil {
		t.Fatal(err)
	}
	if cond.Reason != "Current" || cond.Status != metav1.ConditionTrue || refresh {
		t.Fatalf("after the rewrite: %s/%s refresh=%v (%s), want True/Current without a chart change", cond.Status, cond.Reason, refresh, cond.Message)
	}
	// The apiserver now serves values.yaml's defaults: a new composition omitting both gets them.
	create("fresh", map[string]any{})
	fresh := read("fresh")
	t.Logf("fresh composition after the rewrite: %s", specJSON(fresh))
	if v, found, _ := unstructured.NestedBool(fresh, "restdefinitions", "quota", "enabled"); !found || !v {
		t.Fatalf("a new composition must default quota.enabled to values.yaml's true; spec=%s", specJSON(fresh))
	}

	// 4. After.
	afterOmits, afterExplicit := read("omits"), read("explicit")
	hvAfterOmits := helmValues(t, rewriteValues, afterOmits)
	hvAfterExplicit := helmValues(t, rewriteValues, afterExplicit)
	t.Logf("AFTER  spec omits=%s explicit=%s", specJSON(afterOmits), specJSON(afterExplicit))
	t.Logf("AFTER  helm values omits=%s explicit=%s", hvAfterOmits, hvAfterExplicit)

	// Stored values stay as stored: the one that stored the old default still reads false.
	if v, found, _ := unstructured.NestedBool(afterOmits, "restdefinitions", "quota", "enabled"); !found || v {
		t.Fatalf("the composition that stored the old default must still read false; spec=%s", specJSON(afterOmits))
	}
	// THE assertion the decision rests on: what helm is handed does not change.
	if !bytes.Equal(hvBeforeOmits, hvAfterOmits) {
		t.Fatalf("helm values changed for omits:\n before %s\n after  %s", hvBeforeOmits, hvAfterOmits)
	}
	if !bytes.Equal(hvBeforeExplicit, hvAfterExplicit) {
		t.Fatalf("helm values changed for explicit:\n before %s\n after  %s", hvBeforeExplicit, hvAfterExplicit)
	}
}

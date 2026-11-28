//go:build envtest

package compositiondefinitions

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"testing"
	"time"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/tools/chart"
	"github.com/krateo-platformops/core-provider/internal/tools/chart/chartfs"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/wait"
)

// nova-kog 0.1.10's shape (#686): restdefinitions.quota.enabled is false in values.schema.json and true
// in values.yaml; restdefinitions.group.enabled has a schema default and no values.yaml key.
const (
	novaEnvtestSchema = `{"type":"object","properties":{"restdefinitions":{"type":"object","properties":{` +
		`"quota":{"type":"object","properties":{"enabled":{"type":"boolean","default":false}}},` +
		`"group":{"type":"object","properties":{"enabled":{"type":"boolean","default":false}}}}}}}`
	novaEnvtestValues = "restdefinitions:\n  quota:\n    enabled: true\n"
)

func chartTGZWithSchema(t *testing.T, name, schemaJSON, values string) []byte {
	t.Helper()
	files := map[string]string{
		name + "/Chart.yaml":         "apiVersion: v2\nname: " + name + "\nversion: 0.1.0\n",
		name + "/values.yaml":        values,
		name + "/values.schema.json": schemaJSON,
		name + "/templates/cm.yaml":  "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: {{ .Release.Name }}\n",
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

// #686 on a real apiserver: a composition that omits `restdefinitions` stores values.yaml's value
// (quota.enabled = true), not the schema's (false), which is what a `helm install` of the same chart
// would use; and a schema default with no values.yaml key (group.enabled) is NOT applied.
func TestEnvtest_686_ACompositionStoresValuesYamlsDefault(t *testing.T) {
	ctx := context.Background()
	cl, dyn := startAddEnv(t)

	pkg, err := chartfs.FromReader(bytes.NewReader(chartTGZWithSchema(t, "nova", novaEnvtestSchema, novaEnvtestValues)), "test://nova-0.1.0.tgz")
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
	gen, _, err := generatedFromChart(spec, gvk)
	if err != nil {
		t.Fatalf("generatedFromChart: %v", err)
	}
	cr := &compositiondefinitionsv1alpha1.CompositionDefinition{ObjectMeta: metav1.ObjectMeta{Name: "nova", Namespace: "team-686"}}
	cr.Spec.Chart = &compositiondefinitionsv1alpha1.ChartInfo{Url: "test://nova-0.1.0.tgz", Version: "0.1.0"}
	gvr, err := newExternal(cl, dyn).applyCompositionCRD(ctx, cr, gvk, gen.CRD.DeepCopy())
	if err != nil {
		t.Fatalf("applyCompositionCRD: %v", err)
	}

	if err := cl.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-686-instances"}}); err != nil {
		t.Fatal(err)
	}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": gvr.GroupVersion().String(),
		"kind":       gvk.Kind,
		"metadata":   map[string]any{"name": "omits-restdefinitions", "namespace": "team-686-instances"},
		"spec":       map[string]any{},
	}}
	var name string
	if err := wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		c, err := dyn.Resource(gvr).Namespace("team-686-instances").Create(ctx, obj.DeepCopy(), metav1.CreateOptions{})
		if err != nil {
			return false, nil // the CRD may not be serving yet
		}
		name = c.GetName()
		return true, nil
	}); err != nil {
		t.Fatalf("could not create a composition through %s", gvr)
	}
	got, err := dyn.Resource(gvr).Namespace("team-686-instances").Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	specOut, _ := json.Marshal(got.Object["spec"])
	t.Logf("STORED composition spec (created with spec: {}): %s", specOut)

	quota, found, _ := unstructured.NestedBool(got.Object, "spec", "restdefinitions", "quota", "enabled")
	if !found || !quota {
		t.Fatalf("spec.restdefinitions.quota.enabled = %v (found %v), want true: values.yaml's, not the schema's false (#686)", quota, found)
	}
	if v, found, _ := unstructured.NestedFieldNoCopy(got.Object, "spec", "restdefinitions", "group", "enabled"); found {
		t.Fatalf("spec.restdefinitions.group.enabled = %v, want absent: its schema default has no values.yaml key, so helm would not apply it", v)
	}
}

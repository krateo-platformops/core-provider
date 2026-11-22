//go:build envtest

package getters

// core-provider#568 child 8 (stability across chart versions): what an instance experiences when a
// definition with an INFERRED schema (krateo.io/values-schema: infer) moves to a chart version whose
// inferred schema differs. Every schema here comes from the production seam (chart.SpecSchema, i.e.
// plumbing valuesschema rules v1), every CRD from the production generator and write
// (generation.GenerateCRD, crd.ApplyOrUpdateCRD), and the migration is the production
// UpdateCompositionsVersion, against a real apiserver.
//
// THE KYVERNO RECORD (measured on the real charts, kyverno 3.7.0 -> 3.8.0 from
// https://kyverno.github.io/kyverno/, rules v1): 1059 -> 1114 inferred paths; 2 removed, 57 added,
// 2 shape changes, 3 changed defaults. The epic predicted reportsServer.readinessTimeout 300 ->
// "300s" would be refused (an integer -> string type change). Under the shipped rules it is NOT a
// type change: D5 infers every integer and string as x-kubernetes-int-or-string, so both versions
// accept 300 and "300s"; only the default moves. The two shape changes (test.image.tag and
// webhooksCleanup.image.tag, "v0.1.0" -> null) LOOSEN int-or-string to untyped, and the removed
// webhooksCleanup.autoDeleteWebhooks sits under a typed-OPEN parent (D3), so a stored value there is
// preserved, not pruned. The kyverno fixture below reproduces those three real moves, and the
// migration must succeed with nothing lost.
//
// What int-or-string does NOT absorb is a change of KIND: a boolean that becomes a string (or a map
// that becomes a scalar). No such key moves between kyverno 3.7.0 and 3.8.0, so that arm is a
// synthetic fixture: `features.enabled: true` -> `features.enabled: "true"`.
//
// MEASURED, and NOT what the epic predicted ("the migration would fail and retry", reasoned from
// getters.go): the migration SUCCEEDS. The apiserver ratchets CRD validation (validation ratcheting,
// beta and on by default from Kubernetes 1.30, GA in 1.33; envtest runs 1.36): an update that leaves
// an invalid field UNCHANGED is admitted. The migration write changes only the version label, so
// the instance moves to v2-0-0 still holding a value v2-0-0 refuses. The CONTROL is that a fresh
// create of the same boolean through v2-0-0 is refused. From then on any write that CHANGES that
// field must satisfy the new schema, while writes that leave it alone pass. The field-loss
// prediction (#188) reports nothing, because nothing is pruned. So today nothing surfaces it: no
// condition, no event, no error. Whether core-provider should report such an instance is an OPEN
// QUESTION recorded on #568, not a behaviour this test endorses. This subtest pins what happens, so
// a change to it (by the apiserver or by core-provider) is noticed.
//
// The compositiondefinitions.go migrate loop returns a migration error from Update ("error
// migrating compositions from version ..."), which provider-runtime records as
// Synced=False/ReconcileError. With ratcheting, this fixture never reaches that path.

import (
	"context"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/krateo-platformops/core-provider/internal/tools/chart"
	crdtools "github.com/krateo-platformops/core-provider/internal/tools/crd"
	"github.com/krateo-platformops/core-provider/internal/tools/crd/fieldloss"
	"github.com/krateo-platformops/core-provider/internal/tools/crd/generation"
	"github.com/krateo-platformops/core-provider/internal/tools/deploy"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

const (
	group568 = "composition.krateo.io"
	kind568  = "Inferred568"
)

// valuesV1/valuesV2 mirror the three real kyverno 3.7.0 -> 3.8.0 moves, plus one synthetic
// boolean -> string change (features.enabled) that rules v1 does not absorb.
const valuesV1 = `reportsServer:
  readinessTimeout: 300
test:
  image:
    tag: v0.1.0
webhooksCleanup:
  enabled: true
  autoDeleteWebhooks:
    enabled: false
features:
  enabled: true
`

const valuesV2 = `reportsServer:
  readinessTimeout: 300s
test:
  image:
    tag:
webhooksCleanup:
  enabled: true
features:
  enabled: "true"
`

func inferred568(t *testing.T, version, values string, chartExtra ...string) []byte {
	t.Helper()
	fsys := fstest.MapFS{
		"c/Chart.yaml":  {Data: []byte("apiVersion: v2\nname: inferred568\nversion: " + version + "\n" + strings.Join(chartExtra, ""))},
		"c/values.yaml": {Data: []byte(values)},
	}
	r, err := chart.SpecSchema(fsys, "c", true)
	if err != nil {
		t.Fatalf("SpecSchema %s: %v", version, err)
	}
	if r.Source != chart.SchemaInferred {
		t.Fatalf("premise: chart %s has no values.schema.json, so its schema must be inferred; source=%v", version, r.Source)
	}
	return r.Bytes
}

func waitServed568(t *testing.T, cl client.Client, name, version string) {
	t.Helper()
	if err := wait.PollUntilContextTimeout(context.Background(), 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		var c apiextensionsv1.CustomResourceDefinition
		if err := cl.Get(ctx, client.ObjectKey{Name: name}, &c); err != nil {
			return false, nil
		}
		if !crdtools.IsReady(&c) {
			return false, nil
		}
		for _, v := range c.Spec.Versions {
			if v.Name == version && v.Served {
				return true, nil
			}
		}
		return false, nil
	}); err != nil {
		t.Fatalf("CRD %s never served %s", name, version)
	}
}

func TestEnvtest_568_InferredSchemaAcrossChartVersions(t *testing.T) {
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	scheme := runtime.NewScheme()
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cl, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	dyn := dynamic.NewForConfigOrDie(cfg)
	ctx := context.Background()

	gvk1 := schema.GroupVersionKind{Group: group568, Version: "v1-0-0", Kind: kind568}
	gvk2 := schema.GroupVersionKind{Group: group568, Version: "v2-0-0", Kind: kind568}
	crd1, err := generation.GenerateCRD(inferred568(t, "1.0.0", valuesV1), gvk1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := crdtools.ApplyOrUpdateCRD(ctx, cl, dyn, crd1.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	waitServed568(t, cl, crd1.Name, "v1-0-0")
	gvr1 := schema.GroupVersionResource{Group: group568, Version: "v1-0-0", Resource: crd1.Spec.Names.Plural}
	gvr2 := gvr1
	gvr2.Version = "v2-0-0"

	// Two instances, each owned by its own definition, so each migration is driven separately.
	mk := func(name, owner string, spec map[string]any) {
		t.Helper()
		u := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": group568 + "/v1-0-0", "kind": kind568,
			"metadata": map[string]any{"name": name, "namespace": "default", "labels": map[string]any{
				deploy.CompositionVersionLabel:             "v1-0-0",
				deploy.CompositionDefinitionNameLabel:      owner,
				deploy.CompositionDefinitionNamespaceLabel: "default",
			}},
			"spec": spec,
		}}
		if err := wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 20*time.Second, true, func(ctx context.Context) (bool, error) {
			_, err := dyn.Resource(gvr1).Namespace("default").Create(ctx, u, metav1.CreateOptions{})
			return err == nil, nil
		}); err != nil {
			t.Fatalf("create %s through v1-0-0 never succeeded", name)
		}
	}
	// The kyverno-shaped instance: the values a user set under 3.7.0's inferred schema.
	mk("kyverno-shaped", "def-kyverno", map[string]any{
		"reportsServer":   map[string]any{"readinessTimeout": int64(300)},
		"test":            map[string]any{"image": map[string]any{"tag": "v0.1.0"}},
		"webhooksCleanup": map[string]any{"enabled": true, "autoDeleteWebhooks": map[string]any{"enabled": true}},
	})
	// The refusing instance: a boolean the next version infers as int-or-string.
	mk("bool-to-string", "def-bool", map[string]any{"features": map[string]any{"enabled": true}})
	// The positive control for "nothing is lost": the same values as kyverno-shaped.
	mk("closed-control", "def-closed", map[string]any{
		"webhooksCleanup": map[string]any{"enabled": true, "autoDeleteWebhooks": map[string]any{"enabled": true}},
	})

	// The definition moves to chart 2.0.0: Update writes the new version beside the old one.
	crd2, err := generation.GenerateCRD(inferred568(t, "2.0.0", valuesV2), gvk2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := crdtools.ApplyOrUpdateCRD(ctx, cl, dyn, crd2.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	waitServed568(t, cl, crd2.Name, "v2-0-0")
	var live apiextensionsv1.CustomResourceDefinition
	if err := cl.Get(ctx, client.ObjectKey{Name: crd2.Name}, &live); err != nil {
		t.Fatal(err)
	}
	var target *apiextensionsv1.JSONSchemaProps
	for _, v := range live.Spec.Versions {
		if v.Name == "v2-0-0" {
			target = v.Schema.OpenAPIV3Schema
		}
	}
	ts, err := fieldloss.Structural(target)
	if err != nil {
		t.Fatal(err)
	}
	var lost []string
	opts := MigrationOptions{TargetSchema: ts, OnLoss: func(_ *unstructured.Unstructured, rep fieldloss.Report, _ bool) { lost = append(lost, rep.Paths...) }}
	get := func(name string) *unstructured.Unstructured {
		t.Helper()
		u, err := dyn.Resource(gvr2).Namespace("default").Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return u
	}

	t.Run("the kyverno 3.7.0 -> 3.8.0 moves migrate, and nothing is lost", func(t *testing.T) {
		lost = nil
		if _, err := UpdateCompositionsVersion(ctx, dyn, gvr2, "v1-0-0", "v2-0-0", DefinitionRef{Name: "def-kyverno", Namespace: "default"}, opts); err != nil {
			t.Fatalf("readinessTimeout 300 under an int-or-string node, a tag under a now-untyped node and a key removed under a typed-open parent must all migrate: %v", err)
		}
		u := get("kyverno-shaped")
		if got := u.GetLabels()[deploy.CompositionVersionLabel]; got != "v2-0-0" {
			t.Fatalf("migrated instance is labelled %q, want v2-0-0", got)
		}
		if v, _, _ := unstructured.NestedInt64(u.Object, "spec", "reportsServer", "readinessTimeout"); v != 300 {
			t.Errorf("readinessTimeout = %d after migration, want 300 kept (int-or-string accepts it)", v)
		}
		if v, _, _ := unstructured.NestedString(u.Object, "spec", "test", "image", "tag"); v != "v0.1.0" {
			t.Errorf("test.image.tag = %q after migration, want v0.1.0 kept", v)
		}
		if v, found, _ := unstructured.NestedBool(u.Object, "spec", "webhooksCleanup", "autoDeleteWebhooks", "enabled"); !found || !v {
			t.Errorf("webhooksCleanup.autoDeleteWebhooks.enabled (removed in 2.0.0, under a typed-open parent) found=%v value=%v, want preserved true", found, v)
		}
		if len(lost) != 0 {
			t.Errorf("the field-loss prediction reported %v, want nothing: every map is typed-open", lost)
		}
	})

	t.Run("a boolean that becomes a string migrates under validation ratcheting, holding a value the new version refuses", func(t *testing.T) {
		// CONTROL: the new version does refuse the value, on a write that introduces it.
		fresh := &unstructured.Unstructured{Object: map[string]any{"apiVersion": group568 + "/v2-0-0", "kind": kind568,
			"metadata": map[string]any{"name": "control-fresh-bool", "namespace": "default"},
			"spec":     map[string]any{"features": map[string]any{"enabled": true}}}}
		if _, err := dyn.Resource(gvr2).Namespace("default").Create(ctx, fresh, metav1.CreateOptions{}); !apierrors.IsInvalid(err) ||
			!strings.Contains(err.Error(), "spec.features.enabled") {
			t.Fatalf("control: a fresh boolean through v2-0-0 must be refused as Invalid naming spec.features.enabled; got %v", err)
		}

		lost = nil
		if _, err := UpdateCompositionsVersion(ctx, dyn, gvr2, "v1-0-0", "v2-0-0", DefinitionRef{Name: "def-bool", Namespace: "default"}, opts); err != nil {
			t.Fatalf("measured: the label-only migration write is admitted by validation ratcheting; got %v", err)
		}
		u := get("bool-to-string")
		if got := u.GetLabels()[deploy.CompositionVersionLabel]; got != "v2-0-0" {
			t.Fatalf("instance is labelled %q, want v2-0-0 (migrated)", got)
		}
		if v, found, _ := unstructured.NestedBool(u.Object, "spec", "features", "enabled"); !found || !v {
			t.Fatalf("features.enabled found=%v value=%v, want the stored boolean true carried over unchanged", found, v)
		}
		if len(lost) != 0 {
			t.Errorf("field-loss prediction reported %v; nothing is pruned here", lost)
		}

		// A write that leaves the field alone passes.
		_ = unstructured.SetNestedField(u.Object, "touched", "metadata", "annotations", "probe.krateo.io/568")
		if _, err := dyn.Resource(gvr2).Namespace("default").Update(ctx, u, metav1.UpdateOptions{}); err != nil {
			t.Fatalf("an update that leaves features.enabled unchanged must pass (ratcheting); got %v", err)
		}
		// A write that changes it must satisfy the new schema.
		u = get("bool-to-string")
		_ = unstructured.SetNestedField(u.Object, false, "spec", "features", "enabled")
		if _, err := dyn.Resource(gvr2).Namespace("default").Update(ctx, u, metav1.UpdateOptions{}); !apierrors.IsInvalid(err) ||
			!strings.Contains(err.Error(), "spec.features.enabled") {
			t.Fatalf("changing features.enabled to another boolean must be refused as Invalid naming the field; got %v", err)
		}
		u = get("bool-to-string")
		_ = unstructured.SetNestedField(u.Object, "false", "spec", "features", "enabled")
		if _, err := dyn.Resource(gvr2).Namespace("default").Update(ctx, u, metav1.UpdateOptions{}); err != nil {
			t.Fatalf("changing features.enabled to a string satisfies the new schema and must pass; got %v", err)
		}
	})
	t.Run("control: a map the next version closes does lose the removed key, and the prediction reports it", func(t *testing.T) {
		// valuesschema's ONE closing mechanism (D3): the Chart.yaml annotation krateo.io/values-schema-closed.
		gvk3 := schema.GroupVersionKind{Group: group568, Version: "v3-0-0", Kind: kind568}
		crd3, err := generation.GenerateCRD(inferred568(t, "3.0.0", valuesV2,
			"annotations:\n  krateo.io/values-schema-closed: webhooksCleanup\n"), gvk3)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := crdtools.ApplyOrUpdateCRD(ctx, cl, dyn, crd3.DeepCopy()); err != nil {
			t.Fatal(err)
		}
		waitServed568(t, cl, crd3.Name, "v3-0-0")
		var live3 apiextensionsv1.CustomResourceDefinition
		if err := cl.Get(ctx, client.ObjectKey{Name: crd3.Name}, &live3); err != nil {
			t.Fatal(err)
		}
		var target3 *apiextensionsv1.JSONSchemaProps
		for _, v := range live3.Spec.Versions {
			if v.Name == "v3-0-0" {
				target3 = v.Schema.OpenAPIV3Schema
			}
		}
		ts3, err := fieldloss.Structural(target3)
		if err != nil {
			t.Fatal(err)
		}
		var lost3 []string
		opts3 := MigrationOptions{TargetSchema: ts3, OnLoss: func(_ *unstructured.Unstructured, rep fieldloss.Report, _ bool) { lost3 = append(lost3, rep.Paths...) }}
		gvr3 := gvr1
		gvr3.Version = "v3-0-0"
		if _, err := UpdateCompositionsVersion(ctx, dyn, gvr3, "v1-0-0", "v3-0-0", DefinitionRef{Name: "def-closed", Namespace: "default"}, opts3); err != nil {
			t.Fatal(err)
		}
		if strings.Join(lost3, ",") != "spec.webhooksCleanup.autoDeleteWebhooks" {
			t.Fatalf("the field-loss prediction reported %v, want exactly [spec.webhooksCleanup.autoDeleteWebhooks]: without this the \"nothing is lost\" arm proves nothing", lost3)
		}
		u, err := dyn.Resource(gvr3).Namespace("default").Get(ctx, "closed-control", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if _, found, _ := unstructured.NestedFieldNoCopy(u.Object, "spec", "webhooksCleanup", "autoDeleteWebhooks"); found {
			t.Error("a closed map kept the removed key; the control did not exercise pruning")
		}
		if _, ok := u.GetAnnotations()[PrunedFieldsAnnotation]; !ok {
			t.Errorf("the pruned subtree must be stashed in %s", PrunedFieldsAnnotation)
		}
	})
}

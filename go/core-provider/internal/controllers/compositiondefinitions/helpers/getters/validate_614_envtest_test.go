//go:build envtest

package getters

// core-provider#614: ValidateOwnedInstances against a real apiserver, on #568 child 8's kind-change
// fixture (features.enabled: a boolean in 1.0.0's inferred schema, integer-or-string in 2.0.0's).
//
// THE PREMISE, measured here rather than assumed:
//   - the label-only migration is admitted (validation ratcheting), so the instance sits on v2-0-0
//     holding a boolean v2-0-0 refuses;
//   - a dry-run CREATE of a copy is validated in full (a create has no old object to ratchet
//     against) and refuses that boolean as Invalid, naming spec.features.enabled;
//   - a dry-run create under the instance's OWN name cannot be used: for a VALID instance it answers
//     AlreadyExists, which says nothing about the values. Hence the generated name.
//   - dry runs persist nothing: no krateo-validate-* object exists afterwards.
//
// MEASURED, and wider than #614 assumed: the kind change refuses EVERY instance created under
// 1.0.0, not only one whose user set features.enabled. 1.0.0's inferred schema defaults
// features.enabled to true (D4: defaults on leaves), so the apiserver stored the boolean on every
// create; under 2.0.0 each of them is refused. The first run of this test found it: its "compatible"
// instance, which never set the key, was refused too. The clean control is therefore a separate
// Kind whose two versions differ only in moves int-or-string absorbs.

import (
	"context"
	"strings"
	"testing"
	"time"

	crdtools "github.com/krateo-platformops/core-provider/internal/tools/crd"
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

func TestEnvtest_614_DryRunCreateValidatesWithoutRatcheting(t *testing.T) {
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
	owner := DefinitionRef{Name: "def-614", Namespace: "default"}

	mk := func(name string, spec map[string]any) {
		t.Helper()
		u := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": group568 + "/v1-0-0", "kind": kind568,
			"metadata": map[string]any{"name": name, "namespace": "default", "labels": map[string]any{
				deploy.CompositionVersionLabel:             "v1-0-0",
				deploy.CompositionDefinitionNameLabel:      owner.Name,
				deploy.CompositionDefinitionNamespaceLabel: owner.Namespace,
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
	mk("held-bool", map[string]any{"features": map[string]any{"enabled": true}})
	mk("defaulted-bool", map[string]any{"reportsServer": map[string]any{"readinessTimeout": int64(300)}})

	crd2, err := generation.GenerateCRD(inferred568(t, "2.0.0", valuesV2), gvk2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := crdtools.ApplyOrUpdateCRD(ctx, cl, dyn, crd2.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	waitServed568(t, cl, crd2.Name, "v2-0-0")

	moved, err := UpdateCompositionsVersion(ctx, dyn, gvr2, "v1-0-0", "v2-0-0", owner, MigrationOptions{})
	if err != nil {
		t.Fatalf("premise: the label-only migration is admitted under ratcheting; got %v", err)
	}
	if moved != 2 {
		t.Fatalf("UpdateCompositionsVersion moved %d, want 2", moved)
	}
	get := func(name string) *unstructured.Unstructured {
		t.Helper()
		u, err := dyn.Resource(gvr2).Namespace("default").Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	if v, found, _ := unstructured.NestedBool(get("held-bool").Object, "spec", "features", "enabled"); !found || !v {
		t.Fatalf("premise: held-bool must sit on v2-0-0 still holding the boolean; found=%v value=%v", found, v)
	}
	if v, found, _ := unstructured.NestedBool(get("defaulted-bool").Object, "spec", "features", "enabled"); !found || !v {
		t.Fatalf("premise: 1.0.0's default must have stored features.enabled=true on defaulted-bool; found=%v value=%v", found, v)
	}

	// The clean control: a Kind whose 1.0.0 -> 2.0.0 moves are all absorbed (no features key).
	strip := func(v string) string {
		i := strings.Index(v, "features:")
		return v[:i]
	}
	okKind := schema.GroupVersionKind{Group: group568, Version: "v1-0-0", Kind: "Compatible614"}
	okCRD1, err := generation.GenerateCRD(inferred568(t, "1.0.0", strip(valuesV1)), okKind)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := crdtools.ApplyOrUpdateCRD(ctx, cl, dyn, okCRD1.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	waitServed568(t, cl, okCRD1.Name, "v1-0-0")
	okGVR1 := schema.GroupVersionResource{Group: group568, Version: "v1-0-0", Resource: okCRD1.Spec.Names.Plural}
	okGVR2 := okGVR1
	okGVR2.Version = "v2-0-0"
	okOwner := DefinitionRef{Name: "def-614-ok", Namespace: "default"}
	okInst := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": group568 + "/v1-0-0", "kind": "Compatible614",
		"metadata": map[string]any{"name": "compatible", "namespace": "default", "labels": map[string]any{
			deploy.CompositionVersionLabel:             "v1-0-0",
			deploy.CompositionDefinitionNameLabel:      okOwner.Name,
			deploy.CompositionDefinitionNamespaceLabel: okOwner.Namespace,
		}},
		"spec": map[string]any{"reportsServer": map[string]any{"readinessTimeout": int64(300)}},
	}}
	if err := wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 20*time.Second, true, func(ctx context.Context) (bool, error) {
		_, err := dyn.Resource(okGVR1).Namespace("default").Create(ctx, okInst, metav1.CreateOptions{})
		return err == nil, nil
	}); err != nil {
		t.Fatal("create compatible through v1-0-0 never succeeded")
	}
	okCRD2, err := generation.GenerateCRD(inferred568(t, "2.0.0", strip(valuesV2)), schema.GroupVersionKind{Group: group568, Version: "v2-0-0", Kind: "Compatible614"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := crdtools.ApplyOrUpdateCRD(ctx, cl, dyn, okCRD2.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	waitServed568(t, cl, okCRD2.Name, "v2-0-0")
	if n, err := UpdateCompositionsVersion(ctx, dyn, okGVR2, "v1-0-0", "v2-0-0", okOwner, MigrationOptions{}); err != nil || n != 1 {
		t.Fatalf("compatible migration: moved %d, err %v; want 1, nil", n, err)
	}
	okGet := func() *unstructured.Unstructured {
		t.Helper()
		u, err := dyn.Resource(okGVR2).Namespace("default").Get(ctx, "compatible", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return u
	}

	t.Run("a dry-run create under the instance's own name cannot judge a valid instance", func(t *testing.T) {
		same := validationCopy(okGet())
		same.SetGenerateName("")
		same.SetName("compatible")
		_, err := dyn.Resource(okGVR2).Namespace("default").Create(ctx, same, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
		if !apierrors.IsAlreadyExists(err) {
			t.Fatalf("measured premise for the generated name: a same-name dry run of a VALID instance answers AlreadyExists; got %v", err)
		}
		// And the generated name reaches the verdict for the same instance.
		res, err := ValidateOwnedInstances(ctx, dyn, okGVR2, okOwner)
		if err != nil {
			t.Fatal(err)
		}
		if res.Checked != 1 || len(res.Refused) != 0 || len(res.Unchecked) != 0 {
			t.Fatalf("compatible control: %+v, want 1 checked, none refused or unchecked", res)
		}
	})

	t.Run("the refused instance is named with its field; the compatible one passes", func(t *testing.T) {
		res, err := ValidateOwnedInstances(ctx, dyn, gvr2, owner)
		if err != nil {
			t.Fatal(err)
		}
		if res.Checked != 2 {
			t.Fatalf("checked %d instances, want 2", res.Checked)
		}
		if len(res.Unchecked) != 0 {
			t.Fatalf("unchecked %+v, want none: every dry run here must reach a verdict", res.Unchecked)
		}
		names := []string{}
		for _, r := range res.Refused {
			if r.Namespace != "default" || strings.Join(r.Fields, ",") != "spec.features.enabled" {
				t.Fatalf("refused entry %+v must be in default and name exactly spec.features.enabled", r)
			}
			names = append(names, r.Name)
		}
		if strings.Join(names, ",") != "defaulted-bool,held-bool" {
			t.Fatalf("refused %v, want both v1-0-0 instances (one set the boolean, one got it from the default)", names)
		}
	})

	t.Run("dry runs persist nothing", func(t *testing.T) {
		l, err := dyn.Resource(gvr2).Namespace("default").List(ctx, metav1.ListOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for _, u := range l.Items {
			if strings.HasPrefix(u.GetName(), ValidationNamePrefix) {
				t.Fatalf("a dry-run copy was persisted: %s", u.GetName())
			}
		}
		if len(l.Items) != 2 {
			t.Fatalf("%d instances exist, want the 2 created", len(l.Items))
		}
		ok, err := dyn.Resource(okGVR2).Namespace("default").List(ctx, metav1.ListOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(ok.Items) != 1 {
			t.Fatalf("%d compatible instances exist, want 1 (no dry-run copy persisted)", len(ok.Items))
		}
	})

	t.Run("once the value satisfies the new version, nothing is refused", func(t *testing.T) {
		for _, n := range []string{"held-bool", "defaulted-bool"} {
			u := get(n)
			_ = unstructured.SetNestedField(u.Object, "true", "spec", "features", "enabled")
			if _, err := dyn.Resource(gvr2).Namespace("default").Update(ctx, u, metav1.UpdateOptions{}); err != nil {
				t.Fatalf("a string satisfies v2-0-0's int-or-string (%s): %v", n, err)
			}
		}
		res, err := ValidateOwnedInstances(ctx, dyn, gvr2, owner)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Refused) != 0 || len(res.Unchecked) != 0 || res.Checked != 2 {
			t.Fatalf("after the fix: %+v, want 2 checked, none refused or unchecked", res)
		}
	})
}

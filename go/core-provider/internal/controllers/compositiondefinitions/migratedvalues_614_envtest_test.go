//go:build envtest

package compositiondefinitions

// core-provider#614 end to end on a real apiserver: Update's migration step (migrateAndCheck) moves
// instances onto a version whose schema refuses a value one of them holds, the apiserver admits the
// label-only write (validation ratcheting), and MigratedValuesValid names the instance and field.
// Observe's recheck clears it once the value is fixed. Ready is never touched.

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/cdreason"
	crdtools "github.com/krateo-platformops/core-provider/internal/tools/crd"
	"github.com/krateo-platformops/core-provider/internal/tools/crd/generation"
	"github.com/krateo-platformops/core-provider/internal/tools/deploy"
	rtv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
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
	schema614v1 = `{"type":"object","properties":{"name":{"type":"string"},"features":{"type":"object","properties":{"enabled":{"type":"boolean"}}}}}`
	schema614v2 = `{"type":"object","properties":{"name":{"type":"string"},"features":{"type":"object","properties":{"enabled":{"type":"string"}}}}}`
)

func TestEnvtest_614_MigratedValuesValid(t *testing.T) {
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
	log := logging.NewNopLogger()

	const group, kind = "composition.krateo.io", "Widget614"
	apply := func(version, specSchema string) *apiextensionsv1.CustomResourceDefinition {
		t.Helper()
		crd, err := generation.GenerateCRD([]byte(specSchema), schema.GroupVersionKind{Group: group, Version: version, Kind: kind})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := crdtools.ApplyOrUpdateCRD(ctx, cl, dyn, crd.DeepCopy()); err != nil {
			t.Fatal(err)
		}
		if err := wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
			var c apiextensionsv1.CustomResourceDefinition
			if err := cl.Get(ctx, client.ObjectKey{Name: crd.Name}, &c); err != nil || !crdtools.IsReady(&c) {
				return false, nil
			}
			for _, v := range c.Spec.Versions {
				if v.Name == version && v.Served {
					return true, nil
				}
			}
			return false, nil
		}); err != nil {
			t.Fatalf("CRD never served %s", version)
		}
		return crd
	}
	crd1 := apply("v1-0-0", schema614v1)
	gvr1 := schema.GroupVersionResource{Group: group, Version: "v1-0-0", Resource: crd1.Spec.Names.Plural}
	gvr2 := gvr1
	gvr2.Version = "v2-0-0"
	gvk2 := schema.GroupVersionKind{Group: group, Version: "v2-0-0", Kind: kind}

	mk := func(name, owner string, spec map[string]any) {
		t.Helper()
		u := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": group + "/v1-0-0", "kind": kind,
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
			t.Fatalf("create %s never succeeded", name)
		}
	}
	mk("held", "def-614", map[string]any{"features": map[string]any{"enabled": true}})
	mk("plain", "def-614", map[string]any{"name": "x"})
	mk("other", "def-614-ok", map[string]any{"name": "y"})
	apply("v2-0-0", schema614v2)

	e := &external{dynamic: dyn, kube: cl}
	ready := rtv1.Condition{Type: rtv1.TypeReady, Status: metav1.ConditionTrue, Reason: rtv1.ReasonAvailable, Message: "before", LastTransitionTime: metav1.Now()}
	newCD := func(name string, versions ...string) *compositiondefinitionsv1alpha1.CompositionDefinition {
		cd := &compositiondefinitionsv1alpha1.CompositionDefinition{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}}
		for _, v := range versions {
			cd.Status.Managed.VersionInfo = append(cd.Status.Managed.VersionInfo, compositiondefinitionsv1alpha1.VersionDetail{Version: v})
		}
		cd.SetConditions(ready)
		return cd
	}
	readyUnchanged := func(t *testing.T, cd *compositiondefinitionsv1alpha1.CompositionDefinition) {
		t.Helper()
		if got := cd.GetCondition(rtv1.TypeReady); !reflect.DeepEqual(got, ready) {
			t.Fatalf("Ready changed: got %+v, want %+v", got, ready)
		}
	}

	t.Run("no migration: the condition stays absent", func(t *testing.T) {
		cd := newCD("def-614", "v2-0-0")
		if err := e.migrateAndCheck(ctx, cd, gvr2, gvk2, log); err != nil {
			t.Fatal(err)
		}
		if _, present := migratedValuesValid(cd); present {
			t.Fatalf("nothing migrated, yet MigratedValuesValid = %+v", cd.GetCondition(cdreason.TypeMigratedValuesValid))
		}
		readyUnchanged(t, cd)
	})

	cd := newCD("def-614", "v1-0-0", "v2-0-0")
	t.Run("a migration that carries a refused value is reported, naming instance and field", func(t *testing.T) {
		if err := e.migrateAndCheck(ctx, cd, gvr2, gvk2, log); err != nil {
			t.Fatalf("the migration must succeed (ratcheting) and the check must not fail it: %v", err)
		}
		c := cd.GetCondition(cdreason.TypeMigratedValuesValid)
		if !cdreason.MigratedValuesRefused.Is(c) {
			t.Fatalf("MigratedValuesValid = %+v, want False/ValuesRefusedByNewVersion", c)
		}
		for _, want := range []string{"1 of 2 instance(s) on version v2-0-0", "default/held (spec.features.enabled)"} {
			if !strings.Contains(c.Message, want) {
				t.Fatalf("message %q lacks %q", c.Message, want)
			}
		}
		if strings.Contains(c.Message, "default/plain") {
			t.Fatalf("message %q names the valid instance", c.Message)
		}
		readyUnchanged(t, cd)
	})

	t.Run("Observe's recheck keeps it while the value stands", func(t *testing.T) {
		before := cd.GetCondition(cdreason.TypeMigratedValuesValid)
		e.recheckMigratedValues(ctx, cd, gvr2, log)
		after := cd.GetCondition(cdreason.TypeMigratedValuesValid)
		if !cdreason.MigratedValuesRefused.Is(after) || after.Message != before.Message {
			t.Fatalf("recheck with nothing changed: %+v, want the same refusal", after)
		}
		readyUnchanged(t, cd)
	})

	t.Run("fixing the value clears it on the next recheck", func(t *testing.T) {
		u, err := dyn.Resource(gvr2).Namespace("default").Get(ctx, "held", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		_ = unstructured.SetNestedField(u.Object, "true", "spec", "features", "enabled")
		if _, err := dyn.Resource(gvr2).Namespace("default").Update(ctx, u, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		e.recheckMigratedValues(ctx, cd, gvr2, log)
		c := cd.GetCondition(cdreason.TypeMigratedValuesValid)
		if !cdreason.MigratedValuesAllValid.Is(c) || !strings.Contains(c.Message, "all 2 instance(s) on version v2-0-0") {
			t.Fatalf("after the fix: %+v, want True/AllValid over 2 instances", c)
		}
		readyUnchanged(t, cd)
	})

	t.Run("True is final until the next migration: the recheck does not run", func(t *testing.T) {
		quiet := &external{} // no clients: a recheck that ran would panic
		quiet.recheckMigratedValues(ctx, cd, gvr2, log)
		if !cdreason.MigratedValuesAllValid.Is(cd.GetCondition(cdreason.TypeMigratedValuesValid)) {
			t.Fatal("the condition changed")
		}
	})

	t.Run("a compatible migration reads True", func(t *testing.T) {
		ok := newCD("def-614-ok", "v1-0-0", "v2-0-0")
		if err := e.migrateAndCheck(ctx, ok, gvr2, gvk2, log); err != nil {
			t.Fatal(err)
		}
		c := ok.GetCondition(cdreason.TypeMigratedValuesValid)
		if !cdreason.MigratedValuesAllValid.Is(c) || !strings.Contains(c.Message, "all 1 instance(s)") {
			t.Fatalf("compatible migration: %+v, want True/AllValid over its 1 instance", c)
		}
		readyUnchanged(t, ok)
	})
}

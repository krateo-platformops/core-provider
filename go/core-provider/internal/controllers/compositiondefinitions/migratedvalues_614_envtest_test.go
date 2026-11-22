//go:build envtest

package compositiondefinitions

// core-provider#614 end to end on a real apiserver: Update's migration step (migrateAndCheck) moves
// instances onto a version whose schema refuses a value one of them holds, the apiserver admits the
// label-only write (validation ratcheting), and MigratedValuesValid names the instance and field.
// Observe's recheck clears it once the value is fixed. Ready is never touched.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
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
	// The CompositionDefinition CRD is installed so status (conditions and, since #623,
	// status.migratedValues) is round-tripped through the apiserver, not only checked in memory.
	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "..", "crds")}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	scheme := runtime.NewScheme()
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := compositiondefinitionsv1alpha1.SchemeBuilder.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := admissionregistrationv1.AddToScheme(scheme); err != nil {
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
	// persist writes cd's status to the apiserver (creating the definition the first time) and reads
	// it back, so the assertions and the samples see status as stored, through the CRD's schema.
	persist := func(t *testing.T, cd *compositiondefinitionsv1alpha1.CompositionDefinition) *compositiondefinitionsv1alpha1.CompositionDefinition {
		t.Helper()
		key := client.ObjectKey{Namespace: cd.Namespace, Name: cd.Name}
		var live compositiondefinitionsv1alpha1.CompositionDefinition
		if err := cl.Get(ctx, key, &live); err != nil {
			live = compositiondefinitionsv1alpha1.CompositionDefinition{ObjectMeta: metav1.ObjectMeta{Name: cd.Name, Namespace: cd.Namespace}}
			live.Spec.Chart = &compositiondefinitionsv1alpha1.ChartInfo{Url: "oci://example.invalid/charts/widget614", Version: "2.0.0"}
			if err := cl.Create(ctx, &live); err != nil {
				t.Fatalf("create definition %s: %v", cd.Name, err)
			}
		}
		live.Status = *cd.Status.DeepCopy()
		if err := cl.Status().Update(ctx, &live); err != nil {
			t.Fatalf("status update %s: %v", cd.Name, err)
		}
		var back compositiondefinitionsv1alpha1.CompositionDefinition
		if err := cl.Get(ctx, key, &back); err != nil {
			t.Fatal(err)
		}
		return &back
	}
	// sample writes the definition's status, as read back, for frontend (#623), when
	// KRATEO_MIGRATED_VALUES_SAMPLES_DIR is set.
	sample := func(t *testing.T, name string, back *compositiondefinitionsv1alpha1.CompositionDefinition) {
		t.Helper()
		dir := os.Getenv("KRATEO_MIGRATED_VALUES_SAMPLES_DIR")
		if dir == "" {
			return
		}
		out := map[string]any{"conditions": back.Status.Conditions, "migratedValues": back.Status.MigratedValues}
		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name+".cd-status.json"), append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// agrees asserts status.migratedValues as STORED names exactly the instances and fields the stored
	// condition's message names: the expected entries are given by the test, not derived from either.
	agrees := func(t *testing.T, back *compositiondefinitionsv1alpha1.CompositionDefinition, version string, refused map[string][]string, unchecked []string) {
		t.Helper()
		mv := back.Status.MigratedValues
		if mv == nil {
			t.Fatal("status.migratedValues is absent in the stored status")
		}
		if mv.Version != version || mv.RefusedCount != len(refused) || mv.UncheckedCount != len(unchecked) || len(mv.Refused) != len(refused) || len(mv.Unchecked) != len(unchecked) {
			t.Fatalf("stored migratedValues %+v, want version %s, refused %v, unchecked %v", mv, version, refused, unchecked)
		}
		msg := back.GetCondition(cdreason.TypeMigratedValuesValid).Message
		for _, r := range mv.Refused {
			want, ok := refused[r.Namespace+"/"+r.Name]
			if !ok || strings.Join(r.Fields, ",") != strings.Join(want, ",") || r.FieldCount != len(want) {
				t.Fatalf("stored refused entry %+v, want one of %v", r, refused)
			}
			if !strings.Contains(msg, r.Namespace+"/"+r.Name+" ("+strings.Join(want, ", ")+")") {
				t.Fatalf("the stored message %q does not name %s/%s with %v", msg, r.Namespace, r.Name, want)
			}
		}
		for i, u := range mv.Unchecked {
			if u.Namespace+"/"+u.Name != unchecked[i] || !strings.Contains(msg, unchecked[i]) {
				t.Fatalf("stored unchecked entry %+v, want %s named in %q", u, unchecked[i], msg)
			}
		}
	}
	readyUnchanged := func(t *testing.T, cd *compositiondefinitionsv1alpha1.CompositionDefinition) {
		t.Helper()
		if got := cd.GetCondition(rtv1.TypeReady); !reflect.DeepEqual(got, ready) {
			t.Fatalf("Ready changed: got %+v, want %+v", got, ready)
		}
	}
	// storedReadyUnchanged is readyUnchanged for a definition read back from the apiserver, which keeps
	// lastTransitionTime to the second: the comparison is on what Ready says, not its timestamp.
	storedReadyUnchanged := func(t *testing.T, cd *compositiondefinitionsv1alpha1.CompositionDefinition) {
		t.Helper()
		got := cd.GetCondition(rtv1.TypeReady)
		if got.Status != ready.Status || got.Reason != ready.Reason || got.Message != ready.Message {
			t.Fatalf("stored Ready changed: got %+v, want %+v", got, ready)
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
		if cd.Status.MigratedValues != nil {
			t.Fatalf("nothing migrated, yet status.migratedValues = %+v", cd.Status.MigratedValues)
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
		back := persist(t, cd)
		agrees(t, back, "v2-0-0", map[string][]string{"default/held": {"spec.features.enabled"}}, nil)
		storedReadyUnchanged(t, back)
		sample(t, "values-refused-by-new-version", back)
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
		back := persist(t, cd)
		if back.Status.MigratedValues != nil {
			t.Fatalf("True must clear status.migratedValues; stored %+v", back.Status.MigratedValues)
		}
		sample(t, "all-valid", back)
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

	// ValidationIncomplete through a REAL path: a ValidatingAdmissionPolicy denies the dry-run create
	// with no spec field in its causes (an admission policy's denial, the case validate.go names), so
	// the instance's values cannot be judged. Installed last so it cannot affect the cases above, and
	// it matches CREATE only, so the migration's UPDATE is admitted.
	t.Run("a dry run an admission policy denies is unchecked, never valid", func(t *testing.T) {
		mk("blocked", "def-614-blocked", map[string]any{"name": "blocked"})
		policy := &admissionregistrationv1.ValidatingAdmissionPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "widget614-blocked"},
			Spec: admissionregistrationv1.ValidatingAdmissionPolicySpec{
				MatchConstraints: &admissionregistrationv1.MatchResources{ResourceRules: []admissionregistrationv1.NamedRuleWithOperations{{
					RuleWithOperations: admissionregistrationv1.RuleWithOperations{
						Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Create},
						Rule:       admissionregistrationv1.Rule{APIGroups: []string{group}, APIVersions: []string{"*"}, Resources: []string{gvr1.Resource}},
					},
				}}},
				Validations: []admissionregistrationv1.Validation{{
					Expression: "!has(object.spec.name) || object.spec.name != 'blocked'",
					Message:    "creating a blocked widget is refused by policy",
				}},
			},
		}
		deny := []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny}
		binding := &admissionregistrationv1.ValidatingAdmissionPolicyBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "widget614-blocked"},
			Spec:       admissionregistrationv1.ValidatingAdmissionPolicyBindingSpec{PolicyName: policy.Name, ValidationActions: deny},
		}
		if err := cl.Create(ctx, policy); err != nil {
			t.Fatal(err)
		}
		if err := cl.Create(ctx, binding); err != nil {
			t.Fatal(err)
		}
		// The policy takes effect asynchronously. Wait until it denies a probe dry-run create of the
		// instance's spec BEFORE migrating: a check that ran earlier would read True, and True is final
		// until the next migration (by design), so the recheck would rightly never run again.
		u, err := dyn.Resource(gvr1).Namespace("default").Get(ctx, "blocked", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		probe := &unstructured.Unstructured{Object: map[string]any{"apiVersion": group + "/v2-0-0", "kind": kind,
			"metadata": map[string]any{"generateName": "krateo-validate-", "namespace": "default"}, "spec": u.Object["spec"]}}
		var denial error
		if err := wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
			_, denial = dyn.Resource(gvr2).Namespace("default").Create(ctx, probe.DeepCopy(), metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
			return denial != nil, nil
		}); err != nil {
			t.Fatal("the admission policy never took effect")
		}
		// PREMISE: the denial names no spec field, so it is not a verdict on the values.
		if !strings.Contains(denial.Error(), "ValidatingAdmissionPolicy") {
			t.Fatalf("the probe was refused for another reason: %v", denial)
		}
		blocked := newCD("def-614-blocked", "v1-0-0", "v2-0-0")
		if err := e.migrateAndCheck(ctx, blocked, gvr2, gvk2, log); err != nil {
			t.Fatalf("the migration must not be refused by a CREATE-only policy: %v", err)
		}
		if !cdreason.MigratedValuesUnchecked.Is(blocked.GetCondition(cdreason.TypeMigratedValuesValid)) {
			t.Fatalf("a dry run the policy denies: %+v, want Unknown/ValidationIncomplete", blocked.GetCondition(cdreason.TypeMigratedValuesValid))
		}
		c := blocked.GetCondition(cdreason.TypeMigratedValuesValid)
		if strings.Contains(c.Message, "refused by policy") {
			t.Fatalf("the message must not carry the denial's text: %q", c.Message)
		}
		readyUnchanged(t, blocked)
		back := persist(t, blocked)
		agrees(t, back, "v2-0-0", nil, []string{"default/blocked"})
		sample(t, "validation-incomplete", back)
	})
}

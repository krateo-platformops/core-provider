package policy

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	// Register the policy + binding GVKs as unstructured so the fake client can track them.
	for _, kind := range []string{"MutatingAdmissionPolicy", "MutatingAdmissionPolicyBinding"} {
		gvk := schema.GroupVersionKind{Group: "admissionregistration.k8s.io", Version: "v1", Kind: kind}
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(gvk)
		s.AddKnownTypeWithName(gvk, u)
		lst := &unstructured.UnstructuredList{}
		s.AddKnownTypeWithName(gvk.GroupVersion().WithKind(kind+"List"), lst)
	}
	return s
}

func get(t *testing.T, c client.Client, kind, name string) *unstructured.Unstructured {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: "admissionregistration.k8s.io", Version: "v1", Kind: kind})
	if err := c.Get(context.Background(), client.ObjectKey{Name: name}, u); err != nil {
		t.Fatalf("get %s/%s: %v", kind, name, err)
	}
	return u
}

func TestEnsureCreatesPolicyAndBinding(t *testing.T) {
	c := fakeclient.NewClientBuilder().WithScheme(newScheme()).Build()

	if err := EnsureCompositionVersionPolicy(context.Background(), c); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	p := get(t, c, "MutatingAdmissionPolicy", PolicyName)
	b := get(t, c, "MutatingAdmissionPolicyBinding", BindingName)

	// Binding references the policy by name.
	if got, _, _ := unstructured.NestedString(b.Object, "spec", "policyName"); got != PolicyName {
		t.Errorf("binding policyName = %q, want %q", got, PolicyName)
	}

	// The mutation stamps the served version onto the composition-version label.
	muts, _, _ := unstructured.NestedSlice(p.Object, "spec", "mutations")
	if len(muts) != 1 {
		t.Fatalf("expected 1 mutation, got %d", len(muts))
	}
	mut := muts[0].(map[string]any)

	// #66: the patch type is load-bearing. ApplyConfiguration performs a structured merge, which
	// converts the WHOLE object to its typed form first — so an unrelated field whose schema types
	// it numeric while it holds a Quantity string ("200m") makes the conversion fail, and with
	// failurePolicy: Fail the apply is DENIED. One such composition wedged an entire umbrella
	// reconcile. A JSON Patch touches only the path it names.
	if got, _, _ := unstructured.NestedString(mut, "patchType"); got != "JSONPatch" {
		t.Errorf("patchType = %q, want JSONPatch: ApplyConfiguration forces a whole-object typed "+
			"conversion that denies valid Quantity strings (#66)", got)
	}
	if _, found, _ := unstructured.NestedString(mut, "applyConfiguration", "expression"); found {
		t.Error("applyConfiguration must not be set: it reintroduces the whole-object conversion (#66)")
	}

	cel, _, _ := unstructured.NestedString(mut, "jsonPatch", "expression")
	if !strings.Contains(cel, versionLabel) || !strings.Contains(cel, "request.requestKind.version") {
		t.Errorf("mutation expression does not stamp %s from served version: %q", versionLabel, cel)
	}
	// An object with no labels at all needs the map created in one step — a JSON Patch `add` to
	// /metadata/labels/<key> fails when /metadata/labels is absent. Without both branches the
	// policy denies every composition that happens to carry no labels.
	if !strings.Contains(cel, "has(object.metadata.labels)") {
		t.Errorf("expression must handle an object with no labels, or it denies label-less "+
			"compositions: %q", cel)
	}
	// The label key contains "/", which RFC 6901 requires escaped as ~1 inside a JSON Pointer.
	// escapeKey does it; hand-written escaping breaks silently the next time the label is renamed.
	if !strings.Contains(cel, "jsonpatch.escapeKey") {
		t.Errorf("the label key must be escaped for the JSON Pointer path: %q", cel)
	}

	// Matches the composition API group, all resources, on CREATE/UPDATE.
	rules, _, _ := unstructured.NestedSlice(p.Object, "spec", "matchConstraints", "resourceRules")
	if len(rules) != 1 {
		t.Fatalf("expected 1 resource rule, got %d", len(rules))
	}
	groups, _, _ := unstructured.NestedStringSlice(rules[0].(map[string]any), "apiGroups")
	if len(groups) != 1 || groups[0] != compositionGroup {
		t.Errorf("apiGroups = %v, want [%s]", groups, compositionGroup)
	}
}

func TestEnsureIsIdempotent(t *testing.T) {
	c := fakeclient.NewClientBuilder().WithScheme(newScheme()).Build()

	if err := EnsureCompositionVersionPolicy(context.Background(), c); err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	// A second call must not error on AlreadyExists.
	if err := EnsureCompositionVersionPolicy(context.Background(), c); err != nil {
		t.Fatalf("second ensure (idempotent) failed: %v", err)
	}
}

package policy

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// legacyPolicy builds the policy exactly as this package shipped it before #66.
func legacyPolicy() *unstructured.Unstructured {
	p := &unstructured.Unstructured{}
	p.SetAPIVersion(policyAPIVersion)
	p.SetKind("MutatingAdmissionPolicy")
	p.SetName(PolicyName)
	p.Object["spec"] = map[string]any{
		"mutations": []any{map[string]any{
			"patchType": "ApplyConfiguration",
			"applyConfiguration": map[string]any{
				"expression": legacyApplyConfigurationExpression(),
			},
		}},
	}
	return p
}

func clientKey(name string) client.ObjectKey { return client.ObjectKey{Name: name} }

func readMutation(t *testing.T, obj *unstructured.Unstructured) map[string]any {
	t.Helper()
	muts, _, _ := unstructured.NestedSlice(obj.Object, "spec", "mutations")
	if len(muts) != 1 {
		t.Fatalf("expected 1 mutation, got %d", len(muts))
	}
	return muts[0].(map[string]any)
}

// Without this migration the #66 fix reaches nobody.
//
// EnsureCompositionVersionPolicy is create-if-absent, and every cluster that has ever run
// core-provider already HAS the policy — so IsAlreadyExists was swallowed and the broken
// ApplyConfiguration mutation stayed forever. Confirmed on krateo-057: the live policy still
// carried the legacy form, so a Quantity-string cpu would have been denied on a cluster running
// the "fixed" version.
func TestLegacyPolicyIsMigratedToJSONPatch(t *testing.T) {
	ctx := context.Background()
	c := fakeclient.NewClientBuilder().WithScheme(newScheme()).WithObjects(legacyPolicy()).Build()

	if err := EnsureCompositionVersionPolicy(ctx, c); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	got := &unstructured.Unstructured{}
	got.SetAPIVersion(policyAPIVersion)
	got.SetKind("MutatingAdmissionPolicy")
	if err := c.Get(ctx, clientKey(PolicyName), got); err != nil {
		t.Fatalf("get: %v", err)
	}

	m := readMutation(t, got)
	if pt, _, _ := unstructured.NestedString(m, "patchType"); pt != "JSONPatch" {
		t.Errorf("legacy policy was not migrated: patchType = %q, want JSONPatch. The #66 fix is inert "+
			"on every existing cluster without this.", pt)
	}
	if _, found, _ := unstructured.NestedString(m, "applyConfiguration", "expression"); found {
		t.Error("the legacy applyConfiguration mutation is still present after migration")
	}
}

// The migration must be surgical. It rewrites only what it can prove is our own previous output;
// anything a chart or an operator owns is left alone, preserving the original intent of never
// fighting another field manager.
func TestMigrationLeavesForeignPoliciesAlone(t *testing.T) {
	cases := []struct {
		name string
		spec map[string]any
	}{
		{
			// Someone customised the expression — their intent, not ours to overwrite.
			name: "different expression",
			spec: map[string]any{"mutations": []any{map[string]any{
				"patchType":          "ApplyConfiguration",
				"applyConfiguration": map[string]any{"expression": `Object{ metadata: Object.metadata{ labels: {"custom": "x"} } }`},
			}}},
		},
		{
			// More than one mutation means this policy is doing something we did not ship.
			name: "extra mutations",
			spec: map[string]any{"mutations": []any{
				map[string]any{"patchType": "ApplyConfiguration", "applyConfiguration": map[string]any{"expression": legacyApplyConfigurationExpression()}},
				map[string]any{"patchType": "JSONPatch", "jsonPatch": map[string]any{"expression": "[]"}},
			}},
		},
		{
			name: "no mutations at all",
			spec: map[string]any{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := legacyPolicy()
			p.Object["spec"] = tc.spec
			before, _, _ := unstructured.NestedSlice(p.Object, "spec", "mutations")

			ctx := context.Background()
			c := fakeclient.NewClientBuilder().WithScheme(newScheme()).WithObjects(p.DeepCopy()).Build()
			if err := EnsureCompositionVersionPolicy(ctx, c); err != nil {
				t.Fatalf("ensure: %v", err)
			}

			got := &unstructured.Unstructured{}
			got.SetAPIVersion(policyAPIVersion)
			got.SetKind("MutatingAdmissionPolicy")
			if err := c.Get(ctx, clientKey(PolicyName), got); err != nil {
				t.Fatalf("get: %v", err)
			}
			after, _, _ := unstructured.NestedSlice(got.Object, "spec", "mutations")

			if len(before) != len(after) {
				t.Fatalf("mutation count changed: %d -> %d", len(before), len(after))
			}
			for i := range before {
				b := before[i].(map[string]any)
				a := after[i].(map[string]any)
				bp, _, _ := unstructured.NestedString(b, "patchType")
				ap, _, _ := unstructured.NestedString(a, "patchType")
				if bp != ap {
					t.Errorf("mutation %d was rewritten (%q -> %q); a policy we did not ship must be left alone", i, bp, ap)
				}
			}
		})
	}
}

// Running twice must not thrash the object: the second pass sees JSONPatch, not the legacy form,
// and does nothing.
func TestMigrationIsIdempotent(t *testing.T) {
	ctx := context.Background()
	c := fakeclient.NewClientBuilder().WithScheme(newScheme()).WithObjects(legacyPolicy()).Build()

	for i := 0; i < 3; i++ {
		if err := EnsureCompositionVersionPolicy(ctx, c); err != nil {
			t.Fatalf("ensure pass %d: %v", i, err)
		}
	}

	got := &unstructured.Unstructured{}
	got.SetAPIVersion(policyAPIVersion)
	got.SetKind("MutatingAdmissionPolicy")
	if err := c.Get(ctx, clientKey(PolicyName), got); err != nil {
		t.Fatalf("get: %v", err)
	}
	m := readMutation(t, got)
	if pt, _, _ := unstructured.NestedString(m, "patchType"); pt != "JSONPatch" {
		t.Errorf("after repeated passes patchType = %q, want JSONPatch", pt)
	}
}

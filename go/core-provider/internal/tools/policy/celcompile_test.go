package policy

import (
	"strings"
	"testing"

	"github.com/google/cel-go/cel"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apiserver/pkg/cel/common"
	"k8s.io/apiserver/pkg/cel/library"
	"k8s.io/apiserver/pkg/cel/mutation"
)

// The mutation expression must COMPILE, not merely contain the right substrings.
//
// The other tests in this package assert on substrings, which is the weaker check: a CEL expression
// with a typo, an unbalanced brace, or a function that does not exist passes every substring
// assertion and is then rejected by the API server when the policy is created. That failure is
// worse than the bug this expression fixes — the policy would not exist at all, so no composition
// would get its version label and per-version migration would silently stop working.
//
// Compiling here catches that offline. It does not prove the expression is SEMANTICALLY right
// against a live apiserver — the e2e test in internal/tools/crd does that against a real cluster —
// but it does prove the thing a string match cannot: that this is valid CEL, referring only to
// functions and types that exist.
func TestMutationExpressionCompiles(t *testing.T) {
	p, _ := objects()

	muts, _, _ := unstructured.NestedSlice(p.Object, "spec", "mutations")
	if len(muts) != 1 {
		t.Fatalf("expected 1 mutation, got %d", len(muts))
	}
	expr, found, _ := unstructured.NestedString(muts[0].(map[string]any), "jsonPatch", "expression")
	if !found || expr == "" {
		t.Fatal("no jsonPatch expression to compile")
	}

	env, err := cel.NewEnv(
		// jsonpatch.escapeKey — the RFC 6901 escaper the label path depends on.
		library.JSONPatch(),
		// Resolves the JSONPatch{} and Object{} types the expression constructs. This is the same
		// option the real compiler uses (pkg/admission/plugin/cel/compile.go), so the type surface
		// here matches what the API server will compile against.
		common.ResolverEnvOption(&mutation.DynamicTypeResolver{}),
		// What a mutating policy expression is evaluated against.
		cel.Variable("object", cel.DynType),
		cel.Variable("request", cel.DynType),
	)
	if err != nil {
		t.Fatalf("building the CEL environment: %v", err)
	}

	if _, issues := env.Compile(expr); issues != nil && issues.Err() != nil {
		t.Fatalf("the mutation expression does not compile — the API server would reject the policy, "+
			"leaving NO policy and no version labels at all:\n  expression: %s\n  error: %v", expr, issues.Err())
	}
}

// A guard against the expression being reduced to one branch later.
//
// JSON Patch `add` to /metadata/labels/<key> fails when /metadata/labels does not exist, so both
// branches are required. Someone simplifying this to the single-branch form would produce an
// expression that still compiles, still contains every substring the other tests check, and denies
// every composition that happens to carry no labels — with failurePolicy: Fail, that is an outage.
func TestMutationExpressionKeepsBothBranches(t *testing.T) {
	p, _ := objects()
	muts, _, _ := unstructured.NestedSlice(p.Object, "spec", "mutations")
	expr, _, _ := unstructured.NestedString(muts[0].(map[string]any), "jsonPatch", "expression")

	if !strings.Contains(expr, `path: "/metadata/labels"`) {
		t.Errorf("missing the branch that CREATES the labels map; a label-less composition would be "+
			"denied: %s", expr)
	}
	if !strings.Contains(expr, `path: "/metadata/labels/"`) {
		t.Errorf("missing the branch that adds to an EXISTING labels map: %s", expr)
	}
}

package composition

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// #304. The self-heal log must carry intendedDeletions BESIDE deleted, because a bare `deleted:0`
// cannot distinguish two different defects in two different repositories:
//
//	intended 0, deleted 0   nothing to remove, or the removal was lost BEFORE this merge
//	intended >0, deleted 0  helm was asked and did not do it
//
// Asserted over the AST because the claim is structural — that these keys are present on THIS log
// call — and because there is no behavioural seam here to test: the values come straight off a
// plumbing result and the only way to get them wrong is to not log them. A unit test would have to
// fabricate a ReconcileResult and assert that the fabricated numbers came back, which asserts
// nothing about whether the real call site reads the real field.
//
// This is the same reasoning as driftreturn_order_test.go and releasename_phases_test.go: when the
// defect is "the code does not mention X", a source assertion is the honest instrument, and its
// known weakness is that it cannot prove reachability. Reachability is covered separately — the
// call site is inside the self-heal branch that the ironic session measured being entered on a
// fresh composition's first pass, count 1.
func TestSelfHealLogCarriesTheIntendedDeletionsDifferential(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "composition.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing composition.go: %v", err)
	}

	const marker = "Composition drift detected and self-healed"

	// Required keys. `deleted` and `intendedDeletions` are the pair; `deletionsSkipped` is the
	// precomputed discriminator so a reader does not have to do the comparison themselves — the
	// log is read by people chasing an incident, and a derived boolean they can grep for is worth
	// more than two numbers they must remember to compare.
	required := []string{"deleted", "intendedDeletions", "deletionsSkipped"}

	var found bool
	var keys []string

	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING || !strings.Contains(lit.Value, marker) {
			return true
		}
		found = true
		// Structured logging: alternating key/value from arg 1 onward. Only the string-literal
		// keys are collected; a non-literal key would be unreadable to this test and is also
		// unreadable to a person grepping the logs.
		for i := 1; i < len(call.Args); i += 2 {
			if k, ok := call.Args[i].(*ast.BasicLit); ok && k.Kind == token.STRING {
				keys = append(keys, strings.Trim(k.Value, `"`))
			}
		}
		return false
	})

	// THE DENOMINATOR. If the log message is reworded this test must FAIL rather than silently
	// assert nothing — a source guard whose anchor stops matching is the failure mode that makes
	// these tests worthless, and it has cost me a false pass in this package before.
	if !found {
		t.Fatalf("no log call with the message %q found in composition.go. Either the self-heal "+
			"log was reworded or removed; this test cannot assert anything about a call it cannot "+
			"find, so it fails rather than passing over nothing", marker)
	}
	if len(keys) == 0 {
		t.Fatal("the self-heal log call was found but has no string-literal keys, so the key " +
			"assertions below would pass over an empty set")
	}

	have := map[string]bool{}
	for _, k := range keys {
		have[k] = true
	}
	for _, r := range required {
		if !have[r] {
			t.Errorf("the self-heal log does not carry %q (keys present: %v). Without the "+
				"intendedDeletions pair, `deleted:0` is indistinguishable between 'nothing was "+
				"removed from the render' and 'the merge was asked to remove something and did "+
				"not' — which is why #304 has three candidate mechanisms and no way to choose",
				r, keys)
		}
	}
}

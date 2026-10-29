package composition

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// stampReady's default arm used to be setAvaibleStatus. A reason added to resolveReady without a
// matching case there did not fail loudly and did not stamp nothing — it reported the composition
// HEALTHY. That is the #165 failure mode arriving through the routing that implements the fix for
// it, and it is invisible: "not in a recognised bad branch" read as evidence of good.
//
// Prompted by the portal hitting the identical shape one layer up — its cluster statusColor chain
// tested notReady, then unknown, then fell through to GREEN, so a cluster whose only non-ready row
// was converging was presented as healthy.

// An unknown reason must ERROR rather than stamp anything. The error is what makes the mistake
// visible at the moment it is made.
func TestStampReady_UnknownReasonErrorsRatherThanReportingHealthy(t *testing.T) {
	mg := &unstructured.Unstructured{Object: map[string]any{}}

	err := stampReady(mg, readyOutcome{reason: "SomeFutureReason", message: "x"}, true)
	if err == nil {
		t.Fatal("an unrecognised reason was accepted; before this change it stamped Available, " +
			"which reports a composition healthy on the strength of a reason nobody handled")
	}
	if !strings.Contains(err.Error(), "SomeFutureReason") {
		t.Errorf("the error does not name the offending reason: %v", err)
	}
	// And it must not have written a condition on the way out — erroring AND stamping would be
	// worse than either, because the object would carry a verdict the caller believes failed.
	if conds, found, _ := unstructured.NestedSlice(mg.Object, "status", "conditions"); found && len(conds) > 0 {
		t.Errorf("a condition was stamped despite the error: %v", conds)
	}
}

// The four real reasons must still route correctly — the control arm. A change that made
// stampReady reject everything would satisfy the test above on its own.
func TestStampReady_RoutesEveryRealReason(t *testing.T) {
	for _, c := range []struct{ reason, wantReason, wantStatus string }{
		{"Available", "Available", "True"},
		{"Creating", "Creating", "False"},
		{"Unavailable", "Unavailable", "False"},
		{"ZeroEvidence", "ZeroEvidence", "Unknown"},
	} {
		t.Run(c.reason, func(t *testing.T) {
			mg := &unstructured.Unstructured{Object: map[string]any{}}
			if err := stampReady(mg, readyOutcome{reason: c.reason, message: "m"}, true); err != nil {
				t.Fatalf("stampReady(%q): %v", c.reason, err)
			}
			status, reason, _ := readyCondition(t, mg)
			if reason != c.wantReason || status != c.wantStatus {
				t.Errorf("got %s/%s, want %s/%s", status, reason, c.wantStatus, c.wantReason)
			}
		})
	}
}

// COMPLETENESS, and the half the two tests above cannot cover: they assert that the reasons I
// thought of are handled. This asserts that every reason resolveReady can actually PRODUCE is one
// of them — so adding a `readyOutcome{reason: "Something"}` to childhealth.go without a case in
// stampReady fails here rather than in production.
//
// Source-level because the property is about the set of reasons the code can emit, which is not
// observable by calling resolveReady without enumerating every input that reaches each return.
// Unlike a guard that asserts an identifier is mentioned, this compares two sets and fails on a
// difference in either direction, so it cannot pass by accident.
func TestEveryReasonResolveReadyProducesIsHandledByStampReady(t *testing.T) {
	fset := token.NewFileSet()

	produced := map[string]bool{}
	ch, err := parser.ParseFile(fset, "childhealth.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing childhealth.go: %v", err)
	}
	ast.Inspect(ch, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		if id, ok := lit.Type.(*ast.Ident); !ok || id.Name != "readyOutcome" {
			return true
		}
		for _, el := range lit.Elts {
			kv, ok := el.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if k, ok := kv.Key.(*ast.Ident); !ok || k.Name != "reason" {
				continue
			}
			if bl, ok := kv.Value.(*ast.BasicLit); ok && bl.Kind == token.STRING {
				if s, err := strconv.Unquote(bl.Value); err == nil {
					produced[s] = true
				}
			}
		}
		return true
	})

	handled := map[string]bool{}
	su, err := parser.ParseFile(fset, "support.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing support.go: %v", err)
	}
	for _, decl := range su.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "stampReady" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			cc, ok := n.(*ast.CaseClause)
			if !ok {
				return true
			}
			for _, e := range cc.List {
				if bl, ok := e.(*ast.BasicLit); ok && bl.Kind == token.STRING {
					if s, err := strconv.Unquote(bl.Value); err == nil {
						handled[s] = true
					}
				}
			}
			return true
		})
	}

	// Both sides must be non-empty, or this test is comparing nothing with nothing and passes
	// while asserting zero. This is the control the AST guard in #232 lacked.
	if len(produced) == 0 {
		t.Fatal("found no readyOutcome{reason: \"...\"} literals in childhealth.go — the walk is " +
			"not finding what it thinks it is, so a mismatch could never be detected")
	}
	if len(handled) == 0 {
		t.Fatal("found no case literals in stampReady — same problem, other side")
	}

	for r := range produced {
		if !handled[r] {
			t.Errorf("resolveReady can return reason %q but stampReady has no case for it. Before "+
				"this was an error it fell to `default: setAvaibleStatus`, i.e. the composition "+
				"would be reported HEALTHY on a reason nobody handled.", r)
		}
	}
	// The other direction is a warning sign rather than a fault: a case for a reason nothing emits
	// is dead code, and dead routing is how #232 happened.
	for r := range handled {
		if !produced[r] {
			t.Errorf("stampReady handles reason %q that resolveReady never produces — dead routing, "+
				"which is exactly how the gracefully-paused condition stayed unreachable (#232)", r)
		}
	}
}

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
// The KEYS are now asserted behaviourally, by TestReconcileDiffFields against the one function both
// drift lines pass (reconcilediff.go). What is left for a source assertion is the wiring: that each
// drift log call actually passes reconcileDiffFields. A behavioural test cannot reach these call
// sites without a live helm release, and a call site that stops passing it would leave
// TestReconcileDiffFields green while the line went anonymous again. That was the Update line's
// state until #439: counts only.
//
// Known weakness, as for driftreturn_order_test.go: this proves MENTION, not reachability.
// Reachability of the Observe line is the ironic session's measured first-pass self-heal (count 1).
func TestDriftLogsPassTheSharedDiffFields(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "composition.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing composition.go: %v", err)
	}

	markers := []string{
		"Composition drift detected and self-healed",
		"Update wrote a helm revision; Observe's reconcile had not fully converged",
	}
	for _, marker := range markers {
		var found, passes bool
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
			for _, arg := range call.Args[1:] {
				ast.Inspect(arg, func(m ast.Node) bool {
					if c, ok := m.(*ast.CallExpr); ok {
						if id, ok := c.Fun.(*ast.Ident); ok && id.Name == "reconcileDiffFields" {
							passes = true
						}
					}
					return true
				})
			}
			return false
		})
		// THE DENOMINATOR. A reworded message must FAIL this, not let it pass over nothing.
		if !found {
			t.Errorf("no log call with the message %q in composition.go: reworded or removed, and this "+
				"test cannot assert anything about a call it cannot find", marker)
			continue
		}
		if !passes {
			t.Errorf("the log %q does not pass reconcileDiffFields(...): it no longer names which child "+
				"and which field changed, nor intendedDeletions beside deleted (#304, plumbing#112)", marker)
		}
	}
}

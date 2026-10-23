// Coverage guard for the #69 breadcrumbs: every long-running helm operation must be bracketed.
//
// helmphase_test.go proves the helper BEHAVES — both edges, phase, release, elapsed. It passes
// identically whether a given call site uses it or not, so on its own it let the two `hc.Upgrade`
// call sites ship unwrapped for the whole life of #69: install, rollback, reconcile and uninstall
// were bracketed, upgrade was not. That is the same shape as #182, where a functional test of the
// CRD watch could not see that the informer cached whole objects.
//
// Upgrade was the worst one to miss. It is the operation in steady state — every spec change on an
// existing composition reaches helm through Update — and it is the one named in helmPhase's own
// rationale: "A composition Update that performs a real helm operation ... can run for many minutes
// while the cdc logs NOTHING at INFO. Observed live: an installer controller went silent for ~27
// minutes." The fix was written, tested and documented around an operation it did not cover.
//
// Written against the AST for the reason rbac_ordering_test.go gives: driving these handlers to a
// real helm call needs a chart-inspector, a cluster and a release in a specific status. This is the
// cheap guard that fails in review, which is where an unwrapped call site gets introduced.
package composition

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// helmOps are the helm client methods that can run for minutes and must therefore be legible as
// progress. GetRelease and Close are deliberately absent: they are cheap, local and bounded, and
// bracketing them would bury the breadcrumbs that matter in noise.
var helmOps = map[string]string{
	"Install":   "a first install renders and applies the whole chart",
	"Upgrade":   "the steady-state operation — every spec change on an existing composition",
	"Uninstall": "a teardown waits on finalizer-bearing children (#108)",
	"Rollback":  "the stuck-release recovery, which runs before anything else can proceed",
	"Reconcile": "a drift reconcile re-applies every manifest in the release",
}

// calleeName is the function name of a call expression, for the forms that appear here: a plain
// identifier (helmPhase(...)), a method (helmMetrics.TimedUpgradeWithResult(...)), or an explicit
// generic instantiation (helmPhase[*Release](...)).
func calleeName(call *ast.CallExpr) string {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return fun.Name
	case *ast.SelectorExpr:
		return fun.Sel.Name
	case *ast.IndexExpr:
		if id, ok := fun.X.(*ast.Ident); ok {
			return id.Name
		}
	case *ast.IndexListExpr:
		if id, ok := fun.X.(*ast.Ident); ok {
			return id.Name
		}
	}
	return ""
}

// enclosingCallNames returns the names of every call lexically enclosing target, outermost first.
//
// ast.Inspect calls f(node), then f(nil) once its children are done — but only when f returned
// true. So pushing on the true path and popping on nil keeps the stack exactly in step with the
// walk, and the stack at the moment target is reached is its ancestor chain.
func enclosingCallNames(file *ast.File, target *ast.CallExpr) []string {
	var stack []ast.Node
	var names []string
	done := false

	ast.Inspect(file, func(n ast.Node) bool {
		if done {
			return false
		}
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		if n == target {
			for _, s := range stack {
				if call, ok := s.(*ast.CallExpr); ok {
					names = append(names, calleeName(call))
				}
			}
			done = true
			return false
		}
		stack = append(stack, n)
		return true
	})

	return names
}

// helmClientCalls finds every call of the form hc.<Op>(...) where Op is a long helm operation.
func helmClientCalls(file *ast.File) []*ast.CallExpr {
	var found []*ast.CallExpr
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		recv, ok := sel.X.(*ast.Ident)
		if !ok || recv.Name != "hc" {
			return true
		}
		if _, isOp := helmOps[sel.Sel.Name]; isOp {
			found = append(found, call)
		}
		return true
	})
	return found
}

func TestEveryLongHelmOperationIsBracketed(t *testing.T) {
	// A local FileSet, unlike parseComposition's, because this test reports line numbers.
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, compositionSrc, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parsing %s: %v", compositionSrc, err)
	}

	calls := helmClientCalls(file)
	// If the instrument finds nothing, the test proves nothing — the failure the #182 envtest also
	// guards against. Renaming the helm client variable would silently empty this list.
	if len(calls) == 0 {
		t.Fatalf("found no hc.<Install|Upgrade|Uninstall|Rollback|Reconcile> calls in %s; the helm "+
			"client variable was probably renamed, so this guard is inspecting nothing — fix the "+
			"matcher rather than deleting the test", compositionSrc)
	}

	seen := map[string]bool{}
	for _, call := range calls {
		op := call.Fun.(*ast.SelectorExpr).Sel.Name
		seen[op] = true

		bracketed := false
		for _, name := range enclosingCallNames(file, call) {
			if name == "helmPhase" || name == "helmPhaseErr" {
				bracketed = true
				break
			}
		}
		if !bracketed {
			t.Errorf("%s:%d: hc.%s is not wrapped in helmPhase — %s, so while it runs the cdc logs "+
				"nothing at INFO and a working controller is indistinguishable from a stalled one (#69). "+
				"Wrap it: helmPhase(log, %q, releaseName, func() (...) { ... })",
				compositionSrc, fset.Position(call.Pos()).Line, op, helmOps[op], strings.ToLower(op))
		}
	}

	// Name the operations this guard did NOT observe, so that a call site added elsewhere cannot be
	// the missing one while the test passes on the others.
	for op := range helmOps {
		if !seen[op] {
			t.Logf("note: no hc.%s call site in %s — nothing to check for it", op, compositionSrc)
		}
	}
}

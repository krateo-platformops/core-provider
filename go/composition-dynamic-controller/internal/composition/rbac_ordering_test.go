// Ordering guard for the derived-RBAC apply, pinned at the source level.
//
// What this protects is an ORDER, not a value, and an order is exactly the kind of property that
// survives every unit test while being silently reversed by an ordinary refactor. The bug it guards
// against (core-provider#167) was not a wrong call or a missing one -- every piece was present and
// correct, just sequenced after the operation that depended on it.
//
// The dependency: both handlers drive helm under the composition's OWN ServiceAccount, and
// applyDerivedRBAC is the only thing that re-subjects that ServiceAccount's bindings (see its doc
// comment, and core-provider#166 for how they come to be wrong). So a stuck-release recovery that
// runs FIRST does its rollback with whatever grants happen to be in place -- and when those are
// insufficient it fails, returns, and the apply that would have fixed them is never reached. The
// composition then cannot recover by any route it has. On krateo-057 that loop ran for 22 hours.
//
// Delete carries the live case and Observe the latent one, because Observe returns early for any
// composition with a deletionTimestamp (asserted below, since that early return is the reason Delete
// needs its own apply rather than inheriting Observe's).
//
// Written against the AST rather than by running a reconcile: driving Observe/Delete to the point of
// the recovery needs a helm release wedged in a pending status, a chart-inspector, and a cluster. A
// behavioural test of that is worth having and is not this -- this is the cheap guard that fails in
// review, which is where a reordering gets introduced.
package composition

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

const compositionSrc = "composition.go"

// findFunc returns the named method declaration on *handler.
func findFunc(t *testing.T, file *ast.File, name string) *ast.FuncDecl {
	t.Helper()
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != name || fn.Recv == nil {
			continue
		}
		return fn
	}
	t.Fatalf("no method %q found in %s", name, compositionSrc)
	return nil
}

// posOf walks fn and returns the offset of the first node satisfying match, or -1.
// Offsets are comparable because every node here comes from the same parsed file.
func posOf(fn *ast.FuncDecl, match func(ast.Node) bool) token.Pos {
	found := token.NoPos
	ast.Inspect(fn, func(n ast.Node) bool {
		if found != token.NoPos || n == nil {
			return false
		}
		if match(n) {
			found = n.Pos()
			return false
		}
		return true
	})
	return found
}

// callTo matches a call to a plain function, e.g. isIncompleteHelmOperation(...).
func callTo(name string) func(ast.Node) bool {
	return func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return false
		}
		id, ok := call.Fun.(*ast.Ident)
		return ok && id.Name == name
	}
}

// methodCallTo matches a call to a method, e.g. h.applyDerivedRBAC(...).
func methodCallTo(name string) func(ast.Node) bool {
	return func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return false
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		return ok && sel.Sel.Name == name
	}
}

// selectorNamed matches a qualified identifier, e.g. helmconfig.StatusUninstalling.
func selectorNamed(name string) func(ast.Node) bool {
	return func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		return ok && sel.Sel.Name == name
	}
}

func parseComposition(t *testing.T) *ast.File {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), compositionSrc, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parsing %s: %v", compositionSrc, err)
	}
	return file
}

func TestRBACApplyPrecedesStuckReleaseRecoveryInObserve(t *testing.T) {
	fn := findFunc(t, parseComposition(t), "Observe")

	rbacAt := posOf(fn, methodCallTo("applyDerivedRBAC"))
	if rbacAt == token.NoPos {
		t.Fatal("Observe no longer calls applyDerivedRBAC: the derived grants are never refreshed, " +
			"so a binding left on a replaced ServiceAccount (#166) can never be repaired")
	}

	recoveryAt := posOf(fn, callTo("isIncompleteHelmOperation"))
	if recoveryAt == token.NoPos {
		t.Fatal("Observe no longer checks isIncompleteHelmOperation; if the stuck-release recovery " +
			"moved, this guard must move with it rather than be deleted")
	}

	if rbacAt > recoveryAt {
		t.Error("applyDerivedRBAC runs AFTER the stuck-release recovery in Observe (#167). " +
			"The recovery's Rollback needs the grants this call installs, so a composition wedged " +
			"with a stale binding returns before reaching the only code that could fix it.")
	}
}

func TestRBACApplyPrecedesStuckUninstallRecoveryInDelete(t *testing.T) {
	fn := findFunc(t, parseComposition(t), "Delete")

	rbacAt := posOf(fn, methodCallTo("applyDerivedRBAC"))
	if rbacAt == token.NoPos {
		t.Fatal("Delete no longer calls applyDerivedRBAC. Observe returns early for anything with a " +
			"deletionTimestamp, so Delete is the ONLY handler that can repair a deleting " +
			"composition's grants — without this call the rollback/uninstall loop of #167 returns")
	}

	recoveryAt := posOf(fn, selectorNamed("StatusUninstalling"))
	if recoveryAt == token.NoPos {
		t.Fatal("Delete no longer references StatusUninstalling; if the stuck-uninstall recovery " +
			"moved, this guard must move with it rather than be deleted")
	}

	if rbacAt > recoveryAt {
		t.Error("applyDerivedRBAC runs AFTER the stuck-uninstall recovery in Delete (#167). " +
			"Both the recovery's Rollback and the Uninstall below it need the chart's own kinds; " +
			"granting afterwards is granting on a path that already returned.")
	}
}

// The Observe early return is why Delete needs its own apply. If it is ever removed, Observe starts
// handling deleting compositions and the Delete-side apply stops being the only route -- a change
// worth making deliberately, with this comment read first.
func TestObserveStillReturnsEarlyForDeletingCompositions(t *testing.T) {
	fn := findFunc(t, parseComposition(t), "Observe")

	at := posOf(fn, methodCallTo("GetDeletionTimestamp"))
	if at == token.NoPos {
		t.Fatal("Observe no longer inspects GetDeletionTimestamp: re-check whether Delete still " +
			"needs its own applyDerivedRBAC call, and update #167's reasoning either way")
	}

	rbacAt := posOf(fn, methodCallTo("applyDerivedRBAC"))
	if rbacAt != token.NoPos && at > rbacAt {
		t.Error("the deletionTimestamp check now runs after applyDerivedRBAC in Observe; that is " +
			"not wrong in itself, but it changes which handler covers a deleting composition")
	}
}

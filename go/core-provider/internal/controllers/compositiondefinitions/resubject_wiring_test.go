// Wiring guard for the derived-RBAC re-subjecting, pinned at the source level.
//
// What this protects is a PLACEMENT, and a placement is the kind of property every unit test agrees
// with while an ordinary refactor moves it. ResubjectDerivedRBAC is correct and well tested on its
// own; it is only a fix for #166 if it runs in the one place where the controller identity is
// actually retired.
//
// Both halves of that matter, and they fail in opposite directions:
//
//   - Hoisted OUT of the retirement branch, it would run on reconciles where the old version is
//     still being served — because the UpgradePolicy defers migration, or another
//     CompositionDefinition still references that version. The old ServiceAccount is then a live
//     identity with live instances, and moving its bindings takes permissions away from a
//     controller still using them.
//   - Dropped, or placed before the Undeploy, and the bindings go on naming a ServiceAccount that
//     no longer exists. RBAC is name-based, so that grants nothing and says nothing: the symptom is
//     "could not get information about the resource: <kind>" during a rollback, which reads as a
//     missing CRD. Two engineers lost an hour each to that reading on krateo-057.
//
// Written against the AST rather than by driving a reconcile: reaching the retirement branch needs a
// CompositionDefinition mid-version-bump, a chart, a discovery client and a cluster. The behavioural
// test of that is the e2e suite; this is the cheap guard that fails in review, which is where a
// reordering is introduced. Mirrors composition-dynamic-controller's rbac_ordering_test.go, written
// for the sibling defect (#167).
package compositiondefinitions

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

const controllerSrc = "compositiondefinitions.go"

// deployCallTo matches a call into the deploy package, e.g. deploy.Undeploy(...).
func deployCallTo(name string) func(ast.Node) bool {
	return func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return false
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != name {
			return false
		}
		pkg, ok := sel.X.(*ast.Ident)
		return ok && pkg.Name == "deploy"
	}
}

// innermostBlockContaining returns the narrowest BlockStmt inside fn that encloses a node
// satisfying match, together with that node's position. Narrowest is what makes the guard
// meaningful: every ancestor block also "contains" the call, so only the innermost one identifies
// the branch it is guarded by.
//
// Selected by source range rather than by tracking a stack during the walk: ast.Inspect signals the
// end of EVERY node with a nil callback, not only of blocks, so a push/pop stack silently
// mis-nests. Ranges cannot.
func innermostBlockContaining(fn *ast.FuncDecl, match func(ast.Node) bool) (*ast.BlockStmt, token.Pos) {
	target := token.NoPos
	ast.Inspect(fn, func(n ast.Node) bool {
		if n == nil || target != token.NoPos {
			return false
		}
		if match(n) {
			target = n.Pos()
			return false
		}
		return true
	})
	if target == token.NoPos {
		return nil, token.NoPos
	}

	var narrowest *ast.BlockStmt
	ast.Inspect(fn, func(n ast.Node) bool {
		block, ok := n.(*ast.BlockStmt)
		if !ok {
			return true
		}
		if block.Pos() > target || block.End() < target {
			return true
		}
		if narrowest == nil || block.Pos() > narrowest.Pos() {
			narrowest = block
		}
		return true
	})
	return narrowest, target
}

func parseController(t *testing.T) *ast.File {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), controllerSrc, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parsing %s: %v", controllerSrc, err)
	}
	return file
}

func findMethod(t *testing.T, file *ast.File, name string) *ast.FuncDecl {
	t.Helper()
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Recv != nil && fn.Name.Name == name {
			return fn
		}
	}
	t.Fatalf("no method %q found in %s", name, controllerSrc)
	return nil
}

func TestResubjectRunsInTheRetirementBranch(t *testing.T) {
	fn := findMethod(t, parseController(t), "Update")

	undeployBlock, undeployAt := innermostBlockContaining(fn, deployCallTo("Undeploy"))
	if undeployAt == token.NoPos {
		t.Fatal("Update no longer calls deploy.Undeploy; if the old controller's retirement moved, " +
			"this guard must move with it rather than be deleted")
	}

	resubjectBlock, resubjectAt := innermostBlockContaining(fn, deployCallTo("ResubjectDerivedRBAC"))
	if resubjectAt == token.NoPos {
		t.Fatal("Update no longer calls deploy.ResubjectDerivedRBAC (#166). The derived " +
			"`<release>-krateo-rbac` bindings are release-scoped and survive a version bump; the " +
			"ServiceAccount they name is version-scoped and does not. Without this call nothing " +
			"repoints them at retirement, and a composition whose CR is absent — withdrawn by an " +
			"umbrella dependency gate, say — has no Observe to repair it either.")
	}

	if resubjectBlock != undeployBlock {
		t.Error("deploy.ResubjectDerivedRBAC is no longer in the same branch as deploy.Undeploy (#166). " +
			"It must run exactly where the old controller is retired: on a reconcile that leaves the " +
			"old version served — migration deferred by UpgradePolicy, or another CompositionDefinition " +
			"still referencing it — the old ServiceAccount is a live identity, and re-subjecting its " +
			"bindings takes permissions from a controller that is still using them.")
	}

	if resubjectAt < undeployAt {
		t.Error("deploy.ResubjectDerivedRBAC runs BEFORE deploy.Undeploy. Undeploy deletes the old " +
			"version's own baseline bindings; re-subjecting first rewrites objects that are about to " +
			"be removed, and leaves the repair unordered with respect to the deletion that makes it " +
			"necessary.")
	}
}

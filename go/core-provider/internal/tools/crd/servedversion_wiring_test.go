package crd

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// WIRING. servedversion_test.go covers the DECISION -- what ServedVersionDisagreement answers for
// each input state, and that IsReady is satisfied in the two states it rejects. Neither says
// anything about which predicate the update path actually waits on, so a mutant that puts IsReady
// back at that call site passes the whole file. That mutant is the original defect.
//
// Asserted over the AST rather than by grep because the thing being asserted is structural: WHICH
// argument of WHICH call inside WHICH function. A grep for "servesVersion" is satisfied by the
// declaration and by this test's own name, and a grep for "IsReady" cannot tell the create path's
// legitimate use from the update path's defective one.
func TestApplyOrUpdateCRDWaitsOnTheVersionAwarePredicate(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "crd.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing crd.go: %v", err)
	}

	var fn *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "ApplyOrUpdateCRD" {
			fn = fd
			break
		}
	}
	if fn == nil {
		t.Fatal("ApplyOrUpdateCRD not found in crd.go — this test has lost its subject and would " +
			"otherwise report nothing at all")
	}

	// Collect the predicate passed to every watcher.NewWatcher(...) call, in source order.
	var predicates []string
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "NewWatcher" {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "watcher" {
			return true
		}
		if len(call.Args) == 0 {
			predicates = append(predicates, "<no args>")
			return true
		}
		switch arg := call.Args[len(call.Args)-1].(type) {
		case *ast.Ident:
			predicates = append(predicates, arg.Name)
		case *ast.CallExpr:
			if id, ok := arg.Fun.(*ast.Ident); ok {
				predicates = append(predicates, id.Name+"(...)")
			} else {
				predicates = append(predicates, "<call>")
			}
		default:
			predicates = append(predicates, "<other>")
		}
		return true
	})

	// THE DENOMINATOR. Two wait sites is the fact the asymmetry below rests on; if a refactor
	// collapses or adds one, the positional assertions stop meaning what they say and this must
	// fail rather than silently assert about the wrong call.
	if len(predicates) != 2 {
		t.Fatalf("expected exactly 2 watcher.NewWatcher calls in ApplyOrUpdateCRD, found %d: %v. "+
			"The create path legitimately waits on IsReady and the update path must not, so the "+
			"count is load-bearing for this assertion rather than incidental", len(predicates), predicates)
	}

	// CREATE path: IsReady is correct here. Established genuinely transitions false->true for a
	// brand-new CRD and it has exactly one version, so the two predicates agree.
	if predicates[0] != "IsReady" {
		t.Errorf("the CREATE path waits on %q, expected IsReady. If this changed deliberately, "+
			"change this assertion -- but a version-aware predicate here has to be checked against "+
			"a CRD that has no status yet", predicates[0])
	}

	// UPDATE path: the whole fix. IsReady here is vacuous -- the CRD is already Established and
	// never goes False, so the wait returns having proven only that the object exists.
	if predicates[1] != "servesVersion(...)" {
		t.Errorf("the UPDATE path waits on %q, expected servesVersion(...). This is #339 exactly: "+
			"on the update path Established is already True and never goes False, so waiting on "+
			"IsReady returns immediately without establishing that the version this reconcile just "+
			"appended is one the apiserver will answer on -- and installer's umbrella prunes live "+
			"compositions on that answer", predicates[1])
	}
}

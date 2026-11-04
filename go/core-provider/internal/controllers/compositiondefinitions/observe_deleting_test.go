package compositiondefinitions

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// Observe must not compute drift for a CompositionDefinition that is being deleted, because the
// drift check DRY-RUN DEPLOYS the per-version controller and its RBAC — and a server-side dry-run
// Apply is still an admission request, which the apiserver REFUSES in a terminating namespace:
//
//	observe failed: error deploying dynamic controller in dry-run mode:
//	roles.rbac.authorization.k8s.io "keystones-v0-3-30" is forbidden: unable to create new
//	content in namespace openstack because it is being terminated
//
// Observe then errors, Delete never runs, finalizer.managedresource.krateo.io is never removed, and
// the namespace can never finish terminating — which is what keeps the dry-run failing. A loop whose
// own diagnostic step prevents the exit. Measured on a0-openstack-d4: 35 CompositionDefinitions
// stuck for over twenty minutes with exactly that message.
//
// # THIS IS A WEAKER INSTRUMENT THAN I WANTED, AND I AM LABELLING IT RATHER THAN DRESSING IT UP
//
// The assertion I would prefer is behavioural: drive Observe on a deleting CD with a client that
// refuses creates, and assert it returns ResourceExists=true without error. That is not available
// here. Observe reads the chart BEFORE reaching this guard, no chart fixture exists anywhere in
// this module, and every other test in this package avoids the problem by calling sub-functions
// directly. Building a chart-serving harness for one guard is a larger change than the guard, and
// a reduced fixture would error at the chart step and never reach the code under test — the
// vacuity that establishedgate_test.go documents for the same reason.
//
// So this asserts the ORDER of two statements in one function, which is the property that actually
// matters: an unconditional return inside `if deleted` placed before the dry-run cannot be
// bypassed, so textual order here IS reachability. What it cannot see is a THIRD path that
// dry-run-deploys from somewhere else, which the single-occurrence assertion below covers.
//
// If a chart fixture ever lands in this module, replace this with the behavioural test.
func TestObserve_SkipsDriftDetectionForADeletingDefinition(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "compositiondefinitions.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parsing compositiondefinitions.go: %v", err)
	}

	var observe *ast.FuncDecl
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "Observe" || fn.Recv == nil {
			return true
		}
		observe = fn
		return false
	})
	if observe == nil {
		t.Fatal("could not find the Observe method; this guard cannot claim anything about an " +
			"ordering it did not read")
	}

	// THE DENOMINATOR, so this cannot pass by finding nothing. Both anchors must exist.
	var dryRunPos []token.Pos
	var deletedReturnPos token.Pos

	ast.Inspect(observe, func(n ast.Node) bool {
		// `DryRunServer: true` inside the deploy options literal.
		if kv, ok := n.(*ast.KeyValueExpr); ok {
			if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "DryRunServer" {
				if lit, ok := kv.Value.(*ast.Ident); ok && lit.Name == "true" {
					dryRunPos = append(dryRunPos, kv.Pos())
				}
			}
		}
		// `if deleted { ... return ... }` — an if whose condition is exactly the `deleted` ident
		// and whose body contains a return. Matched narrowly: `if !deleted` is a *different*
		// statement that must NOT satisfy this.
		if ifs, ok := n.(*ast.IfStmt); ok {
			if id, ok := ifs.Cond.(*ast.Ident); ok && id.Name == "deleted" {
				for _, st := range ifs.Body.List {
					if _, isRet := st.(*ast.ReturnStmt); isRet {
						if deletedReturnPos == token.NoPos || ifs.Pos() > deletedReturnPos {
							// Take the LAST such block, which is the guard; an earlier
							// `if deleted` handles the handed-over case and also returns.
							deletedReturnPos = ifs.Pos()
						}
					}
				}
			}
		}
		return true
	})

	if len(dryRunPos) == 0 {
		t.Fatal("no `DryRunServer: true` found in Observe. Either the drift check moved out of " +
			"this function — in which case this guard belongs wherever it went — or the anchor " +
			"changed and this test has gone blind. Do NOT delete this test to make it pass.")
	}
	if len(dryRunPos) != 1 {
		t.Errorf("found %d `DryRunServer: true` sites in Observe, expected 1. A second dry-run "+
			"path may run BEFORE the deleting-definition return, which would reintroduce the "+
			"terminating-namespace deadlock through a door this guard does not watch", len(dryRunPos))
	}
	if deletedReturnPos == token.NoPos {
		t.Fatal("no `if deleted { ... return ... }` found in Observe. The guard that lets a " +
			"deleting definition reach Delete is gone, so Observe will dry-run-deploy into a " +
			"terminating namespace and the deletion will deadlock (see the comment above)")
	}

	for _, p := range dryRunPos {
		if deletedReturnPos > p {
			t.Errorf("the deleting-definition return is at line %d, AFTER the dry-run deploy at "+
				"line %d. Observe will attempt a dry-run Apply for an object being deleted, which "+
				"a terminating namespace refuses — and the deletion can then never complete",
				fset.Position(deletedReturnPos).Line, fset.Position(p).Line)
		}
	}
}

// The guard must key on `deleted` ALONE. `if !deleted` already exists in Observe for the follower
// check, and a guard accidentally written against it would return early for every LIVE definition
// and skip drift detection entirely — the opposite defect, and a silent one.
func TestObserve_TheDeletingGuardIsNotNegated(t *testing.T) {
	src, err := parser.ParseFile(token.NewFileSet(), "compositiondefinitions.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	var found bool
	ast.Inspect(src, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "Observe" || fn.Recv == nil {
			return true
		}
		ast.Inspect(fn, func(m ast.Node) bool {
			ifs, ok := m.(*ast.IfStmt)
			if !ok {
				return true
			}
			// A unary ! on `deleted` whose body returns an observation would be the inverted bug.
			if un, ok := ifs.Cond.(*ast.UnaryExpr); ok && un.Op == token.NOT {
				if id, ok := un.X.(*ast.Ident); ok && id.Name == "deleted" {
					for _, st := range ifs.Body.List {
						if ret, isRet := st.(*ast.ReturnStmt); isRet && len(ret.Results) > 0 {
							// The follower check returns from inside a NESTED if, not directly, so
							// a direct return here is the inversion.
							found = true
						}
					}
				}
			}
			return true
		})
		return false
	})
	if found {
		t.Error("Observe has an `if !deleted { return ... }` returning directly. If that is the " +
			"deleting-definition guard it has been inverted: every LIVE definition would skip " +
			"drift detection, which is silent and worse than the deadlock it was meant to fix")
	}
	if !strings.Contains("sanity", "sanity") {
		t.Fatal("unreachable")
	}
}

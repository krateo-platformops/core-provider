package composition

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// #335. EVERY PHASE THAT NAMES A HELM RELEASE MUST RESOLVE IT, not read the label.
//
// The label does not exist on #303's adoption path — its absence is what triggers adoption — so a
// phase that reads it gets "" and every helm call it makes fails on an empty release name. That
// was measured on 2.13.61: the adoption decision, the upgrade and convergence all worked, and the
// composition then reported Synced=False/ReconcileError forever on
// `get release failed: releaseContent: Release name is invalid: `.
//
// WHY THIS IS AN AST WALK AND NOT A BEHAVIOUR TEST, stated plainly because the shape is one this
// repo distrusts: none of these four phases can be exercised without a chart fixture this
// repository does not have, so a mutant that removes the resolve from any of them survives every
// behavioural test. #322 wired three phases and left two, and nothing failed — the gap was found
// on a live cluster by a peer, twice, three weeks of release apart in effect.
//
// So this asserts a STRUCTURAL property that the untestable sites do have: within each phase, a
// call to resolveReleaseName appears before the first use of GetReleaseName. It cannot prove the
// resolved name is correct — pickAdoptableRelease's own tests do that — only that the phase does
// not skip the resolution. An AST guard cannot tell mention from reachability, which is why it is
// scoped to ORDER inside one function rather than to presence anywhere in the file.
func TestEveryHelmPhaseResolvesTheReleaseNameBeforeReadingIt(t *testing.T) {
	const want = "resolveReleaseName"
	phases := map[string]bool{"Observe": false, "Create": false, "Update": false, "Delete": false}

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "composition.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parsing composition.go: %v", err)
	}

	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || fn.Body == nil {
			continue
		}
		if _, tracked := phases[fn.Name.Name]; !tracked {
			continue
		}
		phases[fn.Name.Name] = true

		resolveAt, readAt := -1, -1
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pos := fset.Position(call.Pos()).Line
			switch sel.Sel.Name {
			case want:
				if resolveAt == -1 || pos < resolveAt {
					resolveAt = pos
				}
			case "GetReleaseName":
				if readAt == -1 || pos < readAt {
					readAt = pos
				}
			}
			return true
		})

		if readAt == -1 {
			continue // the phase never names a release; nothing to order
		}
		if resolveAt == -1 {
			t.Errorf("%s reads GetReleaseName at line %d and never calls %s. On the adoption path "+
				"the label is ABSENT, so this phase runs on \"\" and every helm call it makes fails "+
				"with `Release name is invalid: ` -- forever, because nothing retries into a "+
				"different answer (#335)", fn.Name.Name, readAt, want)
			continue
		}
		if resolveAt > readAt {
			t.Errorf("%s calls %s at line %d but reads GetReleaseName first at line %d, so the "+
				"read sees the unresolved label", fn.Name.Name, want, resolveAt, readAt)
		}
	}

	// THE DENOMINATOR. A rename, a move to another file, or a typo in this list would otherwise
	// leave the walk inspecting nothing and passing — the failure mode every check of this shape
	// has, and the one that makes a zero indistinguishable from a clean result.
	for name, found := range phases {
		if !found {
			t.Errorf("phase %q was not found in composition.go, so this test inspected fewer "+
				"functions than it claims. Fix the list rather than the assertion", name)
		}
	}
	if len(phases) != 4 {
		t.Fatalf("expected to inspect 4 phases, the list has %d", len(phases))
	}
}

// And the property the AST walk cannot see: Update must PERSIST what it resolved, or the next
// reconcile re-derives it and every other consumer of the label still reads "".
//
// Observe cannot persist (it is a pure read, #110/#130) and Create persists already — but on the
// adoption path the runtime routes to UPDATE, because the adopted release exists and Observe
// therefore reports ResourceExists=true. So Create never runs, and Update is the only phase that
// can make the adopted name durable. Measured on 2.13.61: `krateo.io/release-name = None` on a
// composition that had successfully adopted and upgraded.
func TestUpdatePersistsTheResolvedReleaseNameBeforeItsMetadataWrite(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "composition.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parsing composition.go: %v", err)
	}

	var setAt, persistAt int
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || fn.Name.Name != "Update" || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pos := fset.Position(call.Pos()).Line
			name := sel.Sel.Name
			pkg := ""
			if id, ok := sel.X.(*ast.Ident); ok {
				pkg = id.Name
			}
			if name == "SetReleaseName" && (setAt == 0 || pos < setAt) {
				setAt = pos
			}
			if name == "Update" && pkg == "tools" && (persistAt == 0 || pos < persistAt) {
				persistAt = pos
			}
			return true
		})
	}

	if setAt == 0 {
		t.Fatal("Update never calls SetReleaseName, so an adopted name is re-derived on every " +
			"reconcile and every other consumer of krateo.io/release-name reads \"\" (#335)")
	}
	if persistAt == 0 {
		t.Fatal("Update has no tools.Update call, so there is nothing to persist the label WITH; " +
			"the ordering this test exists to check cannot be expressed")
	}
	if setAt > persistAt {
		t.Errorf("Update sets the release name at line %d but persists its metadata at line %d, "+
			"so the write happens before the label is on the object and the label is never stored",
			setAt, persistAt)
	}
}

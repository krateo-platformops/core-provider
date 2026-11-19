package cdreason

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/krateo-platformops/core-provider/internal/tools/restactionrbac"
)

// #568 child 5. Every restactionrbac.Cause constant has an ApiRefRBACComplete=False row: the cause
// IS the condition's reason, so a cause added without a row would write a reason the registry (and
// docs/cd-ready-reasons.md) does not list. Read from the source, so a new constant cannot hide.
func TestEveryRESTActionCauseIsRegistered(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "../tools/restactionrbac/cause.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var causes []string
	for _, d := range file.Decls {
		g, ok := d.(*ast.GenDecl)
		if !ok || g.Tok != token.CONST {
			continue
		}
		for _, sp := range g.Specs {
			vs := sp.(*ast.ValueSpec)
			if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "Cause" {
				continue
			}
			for _, v := range vs.Values {
				if bl, ok := v.(*ast.BasicLit); ok {
					c, _ := strconv.Unquote(bl.Value)
					causes = append(causes, c)
				}
			}
		}
	}
	if len(causes) < 8 {
		t.Fatalf("found %d Cause constants in cause.go, want at least 8: the parse is not reading them", len(causes))
	}
	for _, c := range causes {
		r, ok := ApiRefRBACFailure(restactionrbac.Cause(c))
		if !ok {
			t.Errorf("restactionrbac.Cause %q has no ApiRefRBACComplete row", c)
			continue
		}
		if r.String() != c || r.Type() != TypeApiRefRBACComplete || r.Status() != "False" {
			t.Errorf("row for %q is %s/%s/%s", c, r.Type(), r, r.Status())
		}
	}
	listed := 0
	for _, r := range Table() {
		if r.Type() == TypeApiRefRBACComplete && r.Status() == "False" {
			listed++
		}
	}
	if listed != len(causes) {
		t.Errorf("the table lists %d ApiRefRBACComplete=False rows for %d causes", listed, len(causes))
	}
}

// #568 child 5: a condition of any type is built only here, so the registered set is the whole set.
// Outside internal/cdreason, a provider-runtime Condition composite literal with fields, or an
// assignment to a condition's Reason, is refused. An empty Condition{} (a zero return) is allowed.
// Scope as TestNoReadyConditionBuiltOutsideCdreason (kubernetestargets has its own kind). CONTROL:
// the detector must find this package's own constructions.
func TestNoRegisteredConditionBuiltOutsideCdreason(t *testing.T) {
	root := "../.."
	fset := token.NewFileSet()
	scanned, controlHits := 0, 0
	var violations []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if name := d.Name(); name == "vendor" || name == "testdata" || (strings.HasPrefix(name, ".") && path != root) ||
				rel == "internal/controllers/kubernetestargets" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		scanned++
		sites := conditionConstructionSites(fset, file)
		if strings.HasPrefix(rel, "internal/cdreason/") {
			controlHits += len(sites)
			return nil
		}
		violations = append(violations, sites...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("scanned %d files; control: %d constructions inside internal/cdreason", scanned, controlHits)
	if controlHits == 0 {
		t.Fatal("control: the detector found no construction inside internal/cdreason, so a zero elsewhere says nothing")
	}
	for _, v := range violations {
		t.Errorf("condition built outside internal/cdreason: %s (use a cdreason.Reason)", v)
	}
}

// conditionConstructionSites finds `rtv1.Condition{...}` literals with at least one field, and
// assignments to the Type, Status or Reason of a variable the file declares as an rtv1.Condition (from
// a Condition literal or a `var x rtv1.Condition`), in a file importing provider-runtime's API package.
// Tracking the variable, rather than matching any `.Reason =`, keeps a status struct's own Reason field
// (ScaleToZeroStatus) out of it.
func conditionConstructionSites(fset *token.FileSet, file *ast.File) []string {
	alias := ""
	for _, imp := range file.Imports {
		if p, _ := strconv.Unquote(imp.Path.Value); p == runtimeAPIPkg {
			alias = "v1"
			if imp.Name != nil {
				alias = imp.Name.Name
			}
		}
	}
	if alias == "" {
		return nil
	}
	isCondType := func(e ast.Expr) bool {
		se, ok := e.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		id, ok := se.X.(*ast.Ident)
		return ok && id.Name == alias && se.Sel.Name == "Condition"
	}
	conds := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			for i, r := range n.Rhs {
				if cl, ok := r.(*ast.CompositeLit); ok && isCondType(cl.Type) && i < len(n.Lhs) {
					if id, ok := n.Lhs[i].(*ast.Ident); ok {
						conds[id.Name] = true
					}
				}
			}
		case *ast.ValueSpec:
			if n.Type != nil && isCondType(n.Type) {
				for _, id := range n.Names {
					conds[id.Name] = true
				}
			}
		}
		return true
	})
	var out []string
	ast.Inspect(file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CompositeLit:
			if isCondType(n.Type) && len(n.Elts) > 0 {
				out = append(out, fset.Position(n.Pos()).String())
			}
		case *ast.AssignStmt:
			for _, l := range n.Lhs {
				se, ok := l.(*ast.SelectorExpr)
				if !ok {
					continue
				}
				id, ok := se.X.(*ast.Ident)
				if ok && conds[id.Name] && (se.Sel.Name == "Type" || se.Sel.Name == "Status" || se.Sel.Name == "Reason") {
					out = append(out, fset.Position(l.Pos()).String())
				}
			}
		}
		return true
	})
	return out
}

package compositiondefinitions

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	rtv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
)

// #395. A drift return keeps the definition's last real Ready, and states "update pending" only when
// there is none (absent, or NotObserved after an Observe error).
func TestMarkUpdatePending(t *testing.T) {
	for _, c := range []struct {
		name       string
		start      *rtv1.Condition
		wantReason rtv1.ConditionReason
		wantMsg    string
	}{
		{"absent becomes Creating", nil, rtv1.ReasonCreating, "update pending: x"},
		{"NotObserved becomes Creating", ptrCond(rtv1.NotObserved()), rtv1.ReasonCreating, "update pending: x"},
		{"Available is kept (no flap per chart change)", ptrCond(rtv1.Available()), rtv1.ReasonAvailable, ""},
		{"Unavailable is kept", ptrCond(rtv1.Unavailable()), rtv1.ReasonUnavailable, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			cr := &compositiondefinitionsv1alpha1.CompositionDefinition{}
			if c.start != nil {
				cr.SetConditions(*c.start)
			}
			markUpdatePending(cr, "x")
			got := cr.GetCondition(rtv1.TypeReady)
			if got.Reason != c.wantReason {
				t.Errorf("Ready reason = %q, want %q", got.Reason, c.wantReason)
			}
			if c.wantMsg != "" && got.Message != c.wantMsg {
				t.Errorf("Ready message = %q, want %q", got.Message, c.wantMsg)
			}
		})
	}
}

func ptrCond(c rtv1.Condition) *rtv1.Condition { return &c }

// Every Observe return that hands the reconcile to Update (ResourceExists: true, ResourceUpToDate:
// false) is preceded, in the same block, by a statement that sets Ready: markUpdatePending, or an
// explicit cr.SetConditions(rtv1.<Ready constructor>...) (the CRD-version-not-found path states
// Unavailable itself). A SetConditions of any OTHER condition does not count. So a new drift return
// cannot skip the v1.8.0 Ready contract. The count (7) is asserted so the guard cannot pass by
// reading nothing; it found the seventh, which my manual audit had filed as already covered.
func TestEveryDriftReturnInObserveMarksUpdatePending(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "compositiondefinitions.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var drift, unmarked []int
	ast.Inspect(f, func(n ast.Node) bool {
		fd, ok := n.(*ast.FuncDecl)
		if !ok || fd.Name.Name != "Observe" || fd.Recv == nil {
			return true
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			blk, ok := n.(*ast.BlockStmt)
			if !ok {
				return true
			}
			for i, st := range blk.List {
				ret, ok := st.(*ast.ReturnStmt)
				if !ok || len(ret.Results) == 0 || !isDriftObservation(ret.Results[0]) {
					continue
				}
				line := fset.Position(ret.Pos()).Line
				drift = append(drift, line)
				marked := false
				for _, prev := range blk.List[:i] {
					if es, ok := prev.(*ast.ExprStmt); ok {
						if call, ok := es.X.(*ast.CallExpr); ok && setsReady(call) {
							marked = true
						}
					}
				}
				if !marked {
					unmarked = append(unmarked, line)
				}
			}
			return true
		})
		return false
	})
	if len(drift) != 7 {
		t.Errorf("found %d drift returns in Observe (lines %v), want 7; update this count deliberately", len(drift), drift)
	}
	if len(unmarked) > 0 {
		t.Errorf("drift returns without markUpdatePending in their block: lines %v", unmarked)
	}
}

func isDriftObservation(e ast.Expr) bool {
	cl, ok := e.(*ast.CompositeLit)
	if !ok {
		return false
	}
	var exists, upToDate string
	for _, el := range cl.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		k, _ := kv.Key.(*ast.Ident)
		v, _ := kv.Value.(*ast.Ident)
		if k == nil || v == nil {
			continue
		}
		switch k.Name {
		case "ResourceExists":
			exists = v.Name
		case "ResourceUpToDate":
			upToDate = v.Name
		}
	}
	return strings.EqualFold(exists, "true") && strings.EqualFold(upToDate, "false")
}

// setsReady reports whether call is markUpdatePending(...) or <x>.SetConditions(...) whose argument is
// built from a provider-runtime Ready constructor (rtv1.Available/Creating/Unavailable/Deleting),
// possibly through a method chain such as .WithMessage(...).
func setsReady(call *ast.CallExpr) bool {
	if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "markUpdatePending" {
		return true
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "SetConditions" || len(call.Args) == 0 {
		return false
	}
	found := false
	ast.Inspect(call.Args[0], func(n ast.Node) bool {
		s, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if x, ok := s.X.(*ast.Ident); ok && x.Name == "rtv1" {
			switch s.Sel.Name {
			case "Available", "Creating", "Unavailable", "Deleting":
				found = true
			}
		}
		return true
	})
	return found
}

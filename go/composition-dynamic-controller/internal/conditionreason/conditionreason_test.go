package conditionreason

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	compositioncondition "github.com/krateo-platformops/composition-dynamic-controller/internal/condition"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Every Reason builds a Ready condition with its own reason and status, a set transition time and
// no message; WithMessage changes only the message.
func TestConditionCarriesReasonAndStatus(t *testing.T) {
	for _, r := range Table() {
		c := r.Condition()
		if c.Type != r.Type() || c.Reason != r.String() || c.Status != r.Status() || c.Message != "" || c.LastTransitionTime.IsZero() {
			t.Errorf("%s: got %+v", r, c)
		}
		if m := r.WithMessage("why"); m.Message != "why" || m.Reason != r.String() || m.Status != r.Status() {
			t.Errorf("%s WithMessage: got %+v", r, m)
		}
	}
}

// The graceful pause's readiness half stays Ready=TRUE (#241), now built here.
func TestReconcileGracefullyPausedIsReadyTrue(t *testing.T) {
	c := ReconcileGracefullyPaused.Condition()
	if c.Status != metav1.ConditionTrue || c.Reason != compositioncondition.ReasonReconcileGracefullyPaused {
		t.Fatalf("got %+v, want Ready=True/%s", c, compositioncondition.ReasonReconcileGracefullyPaused)
	}
}

// A reason that the cdc and the runtime both write carries ONE status, and it is the status the
// runtime's own constructor builds. A consumer reading the reason must never need the writer.
func TestOneStatusPerReasonAndItMatchesTheRuntime(t *testing.T) {
	status := map[string]metav1.ConditionStatus{} // keyed type/reason: one reason string can live on two types
	for _, r := range Table() {
		key := r.Type() + "/" + r.String()
		if s, ok := status[key]; ok && s != r.Status() {
			t.Errorf("%s listed with status %s and %s", key, s, r.Status())
		}
		status[key] = r.Status()
	}
	ctors := RuntimeConstructors()
	checked := 0
	for _, c := range ctors {
		reason := c.Reason
		s, listed := status[c.Type+"/"+reason]
		if !listed {
			continue // a runtime constructor this module does not write (none today)
		}
		checked++
		if c.Status != s {
			t.Errorf("%s: listed %s, the runtime's constructor builds %s", reason, s, c.Status)
		}
	}
	if checked != len(ctors) {
		t.Errorf("checked %d of %d runtime constructors against the table", checked, len(ctors))
	}
}

// Reasons are listed once per writer: a duplicate row would make the table lie about its size.
func TestNoDuplicateRows(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range Table() {
		k := string(r.Writer()) + "/" + r.Type() + "/" + r.String()
		if seen[k] {
			t.Errorf("duplicate row %s", k)
		}
		seen[k] = true
	}
}

// Every Reason declared in this package is in Table(), so a reason a writer can use cannot be missing
// from docs/ready-reasons.md: a var added without a Table() entry would render nowhere, and a
// regenerated table would not notice (#528).
func TestEveryDeclaredReasonIsInTheTable(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "conditionreason.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	declared := map[string]bool{}
	inTable := map[string]bool{}
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.GenDecl:
			if d.Tok != token.VAR {
				continue
			}
			for _, sp := range d.Specs {
				vs := sp.(*ast.ValueSpec)
				for i, name := range vs.Names {
					if i >= len(vs.Values) {
						continue
					}
					if call, ok := vs.Values[i].(*ast.CallExpr); ok {
						if fn, ok := call.Fun.(*ast.Ident); ok && (fn.Name == "ready" || fn.Name == "on") {
							declared[name.Name] = true
						}
					}
				}
			}
		case *ast.FuncDecl:
			if d.Name.Name != "Table" {
				continue
			}
			ast.Inspect(d.Body, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok {
					inTable[id.Name] = true
				}
				return true
			})
		}
	}
	if len(declared) < 30 {
		t.Fatalf("control: found only %d declared reasons; the parse is not measuring the file", len(declared))
	}
	for name := range declared {
		if !inTable[name] {
			t.Errorf("%s is declared but not in Table(): it would be missing from docs/ready-reasons.md", name)
		}
	}
}

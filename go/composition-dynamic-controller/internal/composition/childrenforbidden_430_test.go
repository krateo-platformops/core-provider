package composition

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// #430. The rollup returned ChildrenForbidden and resolveReady turned it into Available, so a
// composition whose every child read was refused reported Ready=True. The existing tests checked the
// rollup's reason only. This one follows a ChildrenForbidden verdict all the way to the condition.
func TestChildrenForbiddenReachesTheConditionAsUnknown(t *testing.T) {
	v := healthVerdict{ready: false, reason: ReasonChildrenForbidden, message: "nothing assessed",
		unreadable: []string{"ns/a", "ns/b"}, total: 2}

	out := resolveReady(projectedHealth{}, readyMessageAvailable, v)
	if out.reason != ReasonChildrenForbidden {
		t.Fatalf("resolveReady turned the rollup's ChildrenForbidden into %q", out.reason)
	}
	mg := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "composition.krateo.io/v0-1-0",
		"kind": "Portal", "metadata": map[string]any{"name": "p", "namespace": "ns"}}}
	if err := stampReady(mg, out, true); err != nil {
		t.Fatal(err)
	}
	conds, _, _ := unstructured.NestedSlice(mg.Object, "status", "conditions")
	cm, _ := conds[0].(map[string]any)
	if stringField(cm, "status") != "Unknown" || stringField(cm, "reason") != ReasonChildrenForbidden {
		t.Errorf("Ready = %s/%s, want Unknown/ChildrenForbidden", stringField(cm, "status"), stringField(cm, "reason"))
	}

	// A blueprint's projected ready=true is evidence, exactly as for ZeroEvidence: Available. The
	// refused children stay named in the message by resolveReady's single exit.
	out = resolveReady(projectedHealth{present: true, ready: true}, readyMessageAvailable, v)
	if out.reason != "Available" || !strings.Contains(out.message, "could NOT BE READ") {
		t.Errorf("with ready=true projected: %q / %q, want Available naming the refused children", out.reason, out.message)
	}
}

// Every reason rollupManagedChildren can return must have an arm in resolveReadyVerdict's switch on
// v.reason (or be "Available", the tail). The stampReady guard walks what resolveReady PRODUCES, so a
// reason the rollup produced and resolveReady dropped was outside its domain: that is #430. This
// compares the rollup's reason expressions (healthVerdict literals and `reason` assignments) with the
// switch's case expressions, by source text, and fails naming any the switch does not handle.
func TestEveryReasonTheRollupReturnsIsHandledByResolveReady(t *testing.T) {
	missing, produced, handled := unhandledRollupReasons(t, "childhealth.go")
	if len(produced) < 5 {
		t.Fatalf("the walk found only %d rollup reasons (%v); it is not reading rollupManagedChildren", len(produced), produced)
	}
	if len(missing) > 0 {
		t.Errorf("rollup reasons with no arm in resolveReadyVerdict: %v (handled: %v)", missing, handled)
	}
}

func unhandledRollupReasons(t *testing.T, file string) (missing, produced, handled []string) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	src := func(e ast.Expr) string {
		start, end := fset.Position(e.Pos()).Offset, fset.Position(e.End()).Offset
		b, _ := readFileBytes(file)
		return string(b[start:end])
	}
	prod, hand := map[string]bool{}, map[string]bool{`"Available"`: true}
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok {
			continue
		}
		switch fd.Name.Name {
		case "rollupManagedChildren":
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CompositeLit:
					if id, ok := x.Type.(*ast.Ident); ok && id.Name == "healthVerdict" {
						for _, el := range x.Elts {
							if kv, ok := el.(*ast.KeyValueExpr); ok {
								if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "reason" {
									if id, ok := kv.Value.(*ast.Ident); !ok || id.Name != "reason" {
										prod[src(kv.Value)] = true
									}
								}
							}
						}
					}
				case *ast.AssignStmt:
					for i, l := range x.Lhs {
						if id, ok := l.(*ast.Ident); ok && id.Name == "reason" && i < len(x.Rhs) {
							prod[src(x.Rhs[i])] = true
						}
					}
				}
				return true
			})
		case "resolveReadyVerdict":
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				sw, ok := n.(*ast.SwitchStmt)
				if !ok || sw.Tag == nil || src(sw.Tag) != "v.reason" {
					return true
				}
				for _, st := range sw.Body.List {
					for _, e := range st.(*ast.CaseClause).List {
						hand[src(e)] = true
					}
				}
				return true
			})
		}
	}
	for r := range prod {
		produced = append(produced, r)
		if !hand[r] {
			missing = append(missing, r)
		}
	}
	for r := range hand {
		handled = append(handled, r)
	}
	sort.Strings(missing)
	sort.Strings(produced)
	sort.Strings(handled)
	return missing, produced, handled
}

func readFileBytes(name string) ([]byte, error) { return os.ReadFile(name) }

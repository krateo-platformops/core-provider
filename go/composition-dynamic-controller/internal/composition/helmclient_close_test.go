package composition

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestEveryHelmClientIsClosed guards krateo-core-provider#99.
//
// A helm client owns background resources: helm.WithCache() starts a DiskCache cleanup goroutine
// (hourly ticker) whose ONLY exit is Close(). Observe builds two such clients and runs on every
// resync, so before the fix each reconcile leaked ~2 goroutines permanently — monotone memory
// growth that tracked reconciliation work and drove three controllers into an OOMKill cycle against
// the shared 256Mi limit (a controller with zero compositions, which never reaches Observe, stayed
// flat — the control that isolated it).
//
// The client is constructed inline from h.kubeconfig with no injection seam, so this cannot be
// asserted behaviourally without a live apiserver. Instead this walks the AST and requires every
// helm.NewClient call site to be matched by a `defer hc.Close()`, so a new call site added without
// one fails here rather than in production a week later.
func TestEveryHelmClientIsClosed(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "composition.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing composition.go: %v", err)
	}

	var newClients, closes []token.Position

	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CallExpr:
			// helm.NewClient(...)
			if sel, ok := node.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "NewClient" {
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "helm" {
					newClients = append(newClients, fset.Position(node.Pos()))
				}
			}
		case *ast.DeferStmt:
			// defer hc.Close()
			if sel, ok := node.Call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Close" {
				if recv, ok := sel.X.(*ast.Ident); ok && recv.Name == "hc" {
					closes = append(closes, fset.Position(node.Pos()))
				}
			}
		}
		return true
	})

	if len(newClients) == 0 {
		t.Fatal("found no helm.NewClient call sites — this guard is not looking at the right file")
	}
	if len(closes) < len(newClients) {
		t.Errorf("every helm.NewClient must be matched by `defer hc.Close()` (#99 goroutine leak): "+
			"%d NewClient call sites but only %d deferred closes", len(newClients), len(closes))
		for _, p := range newClients {
			t.Logf("  helm.NewClient at %s", p)
		}
		for _, p := range closes {
			t.Logf("  defer hc.Close() at %s", p)
		}
	}
}

// TestHelmClientCloseIsValueCaptured guards the subtle half of the #99 fix.
//
// Observe reassigns `hc` to a second client. `defer hc.Close()` evaluates the receiver immediately,
// so each defer closes the client it was created for. A closure form — `defer func() { hc.Close() }()`
// — would capture the VARIABLE and close the second client twice; DiskCache.Stop() closes an
// unguarded channel, so the double close panics at runtime. Keep the direct form.
func TestHelmClientCloseIsValueCaptured(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "composition.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing composition.go: %v", err)
	}

	ast.Inspect(file, func(n ast.Node) bool {
		def, ok := n.(*ast.DeferStmt)
		if !ok {
			return true
		}
		lit, ok := def.Call.Fun.(*ast.FuncLit)
		if !ok {
			return true
		}
		// A deferred closure that calls hc.Close() captures the variable, not the value.
		ast.Inspect(lit, func(inner ast.Node) bool {
			call, ok := inner.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Close" {
				if recv, ok := sel.X.(*ast.Ident); ok && recv.Name == "hc" {
					t.Errorf("deferred closure calling hc.Close() at %s captures the variable, not the "+
						"value: after hc is reassigned this closes the same client twice and panics "+
						"in DiskCache.Stop(). Use `defer hc.Close()`.", fset.Position(inner.Pos()))
				}
			}
			return true
		})
		return true
	})
}

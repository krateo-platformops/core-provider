package composition

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/krateo-platformops/plumbing/conditions"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// #498 (Diego, 2026-11-14). A child that declares Ready=False/TerminalByDesign is resting at its
// healthy end state, so the rollup counts it healthy. Only that exact declaration: Pending, the
// reason that means both "in flight" and "resting for good", stays converging (pinned in
// reasoncontract_test.go), and the declaration on any status other than False is not one.
func TestTerminalByDesign_ChildIsHealthy(t *testing.T) {
	if got := krateoReady(krateoCR("False", "TerminalByDesign", true)); got != childHealthy {
		t.Fatalf("Ready=False/TerminalByDesign maps to %d, want childHealthy (%d)", got, childHealthy)
	}
	if got := krateoReady(krateoCR("False", "Pending", true)); got != childConverging {
		t.Errorf("Pending maps to %d, want childConverging (%d): terminality is declared, never inferred from Pending", got, childConverging)
	}
	if got := krateoReady(krateoCR("Unknown", "TerminalByDesign", true)); got == childHealthy {
		t.Error("Ready=Unknown/TerminalByDesign maps to childHealthy: the declaration is Ready=False with this reason, nothing else")
	}
}

// The parent's verdict, which is what anyone reads. A composition whose only not-Ready child is
// declared terminal is Available, not "Creating" forever. The control is the same child at Pending,
// which keeps the parent converging, so the assertion reads the reason rather than the setup.
func TestTerminalByDesign_ParentIsNotHeldBack(t *testing.T) {
	for _, tc := range []struct {
		reason    string
		wantReady bool
	}{{"TerminalByDesign", true}, {"Pending", false}} {
		t.Run(tc.reason, func(t *testing.T) {
			child := krateoCR("False", tc.reason, true)
			_ = unstructured.SetNestedField(child.Object, "provision", "metadata", "name")
			_ = unstructured.SetNestedField(child.Object, "ns", "metadata", "namespace")
			mg := &unstructured.Unstructured{Object: map[string]any{}}
			setManaged(mg, map[string]any{"apiVersion": "composition.krateo.io/v0-1-0", "resource": "portals", "name": "provision", "namespace": "ns"})

			v := (&handler{}).rollupManagedChildren(context.Background(), fakeDynWith(child), mg)
			if v.ready != tc.wantReady {
				t.Fatalf("parent ready=%v reason=%s message=%q, want ready=%v", v.ready, v.reason, v.message, tc.wantReady)
			}
		})
	}
}

// The literal the arm matches is plumbing's shared contract string, so the producer
// (rest-dynamic-controller, importing plumbing) and this reader cannot drift.
func TestTerminalByDesign_LiteralIsPlumbingsContract(t *testing.T) {
	if conditions.ReasonTerminalByDesign != "TerminalByDesign" {
		t.Fatalf("plumbing conditions.ReasonTerminalByDesign = %q, but krateoReady matches \"TerminalByDesign\"", conditions.ReasonTerminalByDesign)
	}
}

// READ ONLY. The cdc must never write TerminalByDesign on a composition: installer's dependency gate
// withholds a composition with an unrecognised Ready reason, which would drop its dependants from the
// umbrella render (installer, 2026-11-14). Writing it needs the string, so the guard is that the
// string, or plumbing's constant, appears in this module's non-test code ONLY inside krateoReady,
// exactly once (that one is the read arm; the count also fails if the arm is removed).
func TestTerminalByDesign_IsNeverWrittenOnAComposition(t *testing.T) {
	root := filepath.Join("..", "..") // the cdc module root
	fset := token.NewFileSet()
	inReader, elsewhere, files := 0, []string{}, 0
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "testdata" || d.Name() == "vendor" || strings.HasPrefix(d.Name(), ".")) && path != root {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		files++
		for _, decl := range f.Decls {
			reader := false
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == "krateoReady" {
				reader = true
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				hit := false
				switch x := n.(type) {
				case *ast.BasicLit:
					hit = x.Kind == token.STRING && strings.Trim(x.Value, "\"`") == "TerminalByDesign"
				case *ast.SelectorExpr:
					hit = x.Sel.Name == "ReasonTerminalByDesign"
				}
				if hit {
					if reader {
						inReader++
					} else {
						elsewhere = append(elsewhere, fset.Position(n.Pos()).String())
					}
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("non-test Go files parsed: %d; TerminalByDesign in krateoReady: %d; elsewhere: %d", files, inReader, len(elsewhere))
	if files < 20 {
		t.Fatalf("parsed only %d files: the walk did not cover the module, so a zero below would mean nothing", files)
	}
	if len(elsewhere) > 0 {
		t.Errorf("TerminalByDesign appears outside krateoReady, the only place the cdc may READ it: %v", elsewhere)
	}
	if inReader != 1 {
		t.Errorf("TerminalByDesign appears %d times in krateoReady, want exactly 1 (the read arm)", inReader)
	}
}

package compositiondefinitions

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// #314. Delete used to return on any chart-read failure, BEFORE every guard, so a
// CompositionDefinition whose chart had become unreadable could never be deleted: Observe answered
// from status via observeWithoutChart (#235) and reported the resource present, the runtime called
// Delete, and Delete returned at the chart fetch on every reconcile, forever.
//
// deleteGVK is the decision Delete now makes, extracted so every input state is testable without
// the clients the rest of Delete needs. Cases span the INPUT STATES — chart readable, chart
// unreadable with a recorded GVK, chart unreadable with nothing recorded — rather than the arms.
func TestDeleteGVK_SpansTheInputStates(t *testing.T) {
	fromChart := schema.GroupVersionKind{Group: "composition.krateo.io", Version: "v0-3-34", Kind: "Keystone"}
	recorded := schema.GroupVersionKind{Group: "composition.krateo.io", Version: "v0-3-33", Kind: "Keystone"}
	chartDown := errors.New("error getting chart info: secret openstack/registry-creds not found")

	cases := []struct {
		name     string
		chartGVK schema.GroupVersionKind
		chartErr error
		recorded schema.GroupVersionKind
		want     schema.GroupVersionKind
		wantErr  bool
	}{
		{
			// The chart wins when it is readable, even if status disagrees. Status is a fallback,
			// not a second opinion: a definition mid-bump records the OLD version on status, and
			// preferring it here would tear down the wrong one.
			name:     "chart readable — the chart's GVK, not the recorded one",
			chartGVK: fromChart, recorded: recorded,
			want: fromChart,
		},
		{
			// THE ARM #314 EXISTS FOR. Unreadable chart, but the definition reconciled before and
			// recorded what it generated. Delete needs nothing else from the chart, so this is a
			// complete substitute rather than a degraded one.
			name:     "chart unreadable, GVK on status — the recorded GVK, no error",
			chartErr: chartDown, recorded: recorded,
			want: recorded,
		},
		{
			// "Could not determine" must not read as "nothing to do". Never reconciled means
			// nothing to tear down in principle, but this is unreachable in practice —
			// observeWithoutChart reports ResourceExists=false for it, so the runtime finalizes
			// without calling Delete — and reaching it means something disagrees.
			name:     "chart unreadable, nothing recorded — the chart error, not a guess",
			chartErr: chartDown,
			wantErr:  true,
		},
	}

	for _, c := range cases {
		got, err := deleteGVK(c.chartGVK, c.chartErr, c.recorded)
		if c.wantErr {
			if err == nil {
				t.Errorf("%s: returned %v with no error. Removing a finalizer on a guess is the "+
					"destructive direction on a teardown path", c.name, got)
			} else if !errors.Is(err, c.chartErr) {
				t.Errorf("%s: the error does not wrap the chart failure (%v), so the operator "+
					"cannot see why", c.name, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: unexpected error %v — this is the wedge #314 describes: a definition "+
				"that cannot be deleted because its chart is unreadable", c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// WIRING. The table above tests the decision; it says nothing about whether Delete USES it. A
// Delete that kept its old early return on the chart error and merely called deleteGVK afterwards
// would pass the table while still wedging. So assert structurally that no chart-read error in
// Delete is returned directly — every one must flow through deleteGVK.
//
// Asserted over the AST because the claim is structural ("this function does not return chartErr
// before deleteGVK sees it"), and a grep cannot tell a return inside Delete from one elsewhere.
func TestDeleteRoutesEveryChartErrorThroughDeleteGVK(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "compositiondefinitions.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}

	var del *ast.FuncDecl
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name != "Delete" || fd.Recv == nil {
			continue
		}
		del = fd
	}
	if del == nil {
		t.Fatal("external.Delete not found — this test has lost its subject")
	}

	// Find the deleteGVK call, then assert nothing BEFORE it returns chartErr.
	var callPos token.Pos
	var earlyReturns []string
	ast.Inspect(del.Body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "deleteGVK" && callPos == 0 {
				callPos = call.Pos()
			}
		}
		if ret, ok := n.(*ast.ReturnStmt); ok {
			for _, r := range ret.Results {
				if id, ok := r.(*ast.Ident); ok && id.Name == "chartErr" {
					earlyReturns = append(earlyReturns, fset.Position(ret.Pos()).String())
				}
			}
		}
		return true
	})

	if callPos == 0 {
		t.Fatal("Delete does not call deleteGVK at all, so a chart-read failure has no fallback " +
			"and the #314 wedge is back")
	}
	for _, at := range earlyReturns {
		t.Errorf("Delete returns chartErr directly at %s, bypassing deleteGVK. That is the "+
			"original defect: a definition with an unreadable chart can never be deleted", at)
	}
}

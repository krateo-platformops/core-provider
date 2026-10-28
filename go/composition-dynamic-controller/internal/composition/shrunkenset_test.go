package composition

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// withManaged builds a composition carrying n managed-child entries.
func withManaged(n int) *unstructured.Unstructured {
	mg := &unstructured.Unstructured{Object: map[string]any{}}
	if n < 0 {
		return mg
	}
	items := make([]any, n)
	for i := range items {
		items[i] = map[string]any{"apiVersion": "v1", "resource": "configmaps", "name": "c"}
	}
	_ = unstructured.SetNestedSlice(mg.Object, items, "status", "managed")
	return mg
}

// #230: a render that produced FEWER children than the object was managing is not converged.
//
// status.managed is replaced wholesale from the render and is the only input to the health rollup,
// so a shrunken render shrinks the health denominator — the rollup evaluates the smaller set, finds
// it healthy, and reports Available with nothing saying the set moved. The installer session hit
// this on the 0.3.465 roll: the umbrella read Ready=True with 108 managed children instead of 122,
// and their gate reported 14 components as disappeared.
func TestManagedSetShrank_DetectsAShrink(t *testing.T) {
	shrank, previous := managedSetShrank(withManaged(122), 108)
	if !shrank {
		t.Fatal("a render of 108 against 122 managed children was not reported as a shrink; the " +
			"composition would report Available over a smaller denominator")
	}
	if previous != 122 {
		t.Errorf("previous = %d, want 122", previous)
	}
}

// THE ANTI-WEDGE PROPERTY, and the reason this compares against the PREVIOUS set rather than a
// high-water mark.
//
// A chart that legitimately drops a component must converge, not hang. Against the previous set it
// reports Creating for exactly ONE reconcile and then settles, because the comparison moves with
// the object. Against a high-water mark it would report Creating forever — which is the wedge this
// file's history is made of (#72's snowplow, #112's CronJob): a predicate treating a steady state
// as a transient one.
func TestManagedSetShrank_APermanentShrinkConvergesAfterOneReconcile(t *testing.T) {
	// Reconcile 1: the component is dropped. 122 -> 108, so Creating.
	if shrank, _ := managedSetShrank(withManaged(122), 108); !shrank {
		t.Fatal("the drop was not noticed at all")
	}
	// Reconcile 2: status.managed now carries 108 and the render is still 108.
	if shrank, _ := managedSetShrank(withManaged(108), 108); shrank {
		t.Fatal("a chart that settled at 108 is STILL reporting a shrink. Against a high-water " +
			"mark this is Ready=False forever; that is the wedge, and it is why the comparison " +
			"is against the previous set.")
	}
}

// Growth says nothing about convergence — an ordinary bump adding a component. The rollup already
// evaluates whatever is listed, so there is nothing to withhold.
func TestManagedSetShrank_GrowthIsNotASignal(t *testing.T) {
	if shrank, _ := managedSetShrank(withManaged(108), 122); shrank {
		t.Error("growth from 108 to 122 was reported as a shrink")
	}
	if shrank, _ := managedSetShrank(withManaged(122), 122); shrank {
		t.Error("an unchanged set of 122 was reported as a shrink")
	}
}

// No previous set is not evidence of a shrink. A first Update, or an object whose status predates
// the field, must report normally — inventing a verdict from an absent field is the mistake this
// file documents twice over.
func TestManagedSetShrank_NoPreviousSetIsNotAShrink(t *testing.T) {
	mg := &unstructured.Unstructured{Object: map[string]any{}}
	shrank, previous := managedSetShrank(mg, 122)
	if shrank {
		t.Error("an object with no status.managed was reported as having shrunk")
	}
	if previous != 0 {
		t.Errorf("previous = %d, want 0", previous)
	}

	// An explicitly empty set is a real zero, and a render of 0 against it is still not a shrink.
	if shrank, _ := managedSetShrank(withManaged(0), 0); shrank {
		t.Error("0 rendered against an empty managed set was reported as a shrink")
	}
}

// A shrink to zero is the sharpest case: every child gone. It must be caught, not treated as the
// no-evidence case above.
func TestManagedSetShrank_ShrinkToZeroIsCaught(t *testing.T) {
	shrank, previous := managedSetShrank(withManaged(122), 0)
	if !shrank {
		t.Fatal("a render of ZERO children against 122 was not reported as a shrink")
	}
	if previous != 122 {
		t.Errorf("previous = %d, want 122", previous)
	}
}

// The message must name the denominator rather than implying the children are unhealthy — the
// counted ones were all fine, which is exactly why Available was so misleading.
func TestShrunkenSetMessage_NamesTheDenominator(t *testing.T) {
	msg := shrunkenSetMessage(108, 122)
	for _, want := range []string{"108", "122", "previously-managed"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not contain %q", msg, want)
		}
	}
	if strings.Contains(strings.ToLower(msg), "unhealthy") || strings.Contains(strings.ToLower(msg), "failed") {
		t.Errorf("message implies the children are sick, which is not what was observed: %q", msg)
	}
}

// The WIRING, source-asserted. managedSetShrank is tested above; this asserts Update actually
// consults it and downgrades the verdict when it fires.
//
// Three times on 2026-10-27 a decision function was covered while the line calling it was not
// (#218's gate conjunction, #221's condition routing, #224's tagged suites), and in each case
// removing the call broke nothing. Update cannot be exercised in a unit test — it builds a helm
// client inline from h.kubeconfig — so this follows TestEveryHelmClientIsClosed and
// TestReconcilePathNeverUpgradesUnconditionally in this package: assert the call site.
//
// What it does NOT prove: that the shrunken condition reaches the object. setStatus's routing is
// covered by TestStampReady_RoutesEveryReasonToTheRightCondition. This owns the decision to
// downgrade; that owns the write.
func TestUpdateConsultsTheManagedSetSize(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "composition.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing composition.go: %v", err)
	}

	var sawUpdate, callsShrank, setsCreating bool
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || fn.Name.Name != "Update" {
			continue
		}
		sawUpdate = true
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.CallExpr:
				if id, ok := node.Fun.(*ast.Ident); ok && id.Name == "managedSetShrank" {
					callsShrank = true
				}
			case *ast.SelectorExpr:
				// statusOpts.conditionType = ConditionTypeCreating
				if node.Sel.Name == "ConditionTypeCreating" {
					setsCreating = true
				}
			case *ast.Ident:
				if node.Name == "ConditionTypeCreating" {
					setsCreating = true
				}
			}
			return true
		})
	}

	if !sawUpdate {
		t.Fatal("did not find func (h *handler) Update in composition.go — this guard is not " +
			"looking at the right file and proves nothing")
	}
	if !callsShrank {
		t.Error("Update does not call managedSetShrank. A render smaller than the managed set then " +
			"reports Available over a shrunken health denominator, which is #230 — a false " +
			"convergence signal to inst.depsReady during a multi-component bump.")
	}
	if !setsCreating {
		t.Error("Update never sets ConditionTypeCreating. The shrink is detected and then discarded, " +
			"which is the same shape as #221: a reason computed on every reconcile and never used.")
	}
}

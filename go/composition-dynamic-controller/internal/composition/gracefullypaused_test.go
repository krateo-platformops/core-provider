package composition

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/krateo-platformops/unstructured-runtime/pkg/tools/statusprojection"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// #232. Both effects of pausing were unreachable, and for one reason: the conditionType was
// assigned to statusOpts AFTER setStatus had already been handed it, at a point where statusOpts is
// never read again. Nothing else in the tree ever passed
// ConditionTypeReconcileGracefullyPaused to setStatus, so
//
//   - setGracefullyPausedCondition could not be called from anywhere, and
//   - the projection gate inside setStatus could not fire.
//
// A paused composition therefore reported Ready=Available and had its statusDataTemplate projected
// as though it were reconciling. 0 of 83 compositions on krateo-057 carry the annotation, which is
// why this sat unnoticed: the one path that would have exposed it is the one nobody walks.

func pausedTestHandler() *handler {
	return &handler{
		statusDataTemplate: []statusprojection.Mapping{
			{ForPath: "chartVersion", Expression: `${ .helm.version }`},
		},
	}
}

func pausedTestComposition() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "composition.krateo.io/v1-0-0",
		"kind":       "FireworksApp",
		"metadata":   map[string]any{"name": "demo", "namespace": "apps", "generation": int64(2)},
		"spec":       map[string]any{"host": "demo.example.com"},
	}}
}

func pausedTestOpts(ct ConditionType) *statusManagerOpts {
	return &statusManagerOpts{
		chartURL:        "oci://reg/fireworksapp",
		chartVersion:    "1.2.0",
		releaseStatus:   "deployed",
		releaseRevision: 7,
		releaseName:     "demo-release",
		digest:          "abc",
		message:         "Composition values updated",
		conditionType:   ct,
	}
}

// A differential: the same setStatus call, paused and not, asserting both effects in both
// directions. The control arm matters as much as the subject — a "fix" that skipped the projection
// unconditionally, or stamped the paused reason unconditionally, would satisfy the paused arm alone.
func TestSetStatus_PausedStampsThePausedConditionAndSkipsProjection(t *testing.T) {
	for _, c := range []struct {
		name            string
		ct              ConditionType
		wantReason      string
		wantProjected   bool
		whyNotProjected string
	}{
		{
			name: "paused", ct: ConditionTypeReconcileGracefullyPaused,
			wantReason: "ReconcileGracefullyPaused", wantProjected: false,
			whyNotProjected: "reconciliation is suspended, so a freshly-projected status would " +
				"assert current facts about a composition nobody is reconciling",
		},
		{
			name: "not paused", ct: ConditionTypeAvailable,
			wantReason: "Available", wantProjected: true,
			whyNotProjected: "the normal path must still project; skipping it unconditionally would " +
				"silently disable statusDataTemplate for every composition",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, mg := pausedTestHandler(), pausedTestComposition()
			if err := h.setStatus(context.Background(), mg, pausedTestOpts(c.ct)); err != nil {
				t.Fatalf("setStatus: %v", err)
			}

			status, reason, _ := readyCondition(t, mg)
			if reason != c.wantReason {
				t.Errorf("Ready reason = %q, want %q", reason, c.wantReason)
			}

			_, projected, _ := unstructured.NestedString(mg.Object, "status", "chartVersion")
			if projected != c.wantProjected {
				t.Errorf("status.chartVersion present = %v, want %v: %s",
					projected, c.wantProjected, c.whyNotProjected)
			}

			// The BASELINE status is written either way. Pausing suspends reconciliation; it does
			// not make the release that just happened untrue, and dropping the digest would make the
			// next reconcile see spurious drift.
			if got, found, _ := unstructured.NestedString(mg.Object, "status", "digest"); !found || got != "abc" {
				t.Errorf("status.digest = %q (found=%v), want abc even when paused", got, found)
			}

			// Pinned deliberately: ReconcileGracefullyPaused() is Status=TRUE. So a paused
			// composition still SATISFIES a readiness gate (the installer chart's accepts True, and
			// krateoReady maps True to childHealthy) — wiring #232 up changes what operators and
			// dashboards see, NOT what gates on it. If that is ever reconsidered, changing the
			// Status is the gating change and belongs in its own issue; this test will fail and say
			// so rather than letting it ride along with a label fix.
			if status != "True" {
				t.Errorf("Ready status = %q, want True; flipping this is a GATING change, not a "+
					"relabelling, because dependents and parent rollups branch on it", status)
			}
		})
	}
}

// THE WIRING, and it is the actual defect rather than a proxy for it. Update cannot be unit-tested
// (it drives helm and a live dynamic client), which is exactly why the dead store survived.
//
// The invariant is structural and narrow: statusManagerOpts.conditionType must be set in the
// composite literal, never assigned afterwards. Every assignment to it that this file ever held was
// a dead store, because the opts are built, handed to setStatus, and not read again. Asserting
// "no such assignment exists" catches the shape, not one instance of it — a future editor who adds
// `statusOpts.conditionType = ...` anywhere in this file fails here regardless of which branch they
// put it in.
func TestConditionTypeIsNeverAssignedAfterTheOptsAreBuilt(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "composition.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing composition.go: %v", err)
	}

	var offenders []string
	ast.Inspect(file, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for _, lhs := range as.Lhs {
			if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "conditionType" {
				offenders = append(offenders, fset.Position(as.Pos()).String())
			}
		}
		return true
	})
	if len(offenders) > 0 {
		t.Errorf("conditionType is assigned after construction at %v. statusManagerOpts is built, "+
			"passed to setStatus and never read again, so any such assignment is a DEAD STORE: it "+
			"silently disables both the paused Ready condition and the projection skip, which is "+
			"#232. Set it in the literal instead.", offenders)
	}
}

// The DECISION itself, which is the half a source guard cannot cover.
//
// This test exists because of a failed mutation rather than by design. The first version of #232
// left the conditional inline in Update and relied on an AST guard asserting that
// ConditionTypeReconcileGracefullyPaused appeared in Update's body. Changing that conditional to
// `if paused && false` SURVIVED: the constant was still mentioned, in dead code, and the guard
// cannot tell mention from reachability. Extracting convergedConditionType makes the decision a
// unit — small, total, and falsifiable.
func TestConvergedConditionType(t *testing.T) {
	if got := convergedConditionType(true); got != ConditionTypeReconcileGracefullyPaused {
		t.Errorf("paused -> %q, want ReconcileGracefullyPaused. This is the only producer of that "+
			"value anywhere in the tree, so if it cannot be produced, setGracefullyPausedCondition "+
			"is uncallable and the projection gate cannot fire — the #232 state exactly.", got)
	}
	if got := convergedConditionType(false); got != ConditionTypeAvailable {
		t.Errorf("not paused -> %q, want Available; the normal path must still derive Ready from "+
			"managed-child health", got)
	}
}

// And the wiring: Update must actually route through that decision. The guard above forbids the
// dead-store shape and this one requires the live one, so neither "assign it afterwards" nor
// "delete pausing entirely" passes.
func TestUpdateRoutesThroughConvergedConditionType(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "composition.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing composition.go: %v", err)
	}

	var sawUpdate, callsDecision, detectsPause bool
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || fn.Name.Name != "Update" {
			continue
		}
		sawUpdate = true
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			ce, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch f := ce.Fun.(type) {
			case *ast.Ident:
				if f.Name == "convergedConditionType" {
					callsDecision = true
				}
			case *ast.SelectorExpr:
				if f.Sel.Name == "IsGracefullyPaused" {
					detectsPause = true
				}
			}
			return true
		})
	}

	if !sawUpdate {
		t.Fatal("did not find func (h *handler) Update — this guard is asserting nothing")
	}
	if !detectsPause {
		t.Error("Update no longer calls IsGracefullyPaused, so a pause cannot be detected at all")
	}
	if !callsDecision {
		t.Error("Update does not call convergedConditionType, so the conditionType it passes to " +
			"setStatus cannot depend on whether the composition is paused — both paused effects " +
			"become unreachable again, which is #232")
	}
}

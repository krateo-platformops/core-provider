package composition

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// #337. EVERY DRIFT RETURN IN Observe MUST COME AFTER THE STATUS WRITE.
//
// A reconcile whose self-heal apply changed an object used to return
// ExternalObservation{ResourceExists: true, ResourceUpToDate: false} BEFORE the setStatus /
// updateStatusWithRetry pair, so it reported drift without ever computing or writing Ready. The
// runtime cannot compensate: its !ResourceUpToDate branch persists only the conditions the handler
// RETURNED, and that path returns none.
//
// Measured on a constructed probe by the openstack-ironic-operator-kog session: `Synced` present,
// `Ready` ABSENT — which every consumer reads as "not evaluated yet", indefinitely. Invisible on a
// composition that has reconciled before, because the previous pass's Ready is still there.
//
// ASSERTS THE WRITE, NOT THE VERDICT, and that is the whole point: all three drift returns produce
// a BYTE-IDENTICAL ExternalObservation, so a test that checks the returned observation cannot tell
// the path that wrote status from the two that did not. The only observable difference is ORDER.
//
// STRUCTURAL because Observe cannot be exercised without a chart fixture this repository does not
// have — the same reason #322 wired three of four phases and nothing failed (#335). Scoped to
// order inside one function, because an AST guard cannot tell mention from reachability.
func TestObserveWritesStatusBeforeEveryDriftReturn(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "composition.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parsing composition.go: %v", err)
	}

	var observe *ast.FuncDecl
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Recv != nil && fn.Name.Name == "Observe" && fn.Body != nil {
			observe = fn
			break
		}
	}
	if observe == nil {
		t.Fatal("Observe not found in composition.go: this test inspected nothing and would " +
			"otherwise pass. Fix the lookup, not the assertion")
	}

	// THE LANDMARK IS setStatus, NOT updateStatusWithRetry, AND THE FIRST VERSION OF THIS TEST GOT
	// THAT WRONG — it took the EARLIEST updateStatusWithRetry, which is at line ~391 in an entirely
	// different branch near the top of Observe. Every drift return is after line 391, so the test
	// passed trivially and a mutant restoring the #337 defect SURVIVED it.
	//
	// setStatus is the call that COMPUTES the Ready verdict for this pass; updateStatusWithRetry
	// appears four times and merely flushes whatever is on the object. Ordering against a flush
	// that may belong to another branch measures nothing. The requirement is: before a path reports
	// exists-but-not-up-to-date, this pass must have COMPUTED its Ready.
	//
	// A landmark that appears several times in several branches is not a landmark. Taking the
	// minimum of it was the same error as a count without a denominator: a true number about the
	// wrong subject.
	setStatusAt := 0
	ast.Inspect(observe.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "setStatus" {
			if pos := fset.Position(call.Pos()).Line; setStatusAt == 0 || pos < setStatusAt {
				setStatusAt = pos
			}
		}
		return true
	})
	if setStatusAt == 0 {
		t.Fatal("Observe has no setStatus call, so there is nothing that computes Ready for the " +
			"drift returns to be ordered against. Either it was renamed or the computation is " +
			"gone; both need a human")
	}
	persistAt := setStatusAt

	// Every ExternalObservation literal that is EXISTS-BUT-NOT-UP-TO-DATE, wherever it sits.
	//
	// ResourceExists: FALSE is deliberately excluded, and the first version of this test did not
	// exclude it — it flagged the release-not-found return and was right to make me justify the
	// exclusion rather than assume it. That path routes to CREATE, not Update, and Create computes
	// and persists status itself. There is also nothing to report Ready ABOUT yet: the release does
	// not exist. So being before the write is correct there and a defect in the exists case, where
	// the handler's Ready is the only source the runtime will not substitute for.
	//
	// Counted separately rather than silently skipped, so the exclusion is visible in the log and a
	// future reader can see the test knows about that return rather than having missed it.
	var driftLines []int
	notExists := 0
	ast.Inspect(observe.Body, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := lit.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "ExternalObservation" {
			return true
		}
		upToDateFalse, existsTrue, sawExists := false, false, false
		for _, elt := range lit.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok {
				continue
			}
			val, ok := kv.Value.(*ast.Ident)
			if !ok {
				continue
			}
			switch key.Name {
			case "ResourceUpToDate":
				upToDateFalse = val.Name == "false"
			case "ResourceExists":
				sawExists = true
				existsTrue = val.Name == "true"
			}
		}
		if !upToDateFalse {
			return true
		}
		if sawExists && !existsTrue {
			notExists++
			return true
		}
		driftLines = append(driftLines, fset.Position(lit.Pos()).Line)
		return true
	})

	// THE DENOMINATOR. Three drift returns exist today; a walk that found none would pass every
	// assertion below, which is the failure mode this whole file is about.
	if len(driftLines) < 3 {
		t.Fatalf("found %d drift returns in Observe, expected at least 3 (self-heal, stale "+
			"inventory, absent children). A walk that inspects fewer returns than exist proves "+
			"nothing about the ones it missed", len(driftLines))
	}
	t.Logf("inspected %d exists-but-not-up-to-date returns against the status write at line %d "+
		"(%d not-exists returns excluded, they route to Create)", len(driftLines), persistAt, notExists)
	if notExists == 0 {
		t.Error("no ResourceExists:false return was found, so the exclusion above is untested and " +
			"this walk may be matching something other than what it describes")
	}

	for _, line := range driftLines {
		if line < persistAt {
			t.Errorf("a drift return at line %d precedes the status write at line %d. That path "+
				"reports ResourceUpToDate=false without computing or writing Ready, and the "+
				"runtime will not fill it in — it persists only conditions the handler RETURNED. "+
				"On a composition with no previous Ready the condition is simply ABSENT, which "+
				"reads as \"not evaluated yet\" forever (#337)", line, persistAt)
		}
	}
}

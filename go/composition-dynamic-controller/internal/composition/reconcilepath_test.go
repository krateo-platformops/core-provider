package composition

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestReconcilePathNeverUpgradesUnconditionally guards krateo-core-provider#175.
//
// A drifting composition wrote TWO helm revisions per reconcile cycle. Observe converges the
// cluster with hc.Reconcile — apply-if-changed, so it writes a revision only when it actually
// mutated something — and then returns ResourceUpToDate:false, which makes the runtime dispatch
// Update. Update performed an UNCONDITIONAL hc.Upgrade, writing a second revision for drift the
// first call had already healed.
//
// Measured on krateo-057: strictly bimodal pairs, ~3s apart within a pair against a 60s cadence.
// git-provider reached 62,783 revisions over 28 days; cloud-meetup-turin sat at 99.8% of the
// theoretical 2.00/min ceiling that doubling implies.
//
// The invariant is therefore: on the RECONCILE path — Observe and Update, the two the runtime
// calls every cycle — convergence goes through Reconcile, never through an unconditional Upgrade.
// Reconcile is idempotent, so whichever of the two runs second finds nothing to do.
//
// Create is deliberately exempt and is NOT checked here. It runs once per composition rather than
// per cycle, and its Upgrade is the legitimate "a release already exists" branch of an install.
//
// Asserted on the source rather than behaviourally, following TestEveryHelmClientIsClosed in this
// package, and for the same reason: the helm client is built inline from h.kubeconfig at six call
// sites with no injection seam, so the choice of verb cannot be observed without a live apiserver
// and a real chart. This guard catches the regression that matters — someone restoring the
// Upgrade — which is exactly how the defect would come back.
//
// What it does NOT prove: that Reconcile writes no revision when the cluster already matches. That
// is plumbing's contract and plumbing's tests. The split is deliberate — this owns the call, that
// owns the behaviour.
func TestReconcilePathNeverUpgradesUnconditionally(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "composition.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing composition.go: %v", err)
	}

	// The two the runtime calls on every reconcile cycle.
	onTheReconcilePath := map[string]bool{"Observe": true, "Update": true}

	found := helmVerbsIn(fset, file, onTheReconcilePath)

	// The guard must have looked. A walk that located neither function, or located them and saw no
	// helm verb at all, would report success having examined nothing — the failure mode this whole
	// class of guard exists to catch, so it must not have it itself.
	for name := range onTheReconcilePath {
		if _, ok := found[name]; !ok {
			t.Fatalf("did not find func (h *handler) %s in composition.go — this guard is not "+
				"looking at the right file and proves nothing", name)
		}
	}

	for _, name := range []string{"Observe", "Update"} {
		var upgrades, reconciles int
		for _, c := range found[name] {
			switch c.verb {
			case "Upgrade":
				upgrades++
				t.Errorf("%s calls hc.Upgrade at %s. On the reconcile path convergence must go "+
					"through hc.Reconcile: an unconditional Upgrade writes a revision for drift the "+
					"other phase has already healed, which is #175 (62,783 revisions on one release).",
					name, c.pos)
			case "Reconcile":
				reconciles++
			}
		}
		if reconciles == 0 && upgrades == 0 {
			t.Errorf("%s calls neither hc.Reconcile nor hc.Upgrade; if convergence moved to a third "+
				"verb this guard no longer covers the path it claims to", name)
		}
		if reconciles == 0 && upgrades > 0 {
			t.Errorf("%s converges without hc.Reconcile", name)
		}
	}
}

// Create keeps its Upgrade, and that is deliberate rather than an oversight. Pinned so the
// exemption above is a recorded decision: if Create ever moves onto the per-cycle path, the
// reasoning for exempting it stops holding and this failing is the prompt to revisit it.
func TestCreateStillUsesUpgrade(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "composition.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing composition.go: %v", err)
	}

	var sawCreate, sawUpgrade bool
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || fn.Name.Name != "Create" {
			continue
		}
		sawCreate = true
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			ce, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := ce.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Upgrade" {
				if recv, ok := sel.X.(*ast.Ident); ok && recv.Name == "hc" {
					sawUpgrade = true
				}
			}
			return true
		})
	}

	if !sawCreate {
		t.Fatal("did not find func (h *handler) Create — this guard is not looking at the right file")
	}
	if !sawUpgrade {
		t.Error("Create no longer calls hc.Upgrade. Not necessarily wrong, but the #175 guard " +
			"exempts Create on the grounds that its Upgrade is the install-time 'release already " +
			"exists' branch; if that is gone, re-read whether the exemption still makes sense.")
	}
}

// helmVerb is one hc.<verb>(...) call site found inside a named method.
type helmVerb struct {
	verb string
	pos  token.Position
}

// helmVerbsIn returns the hc.Upgrade / hc.Reconcile call sites inside each named method of file.
// A method that exists but contains no such call maps to a nil slice, which is how the callers
// tell "found the function, it has no helm verb" from "did not find the function at all".
//
// Separated from the guard so the DETECTOR can be tested against sources that violate the rule.
// Mutating composition.go to prove the guard fires does not work here: hc.Upgrade returns a
// *Release where hc.Reconcile returns a *ReconcileResult, so the obvious mutant does not compile
// and the check comes back inconclusive rather than red. A detector tested only against a tree
// that satisfies it has never been shown to fire at all.
func helmVerbsIn(fset *token.FileSet, file *ast.File, want map[string]bool) map[string][]helmVerb {
	found := map[string][]helmVerb{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || !want[fn.Name.Name] {
			continue
		}
		name := fn.Name.Name
		if _, seen := found[name]; !seen {
			found[name] = nil
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			ce, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := ce.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			recv, ok := sel.X.(*ast.Ident)
			if !ok || recv.Name != "hc" {
				return true
			}
			switch sel.Sel.Name {
			case "Upgrade", "Reconcile":
				found[name] = append(found[name], helmVerb{verb: sel.Sel.Name, pos: fset.Position(ce.Pos())})
			}
			return true
		})
	}
	return found
}

// The detector must FIRE. Run it over sources that violate the rule and over one that satisfies
// it, so the guard above is known to be capable of failing rather than merely observed passing.
func TestHelmVerbsIn_DetectsAnUpgradeOnTheReconcilePath(t *testing.T) {
	const violating = `package p
type handler struct{}
func (h *handler) Update() { hc.Upgrade(nil) }
func (h *handler) Observe() { hc.Reconcile(nil) }
func (h *handler) Create() { hc.Upgrade(nil) }
`
	const compliant = `package p
type handler struct{}
func (h *handler) Update() { hc.Reconcile(nil) }
func (h *handler) Observe() { hc.Reconcile(nil) }
func (h *handler) Create() { hc.Upgrade(nil) }
`
	path := map[string]bool{"Observe": true, "Update": true}

	parse := func(src string) (*token.FileSet, *ast.File) {
		fs := token.NewFileSet()
		f, err := parser.ParseFile(fs, "x.go", src, 0)
		if err != nil {
			t.Fatalf("parsing fixture: %v", err)
		}
		return fs, f
	}

	fs, f := parse(violating)
	got := helmVerbsIn(fs, f, path)
	var upgrades int
	for _, c := range got["Update"] {
		if c.verb == "Upgrade" {
			upgrades++
		}
	}
	if upgrades != 1 {
		t.Fatalf("detector found %d Upgrades in a violating Update, want 1 — it cannot fail, so "+
			"the guard over composition.go proves nothing", upgrades)
	}
	// Create's Upgrade must NOT be collected: the rule exempts it, and a detector that swept it up
	// would fire forever and be switched off.
	if _, ok := got["Create"]; ok {
		t.Error("detector looked inside Create, which the rule deliberately exempts")
	}

	// Control arm: the same detector over a compliant source must find no Upgrade anywhere on the
	// path, or the assertion above is satisfied by something other than the violation.
	fs, f = parse(compliant)
	for name, calls := range helmVerbsIn(fs, f, path) {
		for _, c := range calls {
			if c.verb == "Upgrade" {
				t.Errorf("detector reported an Upgrade in %s of a compliant source", name)
			}
		}
	}
}

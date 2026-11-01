package composition

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// stampReady's default arm used to be setAvaibleStatus. A reason added to resolveReady without a
// matching case there did not fail loudly and did not stamp nothing — it reported the composition
// HEALTHY. That is the #165 failure mode arriving through the routing that implements the fix for
// it, and it is invisible: "not in a recognised bad branch" read as evidence of good.
//
// Prompted by the portal hitting the identical shape one layer up — its cluster statusColor chain
// tested notReady, then unknown, then fell through to GREEN, so a cluster whose only non-ready row
// was converging was presented as healthy.

// An unknown reason must ERROR rather than stamp anything. The error is what makes the mistake
// visible at the moment it is made.
func TestStampReady_UnknownReasonErrorsRatherThanReportingHealthy(t *testing.T) {
	mg := &unstructured.Unstructured{Object: map[string]any{}}

	err := stampReady(mg, readyOutcome{reason: "SomeFutureReason", message: "x"}, true)
	if err == nil {
		t.Fatal("an unrecognised reason was accepted; before this change it stamped Available, " +
			"which reports a composition healthy on the strength of a reason nobody handled")
	}
	if !strings.Contains(err.Error(), "SomeFutureReason") {
		t.Errorf("the error does not name the offending reason: %v", err)
	}
	// And it must not have written a condition on the way out — erroring AND stamping would be
	// worse than either, because the object would carry a verdict the caller believes failed.
	if conds, found, _ := unstructured.NestedSlice(mg.Object, "status", "conditions"); found && len(conds) > 0 {
		t.Errorf("a condition was stamped despite the error: %v", conds)
	}
}

// The four real reasons must still route correctly — the control arm. A change that made
// stampReady reject everything would satisfy the test above on its own.
func TestStampReady_RoutesEveryRealReason(t *testing.T) {
	for _, c := range []struct{ reason, wantReason, wantStatus string }{
		{"Available", "Available", "True"},
		{"Creating", "Creating", "False"},
		{"Unavailable", "Unavailable", "False"},
		{"ZeroEvidence", "ZeroEvidence", "Unknown"},
	} {
		t.Run(c.reason, func(t *testing.T) {
			mg := &unstructured.Unstructured{Object: map[string]any{}}
			if err := stampReady(mg, readyOutcome{reason: c.reason, message: "m"}, true); err != nil {
				t.Fatalf("stampReady(%q): %v", c.reason, err)
			}
			status, reason, _ := readyCondition(t, mg)
			if reason != c.wantReason || status != c.wantStatus {
				t.Errorf("got %s/%s, want %s/%s", status, reason, c.wantStatus, c.wantReason)
			}
		})
	}
}

// COMPLETENESS, and the half the two tests above cannot cover: they assert that the reasons I
// thought of are handled. This asserts that every reason resolveReady can actually PRODUCE is one
// of them — so adding a `readyOutcome{reason: "Something"}` to childhealth.go without a case in
// stampReady fails here rather than in production.
//
// Source-level because the property is about the set of reasons the code can emit, which is not
// observable by calling resolveReady without enumerating every input that reaches each return.
// Unlike a guard that asserts an identifier is mentioned, this compares two sets and fails on a
// difference in either direction, so it cannot pass by accident.
func TestEveryReasonResolveReadyProducesIsHandledByStampReady(t *testing.T) {
	fset := token.NewFileSet()

	produced := map[string]bool{}
	// Every reason expression the walk could not turn into a string, on either side. A guard that
	// silently ignores what it cannot read reports agreement it never checked.
	var unresolved []string

	// String constants declared in the package, so a reason or case written as a named constant
	// resolves rather than vanishing.
	constStrings, err := packageStringConsts(fset, "childhealth.go", "support.go")
	if err != nil {
		t.Fatalf("collecting package string constants: %v", err)
	}

	ch, err := parser.ParseFile(fset, "childhealth.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing childhealth.go: %v", err)
	}
	ast.Inspect(ch, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		if id, ok := lit.Type.(*ast.Ident); !ok || id.Name != "readyOutcome" {
			return true
		}
		for _, el := range lit.Elts {
			kv, ok := el.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if k, ok := kv.Key.(*ast.Ident); !ok || k.Name != "reason" {
				continue
			}
			// Resolve the reason expression, and RECORD WHAT CANNOT BE RESOLVED rather than
			// skipping it. Skipping silently is how this guard went blind: a reason written as
			// `reason: availableReason(...)` is a CallExpr, not a BasicLit, so the walk found
			// nothing and the test still passed on the remaining literals. The guard stayed
			// non-vacuous overall while being blind to the one new entry — which is worse than
			// vacuous, because the calibration below cannot see it either.
			switch v := kv.Value.(type) {
			case *ast.BasicLit:
				if v.Kind == token.STRING {
					if s, err := strconv.Unquote(v.Value); err == nil {
						produced[s] = true
					}
				}
			case *ast.Ident:
				if s, ok := constStrings[v.Name]; ok {
					produced[s] = true
				} else {
					unresolved = append(unresolved, "producer: identifier "+v.Name)
				}
			case *ast.CallExpr:
				// A reason computed by a package-local function or method: resolve it by reading THAT
				// function's return statements, rather than demanding the author maintain a list
				// of what it can return. A hand-kept list is the thing that goes stale, which is
				// the failure this guard exists to prevent.
				rs, ok := returnedStrings(fset, v, constStrings, "childhealth.go", "support.go")
				if !ok {
					unresolved = append(unresolved, "producer: "+exprKind(kv.Value))
					break
				}
				for _, s := range rs {
					produced[s] = true
				}
			default:
				unresolved = append(unresolved, "producer: "+exprKind(kv.Value))
			}
		}
		return true
	})

	handled := map[string]bool{}
	su, err := parser.ParseFile(fset, "support.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing support.go: %v", err)
	}
	for _, decl := range su.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "stampReady" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			cc, ok := n.(*ast.CaseClause)
			if !ok {
				return true
			}
			for _, e := range cc.List {
				switch v := e.(type) {
				case *ast.BasicLit:
					if v.Kind == token.STRING {
						if s, err := strconv.Unquote(v.Value); err == nil {
							handled[s] = true
						}
					}
				case *ast.Ident:
					// `case ReasonAvailablePartialCoverage:` is an Ident, not a literal. The old
					// walk skipped it, so adding a constant-named case made the handler side blind
					// at the same moment the producer side went blind.
					if s, ok := constStrings[v.Name]; ok {
						handled[s] = true
					} else {
						unresolved = append(unresolved, "stampReady case: identifier "+v.Name)
					}
				default:
					unresolved = append(unresolved, "stampReady case: "+exprKind(e))
				}
			}
			return true
		})
	}

	// Both sides must be non-empty, or this test is comparing nothing with nothing and passes
	// while asserting zero. This is the control the AST guard in #232 lacked.
	if len(produced) == 0 {
		t.Fatal("found no readyOutcome{reason: \"...\"} literals in childhealth.go — the walk is " +
			"not finding what it thinks it is, so a mismatch could never be detected")
	}
	if len(handled) == 0 {
		t.Fatal("found no case literals in stampReady — same problem, other side")
	}
	// LOUD, not silent. An unreadable reason expression is the failure mode that let this guard
	// miss AvailablePartialCoverage: both sides skipped what they could not parse and the
	// non-empty checks above still passed on the older literals.
	if len(unresolved) > 0 {
		t.Fatalf("the walk could not resolve %d reason expression(s), so it cannot claim the two "+
			"sides agree: %v\n\nEither write the reason as a string literal or a package-level "+
			"string constant, or extend packageStringConsts to see it. Do NOT make this skip "+
			"quietly again.", len(unresolved), unresolved)
	}

	for r := range produced {
		if !handled[r] {
			t.Errorf("resolveReady can return reason %q but stampReady has no case for it. Before "+
				"this was an error it fell to `default: setAvaibleStatus`, i.e. the composition "+
				"would be reported HEALTHY on a reason nobody handled.", r)
		}
	}
	// The other direction is a warning sign rather than a fault: a case for a reason nothing emits
	// is dead code, and dead routing is how #232 happened.
	for r := range handled {
		if !produced[r] {
			t.Errorf("stampReady handles reason %q that resolveReady never produces — dead routing, "+
				"which is exactly how the gracefully-paused condition stayed unreachable (#232)", r)
		}
	}
}

// packageStringConsts collects package-level `const X = "..."` declarations from the named files, so
// a reason or a case written as a named constant resolves to its value instead of being skipped.
//
// It exists because this guard went blind the moment a reason stopped being an inline literal.
func packageStringConsts(fset *token.FileSet, files ...string) (map[string]string, error) {
	out := map[string]string{}
	for _, f := range files {
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			return nil, err
		}
		for _, d := range file.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, s := range gd.Specs {
				vs, ok := s.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					if i >= len(vs.Values) {
						continue
					}
					bl, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || bl.Kind != token.STRING {
						continue
					}
					if v, err := strconv.Unquote(bl.Value); err == nil {
						out[name.Name] = v
					}
				}
			}
		}
	}
	return out, nil
}

// exprKind names an expression type for an error message, so a failure says WHAT it could not read
// rather than only that it could not.
func exprKind(e ast.Expr) string {
	switch e.(type) {
	case *ast.CallExpr:
		return "a function call (the guard cannot enumerate what it returns)"
	case *ast.SelectorExpr:
		return "a qualified identifier from another package"
	case *ast.BinaryExpr:
		return "a concatenation or other binary expression"
	default:
		return fmt.Sprintf("an unhandled expression of type %T", e)
	}
}

// returnedStrings resolves a call to a package-local function into the set of string values it can
// return, by parsing that function's return statements.
//
// This exists so a computed reason does not need a hand-maintained list of its possible values. A
// list kept by hand beside the code is exactly what goes stale, and a guard whose correctness
// depends on someone remembering to update a second place is the defect it is meant to catch.
//
// Returns ok=false when the call cannot be resolved this way — a method, another package's
// function, or a body that returns something other than a literal or a known constant — so the
// caller reports it as unresolved rather than treating an empty result as "returns nothing".
func returnedStrings(fset *token.FileSet, call *ast.CallExpr, consts map[string]string, files ...string) ([]string, bool) {
	// A plain function call, or a METHOD call on a package-local type. Both are resolvable by name
	// within these files; only a call into another package is not.
	//
	// The method case was a pre-existing blind spot this rewrite exposed: `reason:
	// p.notReadyReason()` has been in resolveReady all along, and the original literal-only walk
	// never saw it. It happens to return only "Creating" and "Unavailable", both handled — so the
	// hole was benign, and a third return added there would have slipped through silently.
	var name string
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		name = fun.Name
	case *ast.SelectorExpr:
		// x.Method(): resolve by the method name. A package-qualified call (pkg.Func) also lands
		// here and simply will not match a local declaration, which is the correct outcome.
		name = fun.Sel.Name
	default:
		return nil, false
	}
	id := &ast.Ident{Name: name}
	for _, f := range files {
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			return nil, false
		}
		for _, d := range file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Name.Name != id.Name || fn.Body == nil {
				continue
			}
			var out []string
			resolvable := true
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				ret, ok := n.(*ast.ReturnStmt)
				if !ok {
					return true
				}
				for _, r := range ret.Results {
					switch v := r.(type) {
					case *ast.BasicLit:
						if v.Kind == token.STRING {
							if s, err := strconv.Unquote(v.Value); err == nil {
								out = append(out, s)
							}
						}
					case *ast.Ident:
						if s, ok := consts[v.Name]; ok {
							out = append(out, s)
						} else {
							resolvable = false
						}
					default:
						resolvable = false
					}
				}
				return true
			})
			if !resolvable || len(out) == 0 {
				// An unreadable return, or a function that returns no strings at all: say so rather
				// than claiming to have enumerated it.
				return nil, false
			}
			return out, true
		}
	}
	return nil, false // no such function in the files searched
}

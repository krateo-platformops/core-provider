package composition

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/krateo-platformops/unstructured-runtime/pkg/tools"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// #276 defect 1. Observe's deletion guard was 252 lines upstream of the self-heal apply, so a
// reconcile that passed it before a delete was issued re-created the children helm was
// concurrently uninstalling — as orphans with no release annotation that nothing would remove.
//
// Measured on a0: six keystone Jobs created 16 SECONDS after the delete, carrying the OLD
// release's version labels, with creating field manager "controller" rather than "helm".

type recheckPluralizer struct{}

func (recheckPluralizer) GVKtoGVR(gvk schema.GroupVersionKind) (schema.GroupVersionResource, error) {
	return schema.GroupVersionResource{Group: gvk.Group, Version: gvk.Version, Resource: "fireworks"}, nil
}

type failingPluralizer struct{}

func (failingPluralizer) GVKtoGVR(schema.GroupVersionKind) (schema.GroupVersionResource, error) {
	return schema.GroupVersionResource{}, errors.New("no plural for this kind")
}

func recheckMG(deleting bool) *unstructured.Unstructured {
	mg := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "composition.krateo.io/v1-0-0", "kind": "Firework",
		"metadata": map[string]any{"name": "fw", "namespace": "krateo-system"},
	}}
	if deleting {
		now := metav1.Now()
		mg.SetDeletionTimestamp(&now)
	}
	return mg
}

func recheckOpts(t *testing.T, objs ...runtime.Object) tools.UpdateOptions {
	t.Helper()
	gvrToKind := map[schema.GroupVersionResource]string{
		{Group: "composition.krateo.io", Version: "v1-0-0", Resource: "fireworks"}: "FireworkList",
	}
	return tools.UpdateOptions{
		Pluralizer:    recheckPluralizer{},
		DynamicClient: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), gvrToKind, objs...),
	}
}

func TestIsBeingDeleted(t *testing.T) {
	t.Run("reports true when the LIVE object has a deletionTimestamp", func(t *testing.T) {
		// The case that matters: the in-memory mg does NOT have one, because this reconcile read it
		// before the delete. Only the live read can see it. A check against mg alone — which is
		// what the guard 252 lines upstream effectively is by this point — returns false here.
		stale := recheckMG(false)
		live := recheckMG(true)

		got, err := isBeingDeleted(context.Background(), stale, recheckOpts(t, live))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !got {
			t.Error("a composition deleted while this reconcile was in flight must be detected; " +
				"otherwise the self-heal re-creates the children the uninstall is removing (#276)")
		}
	})

	t.Run("reports false for a live object that is NOT deleting", func(t *testing.T) {
		// CONTROL. Without this, a function that always returned true would satisfy the arm above
		// and would disable the self-heal entirely — a far worse defect than the one being fixed.
		mg := recheckMG(false)
		got, err := isBeingDeleted(context.Background(), mg, recheckOpts(t, mg))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got {
			t.Error("control: a healthy composition reported as deleting; the self-heal would never run")
		}
	})

	t.Run("a missing object is an error, not a false", func(t *testing.T) {
		// Returning (false, nil) for a NotFound would be the plausible-but-wrong reading: an object
		// that is gone is further along than deleting, and proceeding to self-heal it re-creates a
		// whole release. The caller decides what to do; this must not decide for it by omission.
		_, err := isBeingDeleted(context.Background(), recheckMG(false), recheckOpts(t))
		if err == nil {
			t.Error("a NotFound must surface as an error so the caller can decide, rather than " +
				"silently reading as 'not deleting'")
		} else if !apierrors.IsNotFound(errors.Unwrap(err)) && !strings.Contains(err.Error(), "not found") {
			t.Errorf("error should carry the NotFound cause, got %v", err)
		}
	})

	t.Run("a nil client is an error", func(t *testing.T) {
		if _, err := isBeingDeleted(context.Background(), recheckMG(false), tools.UpdateOptions{}); err == nil {
			t.Error("with no client there is no evidence either way, which must not read as 'not deleting'")
		}
	})

	t.Run("a GVR resolution failure is an error", func(t *testing.T) {
		opts := recheckOpts(t)
		opts.Pluralizer = failingPluralizer{}
		if _, err := isBeingDeleted(context.Background(), recheckMG(false), opts); err == nil {
			t.Error("an unresolvable plural must not read as 'not deleting'")
		}
	})
}

// THE ORDERING, which is the whole point of the change and is a POSITIONAL fact rather than a
// reachability one — so a source assertion is sound here where it would not be for #232's
// mention-versus-reachability case.
//
// The guard was always present; it was present 252 lines too early. A fix that re-added it
// upstream of the chart fetch would pass every behavioural test above and change nothing.
func TestDeletionRecheckSitsAdjacentToTheSelfHealApply(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "composition.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing composition.go: %v", err)
	}

	var observe *ast.FuncDecl
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == "Observe" && fn.Recv != nil {
			observe = fn
			break
		}
	}
	if observe == nil {
		t.Fatal("PREMISE: Observe not found in composition.go; this guard is asserting nothing")
	}

	var recheckLine, reconcileLine, chartFetchLine int
	ast.Inspect(observe.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		line := fset.Position(call.Pos()).Line
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			if fn.Name == "isBeingDeleted" && recheckLine == 0 {
				recheckLine = line
			}
		case *ast.SelectorExpr:
			if fn.Sel.Name == "Reconcile" && reconcileLine == 0 {
				reconcileLine = line
			}
			if fn.Sel.Name == "Get" && chartFetchLine == 0 {
				chartFetchLine = line
			}
		}
		return true
	})

	// Calibration: all three must be found, or the ordering assertions below compare zeros.
	if recheckLine == 0 {
		t.Fatal("isBeingDeleted is not called in Observe at all. The guard 252 lines upstream " +
			"cannot see a delete issued mid-reconcile, which is #276 defect 1")
	}
	if reconcileLine == 0 || chartFetchLine == 0 {
		t.Fatalf("PREMISE: could not locate the landmarks (recheck=%d chartFetch=%d reconcile=%d); "+
			"the ordering assertion would be vacuous", recheckLine, chartFetchLine, reconcileLine)
	}

	if recheckLine >= reconcileLine {
		t.Errorf("isBeingDeleted is at line %d and the self-heal Reconcile at %d — the guard must "+
			"come BEFORE the apply it protects", recheckLine, reconcileLine)
	}
	if recheckLine <= chartFetchLine {
		t.Errorf("isBeingDeleted is at line %d, upstream of the chart fetch at %d. That is the "+
			"defect, not the fix: the whole point is that the guard sits ADJACENT to the apply, "+
			"with no long-running work between them", recheckLine, chartFetchLine)
	}
	t.Logf("ordering: chartFetch=%d < recheck=%d < reconcile=%d (gap to apply: %d lines)",
		chartFetchLine, recheckLine, reconcileLine, reconcileLine-recheckLine)
}

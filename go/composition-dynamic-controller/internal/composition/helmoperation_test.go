package composition

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/krateo-platformops/unstructured-runtime/pkg/logging"
	unstructuredtools "github.com/krateo-platformops/unstructured-runtime/pkg/tools/unstructured"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clienttesting "k8s.io/client-go/testing"
)

// unboundedHelmConfigs returns, for one Go source, every helmconfig.{Action,Rollback,Uninstall}Config
// composite literal that does NOT set Timeout to helmOperationTimeout, as "Type@line". These are the
// configs through which every helm install, upgrade, rollback and uninstall reaches helm, and helm
// treats a zero Timeout as no deadline at all on hooks (#410). InstallConfig and UpgradeConfig carry
// their timeout in the embedded ActionConfig, which is why ActionConfig is the one checked for them.
func unboundedHelmConfigs(t *testing.T, src []byte) (unbounded []string, seen int) {
	t.Helper()
	fs := token.NewFileSet()
	f, err := parser.ParseFile(fs, "src.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	ast.Inspect(f, func(n ast.Node) bool {
		cl, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := cl.Type.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if x, ok := sel.X.(*ast.Ident); !ok || x.Name != "helmconfig" {
			return true
		}
		switch sel.Sel.Name {
		case "ActionConfig", "RollbackConfig", "UninstallConfig":
		default:
			return true
		}
		seen++
		bounded := false
		for _, el := range cl.Elts {
			kv, ok := el.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Timeout" {
				if v, ok := kv.Value.(*ast.Ident); ok && v.Name == "helmOperationTimeout" {
					bounded = true
				}
			}
		}
		if !bounded {
			unbounded = append(unbounded, sel.Sel.Name+"@"+itoa(fs.Position(cl.Pos()).Line))
		}
		return true
	})
	return unbounded, seen
}

func itoa(i int) string { return strconv.Itoa(i) }

// #410. Every helm config literal in the handler is bounded.
func TestEveryHelmOperationIsBounded(t *testing.T) {
	src, err := os.ReadFile("composition.go")
	if err != nil {
		t.Fatal(err)
	}
	unbounded, seen := unboundedHelmConfigs(t, src)
	// Denominator: two rollbacks, one uninstall, and the ActionConfigs of Observe's reconcile,
	// Create's install/upgrade and Update's reconcile. Fewer means a site moved out of this file
	// and the guard no longer covers it.
	if seen != 6 {
		t.Errorf("found %d helm config literals in composition.go, want 6; a site moved and is no longer checked", seen)
	}
	if len(unbounded) > 0 {
		t.Errorf("helm configs without Timeout: helmOperationTimeout: %v. helm treats 0 as no deadline on hooks (#410)", unbounded)
	}
}

// The guard must fail on the defect it guards (control): the pre-#410 shape, no Timeout at all, and
// a Timeout set to something other than the bound.
func TestUnboundedHelmConfigsFiresOnTheDefect(t *testing.T) {
	src := []byte(`package composition
func f() {
	_ = &helmconfig.RollbackConfig{MaxHistory: 3}
	_ = &helmconfig.UninstallConfig{IgnoreNotFound: true, Timeout: 0}
	_ = &helmconfig.ActionConfig{TakeOwnership: true, Timeout: helmOperationTimeout}
	_ = &helmconfig.GetConfig{}
}`)
	unbounded, seen := unboundedHelmConfigs(t, src)
	if seen != 3 {
		t.Fatalf("seen=%d, want 3 (GetConfig is not an operation)", seen)
	}
	if len(unbounded) != 2 || !strings.HasPrefix(unbounded[0], "RollbackConfig@") || !strings.HasPrefix(unbounded[1], "UninstallConfig@") {
		t.Errorf("unbounded=%v, want the RollbackConfig and the Timeout: 0 UninstallConfig", unbounded)
	}
}

// The pending grace must exceed the bound, or a second cdc pod rolls back an operation still inside it.
func TestPendingGraceExceedsTheHelmBound(t *testing.T) {
	if helmOperationTimeout <= 0 {
		t.Fatalf("helmOperationTimeout = %s; zero is no bound", helmOperationTimeout)
	}
	if pendingOperationGrace <= helmOperationTimeout {
		t.Errorf("pendingOperationGrace %s <= helmOperationTimeout %s", pendingOperationGrace, helmOperationTimeout)
	}
}

func TestHelmOperationInProgressCondition(t *testing.T) {
	started := time.Date(2026, 11, 10, 6, 13, 54, 0, time.UTC)
	c := helmOperationInProgress("install", started, 15*time.Minute)
	if c.Reason != reasonHelmOperationInProgress || c.Status != metav1.ConditionFalse {
		t.Errorf("reason=%q status=%q, want %s False", c.Reason, c.Status, reasonHelmOperationInProgress)
	}
	for _, w := range []string{"helm install in progress", "2026-11-10T06:13:54Z", "15m0s"} {
		if !strings.Contains(c.Message, w) {
			t.Errorf("message %q lacks %q", c.Message, w)
		}
	}
}

// recordHelmOperation writes the condition and hands back the updated object; a lost race is returned
// so the runtime requeues; any other write failure lets the operation go ahead.
func TestRecordHelmOperation(t *testing.T) {
	ctx := context.Background()

	t.Run("writes the condition", func(t *testing.T) {
		dyn := srFakeDyn(srPortal("1", "x"))
		got, err := recordHelmOperation(ctx, srPortal("1", "x"), "uninstall", time.Now(), srOpts(dyn), logging.NewNopLogger())
		if err != nil {
			t.Fatal(err)
		}
		conds := unstructuredtools.GetConditions(got)
		found := false
		for _, c := range conds {
			if c.Reason == reasonHelmOperationInProgress && strings.Contains(c.Message, "helm uninstall") {
				found = true
			}
		}
		if !found {
			t.Errorf("conditions %v lack %s", conds, reasonHelmOperationInProgress)
		}
	})

	t.Run("a conflict is returned", func(t *testing.T) {
		dyn := srFakeDyn(srPortal("2", "newer"))
		dyn.PrependReactor("update", "portals", func(a clienttesting.Action) (bool, runtime.Object, error) {
			if ua, ok := a.(clienttesting.UpdateAction); ok && ua.GetSubresource() == "status" {
				return true, nil, apierrors.NewConflict(schema.GroupResource{Group: srGVR.Group, Resource: srGVR.Resource}, "portal", nil)
			}
			return false, nil, nil
		})
		if _, err := recordHelmOperation(ctx, srPortal("1", "stale"), "install", time.Now(), srOpts(dyn), logging.NewNopLogger()); err == nil {
			t.Error("a lost race must be returned, so the runtime requeues and the condition is written")
		}
	})

	t.Run("any other failure lets the operation proceed", func(t *testing.T) {
		dyn := srFakeDyn(srPortal("1", "x"))
		dyn.PrependReactor("update", "portals", func(a clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewInternalError(os.ErrDeadlineExceeded)
		})
		if _, err := recordHelmOperation(ctx, srPortal("1", "x"), "install", time.Now(), srOpts(dyn), logging.NewNopLogger()); err != nil {
			t.Errorf("a report must not gate the operation; got %v", err)
		}
	})
}

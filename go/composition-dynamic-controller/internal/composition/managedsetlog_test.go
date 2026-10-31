package composition

import (
	"context"
	"fmt"
	"strings"
	"testing"

	xcontext "github.com/krateo-platformops/unstructured-runtime/pkg/context"
	"github.com/krateo-platformops/unstructured-runtime/pkg/logging"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// #268. Nothing at default verbosity recorded that status.managed was rewritten, which is the
// first question asked about every defect in the reports-what-it-recorded family. The nearest line
// is log.Debug at composition.go:992 and the cdc runs at Info, so a peer grepping for it on a
// composition that had demonstrably just run Update got 0 — a structural zero indistinguishable
// from a true one.

// capturingLogger implements unstructured-runtime's logging.Logger. The first version of this
// test built a plumbing/context logger, which support.go does not read — xcontext.Logger falls
// back to a stderr logger when its key is absent, so the buffer stayed empty and the two SILENT
// arms below PASSED VACUOUSLY while the positive arms correctly failed. The calibration assertion
// in each silent arm exists so that cannot recur.
type capturingLogger struct{ lines *[]string }

func (c capturingLogger) Info(msg string, kv ...any) {
	// key=value, matching how an operator reads it, so an assertion on "previousTotal=1" is
	// asserting the shape of the real log line rather than of this stub.
	parts := []string{msg}
	for i := 0; i+1 < len(kv); i += 2 {
		parts = append(parts, fmt.Sprintf("%v=%v", kv[i], kv[i+1]))
	}
	*c.lines = append(*c.lines, strings.Join(parts, " "))
}

func (c capturingLogger) Debug(msg string, kv ...any)            {}
func (c capturingLogger) Warn(msg string, kv ...any)             {}
func (c capturingLogger) Error(err error, msg string, kv ...any) {}
func (c capturingLogger) WithValues(kv ...any) logging.Logger    { return c }
func (c capturingLogger) WithName(name string) logging.Logger    { return c }

func logCapture(t *testing.T) (context.Context, *[]string) {
	t.Helper()
	lines := &[]string{}
	ctx := xcontext.BuildContext(context.Background(), xcontext.WithLogger(capturingLogger{lines: lines}))

	// CALIBRATION: prove the capture reaches the code under test before any arm relies on its
	// silence. Without this, an arm asserting "nothing was logged" passes whenever the wiring is
	// wrong, which is exactly what happened on the first attempt.
	xcontext.Logger(ctx).Info("calibration")
	if len(*lines) != 1 {
		t.Fatalf("the logger context does not route to this test's capture (%d lines); every "+
			"silence assertion below would pass vacuously", len(*lines))
	}
	*lines = (*lines)[:0]
	return ctx, lines
}

func mgWithManaged(entries ...map[string]any) *unstructured.Unstructured {
	mg := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "composition.krateo.io/v1-0-0", "kind": "Fireworks",
		"metadata": map[string]any{"name": "f", "namespace": "ns"},
	}}
	if entries != nil {
		list := make([]any, 0, len(entries))
		for _, e := range entries {
			list = append(list, e)
		}
		_ = unstructured.SetNestedSlice(mg.Object, list, "status", "managed")
	}
	return mg
}

func mgChild(res, name string) map[string]any {
	return map[string]any{"apiVersion": "apps/v1", "resource": res, "name": name, "namespace": "ns"}
}

func TestLogManagedSetChange(t *testing.T) {
	t.Run("a rename is reported with both names", func(t *testing.T) {
		// THE #261 SHAPE: the set changes while the COUNT stays the same. A log keyed on length
		// would say nothing here, which is the case an operator most needs named.
		prev := []any{mgChild("deployments", "kept"), mgChild("nodepowers", "blade01-power-on-from-on")}
		mg := mgWithManaged(mgChild("deployments", "kept"), mgChild("nodepowers", "blade01-power"))

		ctx, lines := logCapture(t)
		logManagedSetChange(ctx, prev, mg)
		out := strings.Join(*lines, "\n")

		if !strings.Contains(out, "Composition managed set changed") {
			t.Fatalf("a rename at constant count must be reported; got %q", out)
		}
		if !strings.Contains(out, "blade01-power-on-from-on") {
			t.Error("the REMOVED name is missing; without it an operator cannot tell which entry went")
		}
		if !strings.Contains(out, "blade01-power") {
			t.Error("the ADDED name is missing")
		}
	})

	t.Run("a two-phase render catching up is reported", func(t *testing.T) {
		// THE #273 SHAPE: the set GROWS. A chart whose render depends on cluster state (helm
		// lookup) reaches its full inventory over several reconciles.
		prev := []any{mgChild("secrets", "pki")}
		mg := mgWithManaged(mgChild("secrets", "pki"), mgChild("baremetalhosts", "blade01"),
			mgChild("baremetalhosts", "blade02"))

		ctx, lines := logCapture(t)
		logManagedSetChange(ctx, prev, mg)
		out := strings.Join(*lines, "\n")
		if !strings.Contains(out, "blade01") || !strings.Contains(out, "blade02") {
			t.Errorf("both new children must be named; got %q", out)
		}
		if !strings.Contains(out, "previousTotal=1") {
			t.Errorf("the previous total must be reported so growth is visible; got %q", out)
		}
	})

	// THE LOAD-BEARING ARM. Silence on an unchanged set is what makes this affordable: an Info
	// line on a per-reconcile path across 68 controllers is exactly what #175 trimmed for cost.
	t.Run("SILENT when the set is unchanged", func(t *testing.T) {
		same := []any{mgChild("deployments", "a"), mgChild("services", "b")}
		mg := mgWithManaged(mgChild("deployments", "a"), mgChild("services", "b"))

		ctx, lines := logCapture(t)
		logManagedSetChange(ctx, same, mg)
		if out := strings.Join(*lines, "\n"); out != "" {
			t.Errorf("an unchanged set must emit NOTHING, or every reconcile of every composition "+
				"logs at Info forever; got %q", out)
		}
	})

	t.Run("SILENT when the set is unchanged but ordered differently", func(t *testing.T) {
		// populateManagedResources has no ordering guarantee, so a set compared positionally would
		// report a spurious change on every reconcile — which is the same cost failure as above,
		// arrived at by accident rather than by design.
		prev := []any{mgChild("services", "b"), mgChild("deployments", "a")}
		mg := mgWithManaged(mgChild("deployments", "a"), mgChild("services", "b"))

		ctx, lines := logCapture(t)
		logManagedSetChange(ctx, prev, mg)
		if out := strings.Join(*lines, "\n"); out != "" {
			t.Errorf("reordering is not a change; got %q", out)
		}
	})

	t.Run("the first recording is reported as pure addition", func(t *testing.T) {
		mg := mgWithManaged(mgChild("deployments", "a"))
		ctx, lines := logCapture(t)
		logManagedSetChange(ctx, nil, mg)
		out := strings.Join(*lines, "\n")
		if !strings.Contains(out, "previousTotal=0") {
			t.Errorf("a first recording must show previousTotal=0, distinguishing it from a change "+
				"to an existing inventory; got %q", out)
		}
	})
}

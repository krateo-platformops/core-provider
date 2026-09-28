package composition

import (
	"errors"
	"strings"
	"testing"

	"github.com/krateo-platformops/unstructured-runtime/pkg/logging"
)

// recordingLogger captures Info calls so the breadcrumbs can be asserted.
type recordingLogger struct{ infos []string }

func (l *recordingLogger) Info(msg string, kv ...any) {
	parts := []string{msg}
	for i := 0; i+1 < len(kv); i += 2 {
		parts = append(parts, toStr(kv[i])+"="+toStr(kv[i+1]))
	}
	l.infos = append(l.infos, strings.Join(parts, " "))
}
func (l *recordingLogger) Debug(string, ...any)             {}
func (l *recordingLogger) Warn(string, ...any)              {}
func (l *recordingLogger) Error(error, string, ...any)      {}
func (l *recordingLogger) WithValues(...any) logging.Logger { return l }
func (l *recordingLogger) WithName(string) logging.Logger   { return l }

func toStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return "?"
}

func (l *recordingLogger) joined() string { return strings.Join(l.infos, "\n") }

// The regression for #69.
//
// A composition Update performing a real helm operation logged NOTHING at INFO for the whole
// operation — observed live at ~27 minutes. From outside, that is indistinguishable from a stalled
// controller, and it invites the operator reactions the platform's self-healing design forbids.
func TestHelmPhaseLogsBothEdges(t *testing.T) {
	log := &recordingLogger{}

	out, err := helmPhase(log, "reconcile", "my-release", func() (string, error) {
		return "done", nil
	})
	if err != nil || out != "done" {
		t.Fatalf("helmPhase must pass the result through unchanged: got %q, %v", out, err)
	}

	if len(log.infos) != 2 {
		t.Fatalf("expected exactly two INFO breadcrumbs (start and end), got %d: %s", len(log.infos), log.joined())
	}

	// A start line alone leaves the same ambiguity one step later — "it started 20 minutes ago"
	// does not say whether it is still running.
	if !strings.Contains(log.infos[0], "starting") {
		t.Errorf("first breadcrumb must open the window, got: %s", log.infos[0])
	}
	// A completion line alone is invisible for the entire window that matters.
	if !strings.Contains(log.infos[1], "finished") {
		t.Errorf("second breadcrumb must close it, got: %s", log.infos[1])
	}

	// Both must identify WHICH operation on WHICH release — a controller reconciling several
	// compositions produces interleaved breadcrumbs, and an unattributed "starting" is noise.
	for i, line := range log.infos {
		if !strings.Contains(line, "phase=reconcile") || !strings.Contains(line, "release=my-release") {
			t.Errorf("breadcrumb %d must name phase and release, got: %s", i, line)
		}
	}
	// Elapsed is the number that distinguishes "slow but progressing" from "hung".
	if !strings.Contains(log.infos[1], "elapsed=") {
		t.Errorf("the closing breadcrumb must report elapsed, got: %s", log.infos[1])
	}
}

// A failure must still CLOSE the window it opened. An operator reading the log should never find a
// "starting" with no end — that is the exact ambiguity this fix exists to remove, and a failure
// that logs nothing reintroduces it.
func TestHelmPhaseClosesTheWindowOnFailure(t *testing.T) {
	log := &recordingLogger{}
	boom := errors.New("chart pull failed")

	_, err := helmPhase(log, "install", "my-release", func() (string, error) {
		return "", boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("the original error must propagate unchanged, got: %v", err)
	}

	if len(log.infos) != 2 {
		t.Fatalf("a failed operation must still log both edges, got %d: %s", len(log.infos), log.joined())
	}
	if !strings.Contains(log.infos[1], "failed") {
		t.Errorf("the closing breadcrumb must say it failed, got: %s", log.infos[1])
	}
	if !strings.Contains(log.infos[1], "elapsed=") {
		t.Errorf("elapsed matters on failure too — it separates a fast rejection from a long timeout, got: %s", log.infos[1])
	}
}

// helmPhaseErr is the error-only shape; it must behave identically.
func TestHelmPhaseErrLogsBothEdges(t *testing.T) {
	log := &recordingLogger{}

	if err := helmPhaseErr(log, "uninstall", "my-release", func() error { return nil }); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(log.infos) != 2 {
		t.Fatalf("expected two breadcrumbs, got %d: %s", len(log.infos), log.joined())
	}
	if !strings.Contains(log.infos[0], "phase=uninstall") {
		t.Errorf("phase must be carried through, got: %s", log.infos[0])
	}
}

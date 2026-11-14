package composition

import (
	"context"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// markWrote records WHAT a pass wrote on the active span, so observability can count writes on a
// non-joined act, which opens no step span. One event per call, carrying the kind.
func TestMarkWrote_RecordsTheWriteKind(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	ctx, span := tp.Tracer("test").Start(context.Background(), "reconcile")
	markWrote(ctx, writeHelmReconcile)
	span.End()

	ended := rec.Ended()
	if len(ended) != 1 {
		t.Fatalf("recorded %d spans, want 1", len(ended))
	}
	if !hasWriteEventOn(ended[0], writeHelmReconcile) {
		t.Fatalf("the span must carry a %q event with %s=%s; events: %v", writeEvent, attrWrite, writeHelmReconcile, ended[0].Events())
	}
	if hasWriteEventOn(ended[0], writeRollback) {
		t.Error("only the kind passed may be recorded")
	}
	// Outside a reconcile pass and with no span, both halves are no-ops.
	markWrote(context.Background(), writeDerivedRBAC)
}

func hasWriteEventOn(s sdktrace.ReadOnlySpan, kind string) bool {
	for _, e := range s.Events() {
		if e.Name != writeEvent {
			continue
		}
		for _, kv := range e.Attributes {
			if kv.Key == attrWrite && kv.Value.AsString() == kind {
				return true
			}
		}
	}
	return false
}

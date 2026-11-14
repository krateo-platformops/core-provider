package composition

import (
	"context"

	"github.com/krateo-platformops/unstructured-runtime/pkg/controller"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// Write kinds recorded on the reconcile trace by markWrote. A CLOSED set, consumed by
// observability to count what a pass wrote: a non-joined act (every resync, and every Create,
// Update or Delete that joins no upstream trace) opens no step span, so without this event an
// acted root has nothing in its trace that says what was written. Observe records derived_rbac,
// rollback and helm_reconcile; Update records helm_reconcile when its reconcile changed the
// cluster; Create records helm_install or helm_upgrade; Delete records helm_uninstall.
const (
	writeDerivedRBAC   = "derived_rbac"
	writeRollback      = "rollback"
	writeHelmReconcile = "helm_reconcile"
	writeHelmInstall   = "helm_install"
	writeHelmUpgrade   = "helm_upgrade"
	writeHelmUninstall = "helm_uninstall"

	// writeEvent and attrWrite name the span event and its attribute.
	writeEvent = "write"
	attrWrite  = attribute.Key("krateo.write")
)

// markWrote marks this pass acted (Diego, 2026-11-14: "mark only on a real write") and records
// WHAT was written as a "write" event, attribute krateo.write=kind, on the active span: the
// reconcile root for an act that joined nothing, the act step for one that joined the upstream.
// With tracing off both are no-ops.
func markWrote(ctx context.Context, kind string) {
	controller.MarkActed(ctx)
	trace.SpanFromContext(ctx).AddEvent(writeEvent, trace.WithAttributes(attrWrite.String(kind)))
}

// Package telemetry holds core-provider's tracer accessor and span helpers.
//
// The trace PIPELINE is provider-runtime's since v1.14 (telemetry.Setup with TracingEnabled; #395).
// This package used to build its own TracerProvider; two would race for the global, so it no longer
// does. core-provider is the engine/operator, so it carries no composition-id of its own.
package telemetry

import (
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const tracerName = "github.com/krateo-platformops/core-provider"

// Tracer returns core-provider's tracer on the global TracerProvider, which provider-runtime's
// telemetry.Setup installs when tracing is enabled (#395); a no-op tracer otherwise.
func Tracer() trace.Tracer { return otel.Tracer(tracerName) }

// RecordError marks the span failed when err != nil; no-op otherwise.
func RecordError(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
}

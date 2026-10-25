package composition

import (
	"context"
	"testing"

	runtimeMeta "github.com/krateo-platformops/unstructured-runtime/pkg/meta"
	"github.com/krateo-platformops/unstructured-runtime/pkg/telemetry"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestTraceparentStampingWithTracingDisabled pins the behaviour the comment above the
// InjectTraceparent call in Observe describes, because an earlier version of that comment said
// "No-op when tracing is off" and a rollout was planned on it.
//
// It is NOT a no-op. telemetry.SetupTracing installs the W3C propagator unconditionally, BEFORE
// its disabled return, so an inbound trace is honoured regardless of export config;
// StartReconcileSpan extracts the annotation into ctx either way; and OTel's no-op tracer
// preserves a valid REMOTE span context. So whether a child gets stamped depends on whether the
// object already carries a traceparent, not on whether tracing is enabled.
//
// The test drives the REAL SetupTracing and the REAL InjectTraceparent rather than reconstructing
// the propagator setup, so it cannot agree with a model of the code that has drifted from it. The
// no-inbound case is the control arm: if stamping were genuinely gated on the export config, both
// arms would be empty and the test could not tell the two explanations apart.
func TestTraceparentStampingWithTracingDisabled(t *testing.T) {
	const inbound = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

	shutdown, err := telemetry.SetupTracing(context.Background(), nil, telemetry.Config{TracingEnabled: false})
	if err != nil {
		t.Fatalf("SetupTracing(disabled): %v", err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })

	cases := []struct {
		name       string
		annotation string
		wantStamp  bool
	}{
		{
			// Control arm: the steady state of a fleet with tracing off everywhere. Nothing to
			// continue, so nothing is injected and no child is stamped.
			name:       "object carries no traceparent: nothing stamped",
			annotation: "",
			wantStamp:  false,
		},
		{
			// The case the old comment denied. One traceparent entering anywhere rides through a
			// controller with tracing DISABLED and onto its children — which are themselves
			// compositions, so it walks down the tree.
			name:       "object carries a traceparent: children ARE stamped despite tracing being off",
			annotation: inbound,
			wantStamp:  true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			obj := &unstructured.Unstructured{}
			obj.SetAPIVersion("composition.krateo.io/v1-0-0")
			obj.SetKind("Widget")
			obj.SetName("w")
			obj.SetNamespace("ns")
			if tc.annotation != "" {
				obj.SetAnnotations(map[string]string{runtimeMeta.AnnotationKeyTraceparent: tc.annotation})
			}

			ctx, span := telemetry.StartReconcileSpan(context.Background(), obj)
			defer span.End()

			carrier := map[string]string{}
			telemetry.InjectTraceparent(ctx, carrier)
			got := carrier[runtimeMeta.AnnotationKeyTraceparent]

			if (got != "") != tc.wantStamp {
				t.Fatalf("injected traceparent = %q, want stamped=%v\n"+
					"this is the claim the Observe comment makes about stamping with tracing disabled; "+
					"if the propagator wiring in unstructured-runtime changed, update BOTH", got, tc.wantStamp)
			}
			if tc.wantStamp && got != tc.annotation {
				t.Fatalf("stamped %q, want the inbound trace %q continued unchanged", got, tc.annotation)
			}
		})
	}
}

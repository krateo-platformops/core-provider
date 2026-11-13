package auditrec

import (
	"context"
	"testing"

	"github.com/krateo-platformops/audit"
	"github.com/krateo-platformops/audit/audittest"
	helmconfig "github.com/krateo-platformops/plumbing/helm"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

var cause = audit.CauseRef{
	Group: "composition.krateo.io", Version: "v1-2-0", Resource: "exampleapps",
	Namespace: "example-ns", Name: "example-composition",
	UID: "00000000-0000-4000-8000-0000000000c1", Generation: 4,
}

func verdict(change helmconfig.ObjectChange, verb string) helmconfig.ObjectVerdict {
	return helmconfig.ObjectVerdict{
		Group: "apps", Version: "v1", Resource: "deployments", Kind: "Deployment",
		Namespace: "example-ns", Name: "example-worker", UID: "11111111-2222-4333-8444-555555555555",
		Verb: verb, Change: change,
	}
}

// The decided mapping, one row per change kind plumbing reports.
func TestEventMapping(t *testing.T) {
	for _, tc := range []struct {
		change                       helmconfig.ObjectChange
		verb                         string
		wantOK                       bool
		wantVerb, wantMethod, wantOC string
		wantDenial                   string
		wantExcluded                 string
	}{
		{helmconfig.ChangeCreated, "create", true, "create", "POST", audit.OutcomeSuccess, "", ""},
		{helmconfig.ChangeCreateFailed, "create", true, "create", "POST", audit.OutcomeFailure, "", ""},
		{helmconfig.ChangeDeleted, "delete", true, "delete", "DELETE", audit.OutcomeSuccess, "", ""},
		{helmconfig.ChangeDeleteNotDone, "delete", true, "delete", "DELETE", audit.OutcomeFailure, "", ""},
		{helmconfig.ChangeDeleteUnknown, "delete", true, "delete", "DELETE", audit.OutcomeFailure, "", ""},
		{helmconfig.ChangeSpec, "patch", true, "patch", "PATCH", audit.OutcomeSuccess, "", ""},
		{helmconfig.ChangeSpec, "update", true, "update", "PUT", audit.OutcomeSuccess, "", ""},
		{helmconfig.ChangeCompareFailed, "patch", true, "patch", "PATCH", audit.OutcomeSuccess, "", ""},
		{helmconfig.ChangeUpdateFailed, "patch", true, "patch", "PATCH", audit.OutcomeFailure, "", ""},
		{helmconfig.ChangeKept, "delete", true, "delete", "", audit.OutcomeDenied, audit.DenialReasonKeptResourcePolicy, ""},
		{helmconfig.ChangeMetadataOnly, "patch", false, "", "", "", "", ExcludedMetadataOnly},
	} {
		t.Run(string(tc.change)+"/"+tc.verb, func(t *testing.T) {
			ev, ok, excluded := Event(ActionUpgrade, verdict(tc.change, tc.verb), cause, audit.PreviewFalse)
			if ok != tc.wantOK || excluded != tc.wantExcluded {
				t.Fatalf("ok=%v excluded=%q, want %v %q", ok, excluded, tc.wantOK, tc.wantExcluded)
			}
			if !ok {
				return
			}
			if ev.Verb != tc.wantVerb || ev.HTTPMethod != tc.wantMethod || ev.Outcome != tc.wantOC || ev.DenialReason != tc.wantDenial {
				t.Errorf("verb=%q method=%q outcome=%q denial=%q, want %q %q %q %q",
					ev.Verb, ev.HTTPMethod, ev.Outcome, ev.DenialReason, tc.wantVerb, tc.wantMethod, tc.wantOC, tc.wantDenial)
			}
			if ev.Cause != cause {
				t.Errorf("cause_ref must be the composition reconciled on every record, got %+v", ev.Cause)
			}
			if ev.ObjectName != "example-worker" || ev.Resource != "deployments" || ev.ObjectUID == "" {
				t.Errorf("object_ref must be the child written, got %+v", ev)
			}
			if ev.Message != "" {
				t.Errorf("the body is a closed set and this site writes none, got %q", ev.Message)
			}
		})
	}
}

// Every record shape the site can produce goes through the REAL audit SDK and is conformant to the
// controller writer's schema, and the falsifier proves the checker would notice each required key
// missing (audit's acceptance items 3 and 4).
func TestEveryShapeIsConformant(t *testing.T) {
	tp := sdktrace.NewTracerProvider()
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	for _, c := range []helmconfig.ObjectChange{
		helmconfig.ChangeCreated, helmconfig.ChangeCreateFailed, helmconfig.ChangeDeleted,
		helmconfig.ChangeDeleteNotDone, helmconfig.ChangeSpec, helmconfig.ChangeUpdateFailed,
		helmconfig.ChangeKept,
	} {
		t.Run(string(c), func(t *testing.T) {
			rec := audittest.NewRecorder(t)
			e := audit.New(rec.Logger(Scope),
				audit.WithWriter(audit.WriterController),
				audit.WithActor("system:serviceaccount:krateo-system:exampleapps-v1-2-0-controller", audit.ClassController))
			// Emitted while a recording span is active, as the reconcile's is.
			ctx, span := tp.Tracer("test").Start(context.Background(), "reconcile")
			ev, ok, _ := Event(ActionUpgrade, verdict(c, "patch"), cause, audit.PreviewFalse)
			if !ok {
				t.Fatalf("%s must be recorded", c)
			}
			e.Emit(ctx, ev)
			span.End()
			r := rec.Only(t)
			audittest.AssertConformant(t, audit.WriterController, r)
			audittest.AssertRequiredKeysDetected(t, audit.WriterController, r)
		})
	}
}

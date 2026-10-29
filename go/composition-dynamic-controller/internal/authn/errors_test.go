package authn

import (
	"errors"
	"fmt"
	"net"
	"testing"
)

// #251 needs to report WHY the .api source could not be resolved, and the three cases have
// different owners: unreachable is infrastructure (no authn installed in this target), rejected is
// the caller's identity or mapping, and a 5xx is authn's own internals. Deciding that by matching
// on message text would break the first time a message is reworded, so the client returns typed
// errors and these pin the classification.
//
// The taxonomy is authn's, frozen in its docs/api.md as of 0.28.2, which also promises a cause
// never changes its code. The split here is keyed on the status CLASS rather than on the cause
// name, so it stays correct against an older authn that sends no Status body at all — which is
// what actually runs until the next umbrella roll.

func TestParseStatusError(t *testing.T) {
	for _, tc := range []struct {
		name          string
		code          int
		body          string
		wantReason    string
		wantCause     string
		wantHasStatus bool
		wantServer    bool
		wantRejected  bool
	}{
		{
			name:          "403 with a cause (authn >= 0.28.2)",
			code:          403,
			body:          `{"kind":"Status","status":"Failure","reason":"Forbidden","code":403,"details":{"causes":[{"reason":"NoMapping","message":"no clientconfig mapped"}]}}`,
			wantReason:    "Forbidden",
			wantCause:     "NoMapping",
			wantHasStatus: true,
			wantRejected:  true,
		},
		{
			name:          "401 missing bearer, now JSON too",
			code:          401,
			body:          `{"kind":"Status","status":"Failure","reason":"Unauthorized","code":401,"details":{"causes":[{"reason":"MissingBearer"}]}}`,
			wantReason:    "Unauthorized",
			wantCause:     "MissingBearer",
			wantHasStatus: true,
			wantRejected:  true,
		},
		{
			// authn moved its own-fault causes to 500 after review. ServerSide must be true so the
			// condition sends an operator to authn rather than to their audiences and mappings.
			name:          "500 is authn's own failure, not a rejection",
			code:          500,
			body:          `{"kind":"Status","status":"Failure","reason":"InternalError","code":500,"details":{"causes":[{"reason":"IssuanceFailed"}]}}`,
			wantReason:    "InternalError",
			wantCause:     "IssuanceFailed",
			wantHasStatus: true,
			wantServer:    true,
		},
		{
			// The path that actually runs on the fleet today: older authn, non-JSON body. Must not
			// invent a reason it does not have.
			name:         "older authn sends plain text",
			code:         403,
			body:         "forbidden\n",
			wantRejected: true,
		},
		{
			name:       "a 502 from something in front of authn carries no Status either",
			code:       502,
			body:       "<html>bad gateway</html>",
			wantServer: true,
		},
		{
			// Valid JSON that is not a Status must not be mined for fields.
			name: "JSON that is not a Status is not treated as one",
			code: 403,
			body: `{"error":"nope","reason":"Forbidden"}`,
			// wantReason deliberately empty: kind != "Status"
			wantRejected: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := parseStatusError(tc.code, []byte(tc.body))
			if e.Code != tc.code {
				t.Errorf("Code = %d, want %d", e.Code, tc.code)
			}
			if e.Reason != tc.wantReason {
				t.Errorf("Reason = %q, want %q", e.Reason, tc.wantReason)
			}
			if e.Cause != tc.wantCause {
				t.Errorf("Cause = %q, want %q", e.Cause, tc.wantCause)
			}
			if e.HasStatusBody != tc.wantHasStatus {
				t.Errorf("HasStatusBody = %v, want %v", e.HasStatusBody, tc.wantHasStatus)
			}
			if e.ServerSide() != tc.wantServer {
				t.Errorf("ServerSide() = %v, want %v", e.ServerSide(), tc.wantServer)
			}
			if e.Rejected() != tc.wantRejected {
				t.Errorf("Rejected() = %v, want %v", e.Rejected(), tc.wantRejected)
			}
			// The message must always carry the code and body, whatever was parseable, or an
			// operator loses the only detail an older authn provides.
			if msg := e.Error(); msg == "" {
				t.Error("Error() is empty")
			}
		})
	}
}

// The two error types must be distinguishable with errors.As through wrapping, because the cdc's
// snowplow client wraps this with %w ("acquiring snowplow token: %w") before #251 ever sees it.
func TestErrorsAreUnwrappableThroughWrapping(t *testing.T) {
	unreachable := &UnreachableError{Server: "http://authn.krateo-system.svc:8082", Err: &net.DNSError{Err: "no such host"}}
	wrapped := errors.New("outer: " + unreachable.Error())
	_ = wrapped

	var ue *UnreachableError
	if !errors.As(wrapError(wrapError(unreachable)), &ue) {
		t.Fatal("UnreachableError must survive two layers of %w wrapping")
	}
	if ue.Server == "" {
		t.Error("the recovered error lost its Server, which is what tells an operator WHERE to look")
	}

	var se *StatusError
	if !errors.As(wrapError(wrapError(parseStatusError(403, []byte(`{"kind":"Status","reason":"Forbidden"}`)))), &se) {
		t.Fatal("StatusError must survive two layers of %w wrapping")
	}
	if se.Code != 403 {
		t.Errorf("recovered Code = %d, want 403", se.Code)
	}

	// CONTROL: an unrelated error must NOT match either type, or errors.As is matching too eagerly
	// and every failure would be reported as an authn problem.
	var ue2 *UnreachableError
	if errors.As(wrapError(errors.New("snowplow returned 500")), &ue2) {
		t.Error("an unrelated error matched *UnreachableError")
	}
}

func wrapError(err error) error { return fmt.Errorf("layer: %w", err) }

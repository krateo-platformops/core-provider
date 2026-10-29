package authn

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// Typed errors so a caller can tell WHY the token exchange failed without matching on message
// text. The distinction matters because the three cases have different owners: Unreachable is
// infrastructure (authn is not installed here, or not routable), Rejected is the caller's identity
// or mapping, and ServerError is authn's own internals. #251 surfaces each as a different
// ApiResolved reason, and string-matching an error message is not a sound way to decide which.

// UnreachableError means the request never reached authn: DNS, dial, TLS or timeout. On a spoke
// this is the signature of "no authn was installed into this target at all", which is the failure
// #251 exists to stop being silent.
type UnreachableError struct {
	Server string
	Err    error
}

func (e *UnreachableError) Error() string {
	return fmt.Sprintf("authn at %s is unreachable: %v", e.Server, e.Err)
}
func (e *UnreachableError) Unwrap() error { return e.Err }

// StatusError means authn answered with a non-200. Cause carries authn's own machine-readable
// discriminator from details.causes[0].reason when present (authn >= 0.28.2); Reason carries the
// top-level Kubernetes Status reason. Both are empty against an older authn, which returns a
// non-JSON body — hence HasStatusBody.
type StatusError struct {
	Code          int
	Reason        string // top-level Status reason, e.g. Forbidden / Unauthorized / InternalError
	Cause         string // details.causes[0].reason, e.g. NoMapping / AudienceMismatch
	Body          string
	HasStatusBody bool
}

func (e *StatusError) Error() string {
	switch {
	case e.Cause != "":
		return fmt.Sprintf("authn returned %d (%s/%s): %s", e.Code, e.Reason, e.Cause, e.Body)
	case e.Reason != "":
		return fmt.Sprintf("authn returned %d (%s): %s", e.Code, e.Reason, e.Body)
	default:
		return fmt.Sprintf("authn returned %d: %s", e.Code, e.Body)
	}
}

// ServerSide reports whether this is authn failing on its own account rather than refusing the
// caller. A rejection is about the caller; a 5xx is about authn, and conflating them sends an
// operator to the wrong place. Deliberately keyed on the status CLASS rather than on the
// InternalError reason, so it holds for an older authn that sends no Status body at all.
func (e *StatusError) ServerSide() bool { return e.Code >= 500 }

// Rejected reports whether authn considered the request and refused it.
func (e *StatusError) Rejected() bool {
	return e.Code == http.StatusUnauthorized || e.Code == http.StatusForbidden
}

// parseStatusError builds a StatusError from a non-200 response body, decoding it as a Kubernetes
// Status when it is one. A body that is not a Status is not an error here: an older authn sends
// plain text, and the code alone is still a usable signal.
func parseStatusError(code int, body []byte) *StatusError {
	out := &StatusError{Code: code, Body: string(body)}

	var st struct {
		Kind    string `json:"kind"`
		Reason  string `json:"reason"`
		Details struct {
			Causes []struct {
				Reason string `json:"reason"`
			} `json:"causes"`
		} `json:"details"`
	}
	if err := json.Unmarshal(body, &st); err != nil || st.Kind != "Status" {
		return out
	}
	out.HasStatusBody = true
	out.Reason = st.Reason
	if len(st.Details.Causes) > 0 {
		out.Cause = st.Details.Causes[0].Reason
	}
	return out
}

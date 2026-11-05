package chartinspector

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/krateo-platformops/plumbing/http/response"
)

// StatusError carries chart-inspector's HTTP status so a caller can tell WHOSE fault a failure is.
//
// Before this, every non-200 collapsed into a formatted string and a 422 was indistinguishable
// from a 500 — so the cdc could not act on the distinction chart-inspector had just started
// making (core-provider#265).
type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("unexpected status code: %d - response body: %s", e.Code, e.Body)
}

// Unprocessable reports a 422: the request was well-formed and chart-inspector cannot render it
// AS THINGS STAND. The canonical cause is a chart referencing an API kind the cluster does not
// serve.
//
// "AS THINGS STAND" IS THE WHOLE CONTRACT AND A CALLER MUST NOT LATCH ON IT. chart-inspector
// classifies on a REST-mapping miss, which cannot distinguish a kind that will never exist from
// one whose CRD was created moments ago and has not yet reached its RESTMapper. Treating a 422 as
// permanent would freeze a composition on a transient window — the same defect as a condition
// computed once from state nobody refreshes (#261, #273).
func (e *StatusError) Unprocessable() bool { return e.Code == http.StatusUnprocessableEntity }

// ReasonValuesSchemaViolation is the Status reason chart-inspector puts on a values-schema 400
// (#325). A CONTRACT: chart-inspector's internal/handlers/resources/get declares the same value, and
// the two modules cannot share a declaration, so each pins the literal in a test.
const ReasonValuesSchemaViolation = "ValuesSchemaViolation"

// ValuesSchemaViolation reports a 400 that chart-inspector MARKED as a values-schema violation.
//
// BY WHAT THE RESPONSE SAYS, NOT BY ELIMINATION. chart-inspector answers 400 for missing query
// parameters and a missing releaseName too, all with reason BadRequest. Classifying "any 400 this
// client could not have caused" as a schema violation would hold only while Validate and the
// server agree on every parameter, and would mislabel the next 400 cause anyone adds. A 400 without
// the reason is a refusal, and is reported as one.
func (e *StatusError) ValuesSchemaViolation() bool {
	return e.Code == http.StatusBadRequest && e.Reason() == ReasonValuesSchemaViolation
}

// Reason is the Status reason chart-inspector sent, or "" when the body is not a Status that has one.
func (e *StatusError) Reason() string {
	var st response.Status
	if err := json.Unmarshal([]byte(e.Body), &st); err != nil {
		return ""
	}
	return string(st.Reason)
}

// ClientSide reports any 4xx, including the two above.
func (e *StatusError) ClientSide() bool { return e.Code >= 400 && e.Code < 500 }

// ServerSide reports a 5xx: chart-inspector itself failed and the caller's blueprint is not
// implicated.
func (e *StatusError) ServerSide() bool { return e.Code >= 500 }

// Detail is what chart-inspector SAID, in the form a person reads it.
//
// chart-inspector answers every failure with plumbing's response.Status, a JSON object whose
// `message` is the sentence (for a values-schema 400, helm's own explanation). Quoting Body verbatim
// put that sentence inside a JSON string, escaped quotes and all, which is the burying #325 is
// about. So the message is extracted when the body is a Status that carries one.
//
// DEFENSIVE, AND IT NEVER DROPS THE BODY. Anything else in front of chart-inspector can answer too
// (net/http's ServeMux answers a wrong path with the text "404 page not found"), and an older or
// newer chart-inspector may shape its body differently. A body that is not a JSON object, or is one
// without a string `message`, comes back verbatim: an unparsed sentence is less readable, a dropped
// one is lost.
func (e *StatusError) Detail() string {
	var st response.Status
	if err := json.Unmarshal([]byte(e.Body), &st); err == nil && st.Message != "" {
		return st.Message
	}
	return e.Body
}

// AsStatusError unwraps to a *StatusError if the chain carries one.
func AsStatusError(err error) (*StatusError, bool) {
	var se *StatusError
	if errors.As(err, &se) {
		return se, true
	}
	return nil, false
}

// MissingParameterError is Validate refusing a request chart-inspector would refuse. No request was
// sent, so a caller must not report it as chart-inspector being unreachable.
type MissingParameterError struct {
	// Name is the query parameter, spelled as chart-inspector reads it.
	Name string
}

func (e *MissingParameterError) Error() string { return e.Name + " is required" }

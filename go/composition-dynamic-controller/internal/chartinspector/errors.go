package chartinspector

import (
	"errors"
	"fmt"
	"net/http"
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

// ServerSide reports a 5xx: chart-inspector itself failed and the caller's blueprint is not
// implicated.
func (e *StatusError) ServerSide() bool { return e.Code >= 500 }

// AsStatusError unwraps to a *StatusError if the chain carries one.
func AsStatusError(err error) (*StatusError, bool) {
	var se *StatusError
	if errors.As(err, &se) {
		return se, true
	}
	return nil, false
}

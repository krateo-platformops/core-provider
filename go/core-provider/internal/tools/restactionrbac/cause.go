package restactionrbac

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// Cause names why a RESTAction's read-set could not be fetched. Each one sends an operator somewhere
// different, which is why they are told apart.
type Cause string

const (
	// CauseRESTActionNotFound: snowplow says the RESTAction does not exist.
	CauseRESTActionNotFound Cause = "RESTActionNotFound"
	// CauseRESTActionForbidden: the caller may not read the RESTAction. It MAY ALSO NOT EXIST: on an
	// informer miss snowplow asks the apiserver with the caller's own token, and a caller without
	// `get restactions` is refused either way. So this never means "missing".
	CauseRESTActionForbidden Cause = "RESTActionForbidden"
	// CauseStageUnresolvable: a stage cannot be enumerated without dispatching it.
	CauseStageUnresolvable Cause = "StageUnresolvable"
	// CauseInvalidApiRef: snowplow refused the request itself, such as extras naming a reserved key.
	CauseInvalidApiRef Cause = "InvalidApiRef"
	// CauseUnauthorized: no usable identity reached snowplow.
	CauseUnauthorized Cause = "Unauthorized"
	// CauseSnowplowUnavailable: snowplow could not be reached, or ran out of time.
	CauseSnowplowUnavailable Cause = "SnowplowUnavailable"
	// CauseSnowplowError: snowplow, or something in front of it, answered in a way nothing here
	// recognises. That includes cyclic stages, which snowplow reports as a plain 500.
	CauseSnowplowError Cause = "SnowplowError"
	// CauseNotConfigured: core-provider has no snowplow URL, so apiRef RBAC is switched off in the
	// chart while a definition declares an apiRef.
	CauseNotConfigured Cause = "SnowplowNotConfigured"
)

// maxDetail bounds what a response body may add to an error, and so to a condition message.
const maxDetail = 1024

// Error is a read-set that could not be fetched.
type Error struct {
	Cause Cause
	// Status is snowplow's HTTP status, 0 when no response arrived.
	Status int
	Detail string
	err    error
}

func (e *Error) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("restactionrbac: snowplow /rbac answered %d (%s): %s", e.Status, e.Cause, e.Detail)
	}
	return fmt.Sprintf("restactionrbac: %s: %s", e.Cause, e.Detail)
}

func (e *Error) Unwrap() error { return e.err }

// NotConfigured is the error for a declared apiRef with no snowplow URL.
func NotConfigured() error {
	return &Error{Cause: CauseNotConfigured, Detail: "snowplow URL not configured (set CORE_PROVIDER_SNOWPLOW_URL, or apiRefRBAC.enabled in the chart)"}
}

// failedBeforeResponse wraps a failure that never got an answer from snowplow.
func failedBeforeResponse(cause Cause, what string, err error) error {
	return &Error{Cause: cause, Detail: fmt.Sprintf("%s: %v", what, err), err: err}
}

// fromResponse classifies a non-200 answer. snowplow's `reason` decides 403 and 404, not the status
// alone: a route that does not exist, an older snowplow or a proxy can all answer those statuses, and
// calling that a missing RESTAction sends an operator to check the wrong object.
func fromResponse(status int, body []byte) error {
	var s struct {
		Kind    string `json:"kind"`
		Reason  string `json:"reason"`
		Message string `json:"message"`
	}
	detail := bounded(body)
	if json.Unmarshal(body, &s) == nil && s.Kind == "Status" && s.Message != "" {
		detail = bounded([]byte(s.Message))
	}

	e := &Error{Cause: CauseSnowplowError, Status: status, Detail: detail}
	switch {
	case status == http.StatusNotFound && s.Kind == "Status" && s.Reason == "NotFound":
		e.Cause = CauseRESTActionNotFound
	case status == http.StatusForbidden && s.Kind == "Status" && s.Reason == "Forbidden":
		e.Cause = CauseRESTActionForbidden
	case status == http.StatusUnprocessableEntity:
		e.Cause, e.err = CauseStageUnresolvable, ErrIncomplete
	case status == http.StatusBadRequest:
		e.Cause = CauseInvalidApiRef
	case status == http.StatusUnauthorized:
		e.Cause = CauseUnauthorized
	case status == http.StatusBadGateway, status == http.StatusServiceUnavailable, status == http.StatusGatewayTimeout:
		e.Cause = CauseSnowplowUnavailable
	}
	return e
}

func bounded(b []byte) string {
	if len(b) > maxDetail {
		return string(b[:maxDetail]) + "...(truncated)"
	}
	return string(b)
}

// AsError returns the read-set failure inside err, if there is one.
func AsError(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

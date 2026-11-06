package restactionrbac

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const statusBody = `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":%q,"code":%d,"message":"the detail"}`

func status(reason string, code int) string {
	return strings.NewReplacer("%q", `"`+reason+`"`, "%d", itoa(code)).Replace(statusBody)
}

func itoa(n int) string {
	return map[int]string{400: "400", 401: "401", 403: "403", 404: "404", 500: "500", 504: "504"}[n]
}

func inspectAgainst(t *testing.T, code int, body string) error {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	_, err := New(srv.URL).Inspect(context.Background(), Params{APIRefName: "ra", APIRefNamespace: "ns"})
	return err
}

// Each row is snowplow's /rbac contract (1.12.56 and 1.12.65). The reason field decides, not the
// status alone, because the operator's action differs and the status can come from elsewhere.
func TestInspect_ClassifiesEachOutcome(t *testing.T) {
	for name, tc := range map[string]struct {
		code int
		body string
		want Cause
	}{
		"missing RESTAction":  {404, status("NotFound", 404), CauseRESTActionNotFound},
		"caller may not read": {403, status("Forbidden", 403), CauseRESTActionForbidden},
		"stage unresolvable":  {422, "stage x cannot be enumerated", CauseStageUnresolvable},
		"bad request":         {400, status("BadRequest", 400), CauseInvalidApiRef},
		"no user config":      {401, status("Unauthorized", 401), CauseUnauthorized},
		"cyclic stages":       {500, status("InternalError", 500), CauseSnowplowError},
		"page budget ran out": {504, status("Timeout", 504), CauseSnowplowUnavailable},
		"bad gateway":         {502, "upstream gone", CauseSnowplowUnavailable},
		"service unavailable": {503, "", CauseSnowplowUnavailable},
		"unlisted status":     {418, "teapot", CauseSnowplowError},
		// A 404 that is not snowplow's Status is a route that does not exist, an old snowplow or a
		// proxy. Calling it a missing RESTAction sends an operator to check the wrong object.
		"bare 404":              {404, "404 page not found", CauseSnowplowError},
		"404 with other reason": {404, status("Gone", 404), CauseSnowplowError},
		// The same for 403: a proxy refusing us says nothing about the RESTAction.
		"bare 403": {403, "denied by proxy", CauseSnowplowError},
	} {
		t.Run(name, func(t *testing.T) {
			var e *Error
			err := inspectAgainst(t, tc.code, tc.body)
			if !errors.As(err, &e) {
				t.Fatalf("want a *Error, got %T: %v", err, err)
			}
			if e.Cause != tc.want {
				t.Fatalf("cause = %q, want %q", e.Cause, tc.want)
			}
			if e.Status != tc.code {
				t.Fatalf("status = %d, want %d", e.Status, tc.code)
			}
		})
	}
}

// Nothing may stop matching ErrIncomplete for adding a cause.
func TestInspect_UnresolvableStageIsStillErrIncomplete(t *testing.T) {
	if err := inspectAgainst(t, 422, "stage x"); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("want ErrIncomplete, got %v", err)
	}
	if err := inspectAgainst(t, 404, status("NotFound", 404)); errors.Is(err, ErrIncomplete) {
		t.Fatal("a missing RESTAction must not read as an incomplete read-set")
	}
}

// The operator reads the message, so snowplow's own text must reach it.
func TestInspect_KeepsSnowplowsMessage(t *testing.T) {
	if err := inspectAgainst(t, 404, status("NotFound", 404)); !strings.Contains(err.Error(), "the detail") {
		t.Fatalf("snowplow's message was lost: %v", err)
	}
}

func TestInspect_BoundsAnOversizedBody(t *testing.T) {
	err := inspectAgainst(t, 500, strings.Repeat("x", 100000))
	if len(err.Error()) > 2*maxDetail {
		t.Fatalf("a %d byte body reached the error unbounded", len(err.Error()))
	}
}

func TestInspect_UnreachableIsSnowplowUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	_, err := New(url).Inspect(context.Background(), Params{APIRefName: "ra", APIRefNamespace: "ns"})
	var e *Error
	if !errors.As(err, &e) || e.Cause != CauseSnowplowUnavailable {
		t.Fatalf("want SnowplowUnavailable, got %v", err)
	}
}

func TestInspect_TokenFailureIsUnauthorized(t *testing.T) {
	cli := New("http://127.0.0.1:1").WithToken(func(context.Context) (string, error) {
		return "", errors.New("authn said no")
	})
	_, err := cli.Inspect(context.Background(), Params{APIRefName: "ra", APIRefNamespace: "ns"})
	var e *Error
	if !errors.As(err, &e) || e.Cause != CauseUnauthorized {
		t.Fatalf("want Unauthorized, got %v", err)
	}
}

// Success is not an error: a control that the classification did not swallow the 200 path.
func TestInspect_SuccessStillDecodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"readSet":[{"group":"","version":"v1","resource":"pods","verb":"get"}]}`))
	}))
	defer srv.Close()

	got, err := New(srv.URL).Inspect(context.Background(), Params{APIRefName: "ra", APIRefNamespace: "ns"})
	if err != nil || len(got) != 1 {
		t.Fatalf("want one row and no error, got %d rows, err %v", len(got), err)
	}
}

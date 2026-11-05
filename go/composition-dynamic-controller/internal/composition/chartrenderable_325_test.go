package composition

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/krateo-platformops/composition-dynamic-controller/internal/chartinspector"
	"github.com/krateo-platformops/plumbing/http/response"
)

// helmSaid is a values-schema failure as plumbing reports it, with the quotes and the newline that
// the raw JSON body escapes. So "the message is parsed" and "the body is quoted" differ visibly.
const helmSaid = "chart values do not satisfy the chart's values.schema.json: helm said: values " +
	"don't meet the specifications of the schema(s) in the following chart(s):\nprobe:\n- (root): " +
	"Additional property \"global\" is not allowed"

// statusBody is a failure body exactly as chart-inspector writes one: plumbing's response.New,
// JSON-encoded.
func statusBody(t *testing.T, code int, msg string) string {
	t.Helper()
	return encodeStatus(t, response.New(code, errors.New(msg)))
}

// schemaViolationBody is #350's values-schema 400: response.New(400, err) with the reason set.
func schemaViolationBody(t *testing.T, msg string) string {
	t.Helper()
	return encodeStatus(t, schemaViolationStatus(msg))
}

func schemaViolationStatus(msg string) *response.Status {
	st := response.New(http.StatusBadRequest, errors.New(msg))
	st.Reason = "ValuesSchemaViolation" // the literal chart-inspector pins
	return st
}

func encodeStatus(t *testing.T, st *response.Status) string {
	t.Helper()
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// #325, across the INPUT STATES the classifier can be handed. Each row asserts the reason AND what
// the message must and must not contain, because "the reason is right" is the half a consumer
// branches on and "the author can read helm's sentence" is the half the issue was filed about.
func TestChartRenderableFailureInputStates(t *testing.T) {
	status400 := schemaViolationBody(t, helmSaid)
	param400 := statusBody(t, http.StatusBadRequest, "missing required query parameters")
	status403 := statusBody(t, http.StatusForbidden, "forbidden by the mesh")
	status500 := statusBody(t, http.StatusInternalServerError, "unable to fetch the chart")

	for _, tc := range []struct {
		name        string
		err         error
		wantReason  string
		mustHave    []string
		mustNotHave []string
	}{
		{
			name:        "400 MARKED ValuesSchemaViolation: helm's sentence parsed out",
			err:         fmt.Errorf("getting resources: %w", &chartinspector.StatusError{Code: 400, Body: status400}),
			wantReason:  reasonValuesSchemaViolation,
			mustHave:    []string{helmSaid, "values.schema.json", "HTTP 400"},
			mustNotHave: []string{`"kind":"Status"`, `\"global\"`, `\n`},
		},
		{
			// THE CONTROL for the marker: chart-inspector's OTHER 400s (parameters) carry reason
			// BadRequest, and must not be reported as the author's values being wrong.
			name:        "400 Status WITHOUT the marker (a parameter 400): a refusal, not a schema violation",
			err:         &chartinspector.StatusError{Code: 400, Body: param400},
			wantReason:  reasonInspectorRejected,
			mustHave:    []string{"refused the request (HTTP 400)", "missing required query parameters"},
			mustNotHave: []string{"values.schema.json"},
		},
		{
			name:       "400 with a non-JSON body: unmarked, so a refusal, the body verbatim",
			err:        &chartinspector.StatusError{Code: 400, Body: "Bad Request: something in front of chart-inspector"},
			wantReason: reasonInspectorRejected,
			mustHave:   []string{"Bad Request: something in front of chart-inspector"},
		},
		{
			name:       "400 with a JSON body that is not a Status: unmarked, the body verbatim",
			err:        &chartinspector.StatusError{Code: 400, Body: `{"error":"x"}`},
			wantReason: reasonInspectorRejected,
			mustHave:   []string{`{"error":"x"}`},
		},
		{
			name:       "400 with no body: unmarked, says so",
			err:        &chartinspector.StatusError{Code: 400},
			wantReason: reasonInspectorRejected,
			mustHave:   []string{"(no response body)"},
		},
		{
			// The marker on a non-400 is not a schema violation: the code decides the class, the
			// reason only clarifies it.
			name:       "the marker on a 500: still InspectorUnavailable",
			err:        &chartinspector.StatusError{Code: 500, Body: `{"kind":"Status","code":500,"reason":"ValuesSchemaViolation"}`},
			wantReason: reasonInspectorUnavailable,
		},
		{
			// CONTROL. The 422 arm is unchanged, including its fixed message without the body.
			name:        "422: UnservedKind, unchanged",
			err:         &chartinspector.StatusError{Code: 422, Body: statusBody(t, 422, `no matches for kind "X"`)},
			wantReason:  reasonUnservedKind,
			mustHave:    []string{"install the missing CRD", "HTTP 422"},
			mustNotHave: []string{"values.schema.json", "no matches for kind"},
		},
		{
			// CONTROL. The 5xx arm is unchanged: the raw body, as before.
			name:       "500: InspectorUnavailable, unchanged",
			err:        &chartinspector.StatusError{Code: 500, Body: status500},
			wantReason: reasonInspectorUnavailable,
			mustHave:   []string{"chart-inspector failed to render the chart (HTTP 500): " + status500},
		},
		{
			name:       "no response: InspectorUnavailable, could not reach",
			err:        fmt.Errorf("getting chartinspector: %w", errors.New("dial tcp 10.0.0.1:8081: connect: connection refused")),
			wantReason: reasonInspectorUnavailable,
			mustHave:   []string{"could not reach chart-inspector", "connection refused"},
		},
		{
			name:        "403 with a Status body: a refusal, message parsed",
			err:         &chartinspector.StatusError{Code: 403, Body: status403},
			wantReason:  reasonInspectorRejected,
			mustHave:    []string{"refused the request (HTTP 403)", "forbidden by the mesh"},
			mustNotHave: []string{`"kind":"Status"`, "unavailable"},
		},
		{
			name:       "404 from the mux: a refusal, text verbatim",
			err:        &chartinspector.StatusError{Code: 404, Body: "404 page not found\n"},
			wantReason: reasonInspectorRejected,
			mustHave:   []string{"404 page not found", "/resources"},
		},
		{
			name:       "a status outside the protocol is chart-inspector's",
			err:        &chartinspector.StatusError{Code: 302, Body: "Found"},
			wantReason: reasonInspectorUnavailable,
			mustHave:   []string{"HTTP 302", "Found"},
		},
		{
			name:        "a request Validate refused was never sent, so it is not 'could not reach'",
			err:         fmt.Errorf("validating parameters: %w", &chartinspector.MissingParameterError{Name: "releaseName"}),
			wantReason:  reasonInspectorRejected,
			mustHave:    []string{`"releaseName"`, "did not call chart-inspector"},
			mustNotHave: []string{"could not reach"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reason, msg := chartRenderableFailure(tc.err)
			if reason != tc.wantReason {
				t.Errorf("reason = %q, want %q (message %q)", reason, tc.wantReason, msg)
			}
			for _, s := range tc.mustHave {
				if !strings.Contains(msg, s) {
					t.Errorf("message lacks %q:\n%s", s, msg)
				}
			}
			for _, s := range tc.mustNotHave {
				if strings.Contains(msg, s) {
					t.Errorf("message contains %q, which it must not:\n%s", s, msg)
				}
			}
		})
	}
}

// The same 400 THROUGH THE REAL CLIENT: chart-inspector's Status, written by plumbing's encoder
// onto the wire, captured by Resources into a StatusError and classified here. The table above
// hands the classifier a StatusError it built; this proves the client produces one the classifier
// reads the same way, so a change to how the client captures the body cannot pass both.
func TestValuesSchema400ThroughTheClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = response.Encode(w, schemaViolationStatus(helmSaid))
	}))
	defer srv.Close()

	ci := chartinspector.NewChartInspector(srv.URL)
	_, err := ci.Resources(context.Background(), chartinspector.Parameters{
		CompositionName:                "probe",
		CompositionNamespace:           "demo",
		CompositionVersion:             "v0-1-0",
		CompositionResource:            "probes",
		CompositionDefinitionName:      "probe",
		CompositionDefinitionNamespace: "krateo-system",
		ReleaseName:                    "probe-1",
	})
	if err == nil {
		t.Fatal("a 400 returned no error")
	}
	reason, msg := chartRenderableFailure(fmt.Errorf("generating RBAC: %w", err))
	if reason != reasonValuesSchemaViolation {
		t.Errorf("reason = %q, want %q", reason, reasonValuesSchemaViolation)
	}
	if !strings.Contains(msg, helmSaid) {
		t.Errorf("message does not carry helm's sentence unescaped:\n%s", msg)
	}

	// CONTROL: the error the client returned DOES carry the raw JSON, so the assertion above is
	// about the classifier parsing it and not about the body never having been JSON.
	if !strings.Contains(err.Error(), `"kind":"Status"`) {
		t.Errorf("control: the client's error should carry the raw Status body, got %v", err)
	}
}

// The reason strings are a consumer contract (frontend and portal read them), so the VALUES are
// pinned, not the identifiers: renaming one is a breaking change for a reader in another repo.
// The set is OPEN; a consumer must handle a reason it does not know.
func TestChartRenderableReasonStringsAreStable(t *testing.T) {
	for _, c := range []struct{ got, want string }{
		{reasonChartRenderable, "Renderable"},
		{reasonUnservedKind, "UnservedKind"},
		{reasonValuesSchemaViolation, "ValuesSchemaViolation"},
		{reasonInspectorRejected, "InspectorRejected"},
		{reasonInspectorUnavailable, "InspectorUnavailable"},
	} {
		if c.got != c.want {
			t.Errorf("reason = %q, want %q: a consumer matches this string exactly", c.got, c.want)
		}
	}
}

// The marker's VALUE is a contract with chart-inspector, which pins the same literal; asserting the
// literal (not the const) means a rename here fails here rather than silently in production.
func TestValuesSchemaViolationReasonIsTheContractLiteral(t *testing.T) {
	if chartinspector.ReasonValuesSchemaViolation != "ValuesSchemaViolation" {
		t.Errorf("ReasonValuesSchemaViolation = %q; chart-inspector sends \"ValuesSchemaViolation\"",
			chartinspector.ReasonValuesSchemaViolation)
	}
}

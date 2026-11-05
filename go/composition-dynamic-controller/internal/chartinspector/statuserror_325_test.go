package chartinspector

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/krateo-platformops/plumbing/http/response"
)

// validParams satisfies every check chart-inspector's handler makes.
func validParams() Parameters {
	return Parameters{
		CompositionName:                "mariadb",
		CompositionNamespace:           "openstack",
		CompositionVersion:             "v0-3-28",
		CompositionResource:            "mariadbs",
		CompositionDefinitionName:      "mariadb",
		CompositionDefinitionNamespace: "krateo-system",
		ReleaseName:                    "mariadb-9c847c79",
	}
}

// #325. A 400 from chart-inspector can be read as a values-schema violation only if its two
// parameter-check 400s cannot reach the cdc. This asserts the set Validate refuses is the set
// chart-inspector checks, by SENDING: each blanked parameter must produce no request at all.
//
// The seven names are chart-inspector's (handlers/resources/get/resources.go: the six in its
// "missing required query parameters" check, and releaseName in its own check from #329).
func TestValidatePreemptsEveryParameterCheck400(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	ci := NewChartInspector(srv.URL)

	blank := map[string]func(*Parameters){
		"compositionName":                func(p *Parameters) { p.CompositionName = "" },
		"compositionNamespace":           func(p *Parameters) { p.CompositionNamespace = "" },
		"compositionVersion":             func(p *Parameters) { p.CompositionVersion = "" },
		"compositionResource":            func(p *Parameters) { p.CompositionResource = "" },
		"compositionDefinitionName":      func(p *Parameters) { p.CompositionDefinitionName = "" },
		"compositionDefinitionNamespace": func(p *Parameters) { p.CompositionDefinitionNamespace = "" },
		"releaseName":                    func(p *Parameters) { p.ReleaseName = "" },
	}
	refused := 0
	for name, clear := range blank {
		t.Run(name, func(t *testing.T) {
			p := validParams()
			clear(&p)
			before := hits.Load()
			_, err := ci.Resources(context.Background(), p)
			var missing *MissingParameterError
			if !errors.As(err, &missing) {
				t.Fatalf("Resources with %s empty returned %v; want a *MissingParameterError", name, err)
			}
			if missing.Name != name {
				t.Errorf("MissingParameterError names %q; want %q", missing.Name, name)
			}
			if _, ok := AsStatusError(err); ok {
				t.Error("a refusal before sending carries no HTTP status, and must not look like it does")
			}
			if hits.Load() != before {
				t.Errorf("a request reached chart-inspector with %s empty; it would have answered 400 "+
					"and been read as a values-schema violation", name)
			}
			refused++
		})
	}
	t.Logf("refused %d of %d blanked parameters before sending", refused, len(blank))
	if refused != 7 {
		t.Errorf("refused %d of 7; the subtests above did not all run", refused)
	}

	// CONTROL: with every parameter present the request IS sent, so a zero above is the client
	// refusing and not the server being unreachable.
	before := hits.Load()
	if _, err := ci.Resources(context.Background(), validParams()); err != nil {
		t.Fatalf("control: Resources with every parameter set: %v", err)
	}
	if got := hits.Load() - before; got != 1 {
		t.Errorf("control: %d requests reached the server with every parameter set; want 1", got)
	}
}

// closeRecorder is a response body that records whether it was closed.
type closeRecorder struct {
	io.Reader
	closed atomic.Bool
}

func (c *closeRecorder) Close() error { c.closed.Store(true); return nil }

// failingReader fails mid-body, so the non-200 arm's io.ReadAll returns an error.
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("connection reset mid-body") }

type stubTransport struct {
	code int
	body *closeRecorder
}

func (s stubTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: s.code, Body: s.body, Header: http.Header{}, Request: r}, nil
}

// The client closes the response body on EVERY path. The defer used to sit below the non-200
// return, so the 400, 500 and read-error rows failed here while the 200 row, the control, passed.
//
// A stub transport rather than httptest, because the real transport releases a connection on a
// read to EOF whether or not Close is called (see the comment at the defer), and so cannot tell a
// closed body from one read to the end.
func TestResourcesClosesTheBodyOnEveryPath(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		body io.Reader
	}{
		{"200 (control)", http.StatusOK, strings.NewReader(`[]`)},
		{"400", http.StatusBadRequest, strings.NewReader(`{"kind":"Status","code":400}`)},
		{"500", http.StatusInternalServerError, strings.NewReader(`{"kind":"Status","code":500}`)},
		{"non-200 whose body fails to read", http.StatusBadGateway, failingReader{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &closeRecorder{Reader: tc.body}
			ci := NewChartInspector("http://chart-inspector.invalid")
			ci.WithHTTPClient(&http.Client{Transport: stubTransport{code: tc.code, body: rec}})
			_, _ = ci.Resources(context.Background(), validParams())
			if !rec.closed.Load() {
				t.Errorf("HTTP %d: the response body was never closed", tc.code)
			}
		})
	}
}

// statusBody is a failure body exactly as chart-inspector writes it: plumbing's response.New,
// JSON-encoded, the call its handler makes (response.Encode(w, response.New(code, err))).
func statusBody(t *testing.T, code int, msg string) string {
	t.Helper()
	b, err := json.Marshal(response.New(code, errors.New(msg)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Detail extracts a Status message, and returns everything else verbatim.
func TestStatusErrorDetail(t *testing.T) {
	// Quotes and a newline, so the parsed and the raw forms differ visibly: the raw JSON escapes
	// both.
	const helmSaid = "values don't meet the specifications of the schema(s) in the following " +
		"chart(s):\nprobe:\n- (root): Additional property \"global\" is not allowed"

	for _, tc := range []struct {
		name, body, want string
	}{
		{"a Status with a message yields the message", statusBody(t, 400, helmSaid), helmSaid},
		{"plain text yields the text", "404 page not found\n", "404 page not found\n"},
		{"a JSON array yields the body", `["not","a","status"]`, `["not","a","status"]`},
		{"a Status without a message yields the body", `{"kind":"Status","code":400}`, `{"kind":"Status","code":400}`},
		{"a non-string message yields the body", `{"message":5}`, `{"message":5}`},
		{"JSON null yields the body", `null`, `null`},
		{"an empty body yields nothing, and nothing is lost", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := (&StatusError{Code: 400, Body: tc.body}).Detail()
			if got != tc.want {
				t.Errorf("Detail() = %q, want %q", got, tc.want)
			}
		})
	}
}

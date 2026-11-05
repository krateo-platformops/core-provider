package chartinspector

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// #329. The resolved release name must reach chart-inspector ON THE WIRE.
//
// WHY THIS TEST IS AT THE WIRE AND NOT AT THE STRUCT. chart-inspector used to re-derive the name
// from the composition's krateo.io/release-name LABEL, read off the live object it fetches itself.
// On core-provider's #303 adoption path that label does not exist — its absence is what triggers
// adoption — the cdc resolves the adopted name in memory, and Observe is a pure read that must not
// persist it. So chart-inspector dry-ran an install with release name "" and helm answered
// `no name provided`; the failure was in Observe, so the runtime never reached Update, the only
// phase that would have written the label. Retried at 1s, 2s, 4s, 8s, 16s, forever.
//
// A test asserting the Parameters struct would have passed the whole time, because the struct was
// never the problem — the name was simply not in the request. The query string is the interface.
func TestResources_SendsTheReleaseNameOnTheWire(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	ci := NewChartInspector(srv.URL)
	if _, err := ci.Resources(context.Background(), Parameters{
		CompositionName:                "mariadb",
		CompositionNamespace:           "openstack",
		CompositionVersion:             "v0-3-28",
		CompositionResource:            "mariadbs",
		CompositionDefinitionName:      "mariadb",
		CompositionDefinitionNamespace: "krateo-system",
		ReleaseName:                    "mariadb-9c847c79",
	}); err != nil {
		t.Fatalf("Resources: %v", err)
	}

	if got := queryValue(gotQuery, "releaseName"); got != "mariadb-9c847c79" {
		t.Errorf("releaseName on the wire = %q, want %q. The request carried %q.\n\n"+
			"Without it chart-inspector falls back to nothing and answers 400, or on the version "+
			"before #329 dry-ran an install with release name \"\" and answered 500 — which the "+
			"caller retried forever, because a 500 is the one class a caller may read as "+
			"'not my fault, retry later'", got, "mariadb-9c847c79", gotQuery)
	}

	// CONTROL: an empty ReleaseName is not sent AT ALL. It used to go out with the parameter
	// omitted, so that chart-inspector's 400 fired and named it; since #325 a 400 is how this
	// client learns of a values-schema violation, and so Validate refuses the request before it
	// leaves (see TestValidatePreemptsEveryParameterCheck400 for the whole set).
	const notCalled = "<no request reached the server>"
	gotQuery = notCalled
	_, err := ci.Resources(context.Background(), Parameters{
		CompositionName:                "mariadb",
		CompositionNamespace:           "openstack",
		CompositionVersion:             "v0-3-28",
		CompositionResource:            "mariadbs",
		CompositionDefinitionName:      "mariadb",
		CompositionDefinitionNamespace: "krateo-system",
	})
	var missing *MissingParameterError
	if !errors.As(err, &missing) || missing.Name != "releaseName" {
		t.Errorf("Resources with no ReleaseName returned %v; want a *MissingParameterError naming "+
			"releaseName", err)
	}
	if gotQuery != notCalled {
		t.Errorf("a request reached the server with query %q; an empty ReleaseName must be refused "+
			"before it is sent", gotQuery)
	}
}

// queryValue reads one key out of a raw query without net/url, so the test cannot pass because it
// parsed the same way the code under test does.
func queryValue(raw, key string) string {
	for _, pair := range splitAmp(raw) {
		for i := 0; i < len(pair); i++ {
			if pair[i] == '=' {
				if pair[:i] == key {
					return unescapePlus(pair[i+1:])
				}
				break
			}
		}
	}
	return ""
}

func splitAmp(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '&' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

func unescapePlus(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] == '+' {
			b[i] = ' '
		}
	}
	return string(b)
}

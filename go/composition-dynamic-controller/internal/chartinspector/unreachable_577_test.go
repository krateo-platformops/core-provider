package chartinspector

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

var params577 = Parameters{
	CompositionName:                "mariadb",
	CompositionNamespace:           "openstack",
	CompositionVersion:             "v0-3-28",
	CompositionResource:            "mariadbs",
	CompositionDefinitionName:      "mariadb",
	CompositionDefinitionNamespace: "krateo-system",
	ReleaseName:                    "mariadb-9c847c79",
}

// inspect calls Resources on a client for url.
func inspect(url string) ([]Resource, error) {
	ci := NewChartInspector(url)
	return ci.Resources(context.Background(), params577)
}

// cutAfter answers 200 promising len(body)+100 bytes, writes body, and drops the connection: the
// stream a chart-inspector killed mid-response leaves behind.
func cutAfter(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			panic(err)
		}
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: " +
			strconv.Itoa(len(body)+100) + "\r\n\r\n" + body)
		_ = buf.Flush()
		_ = conn.Close()
	}
}

// #577. Which failures are "chart-inspector gave no answer", the only class the cdc keeps Ready
// through. Every case runs a real server over a real transport: the classification is about what the
// network does, so a hand-built error would assert the test's own idea of it.
func TestResources_UnreachableIsOnlyANoAnswer(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close()

	cases := []struct {
		name        string
		url         string
		handler     http.Handler
		unreachable bool
		status      bool // a *StatusError
	}{
		{name: "connection refused (the roll on 057)", url: closedURL, unreachable: true},
		{name: "stream cut inside an element", handler: cutAfter(`[{"group":"","version":"v1","res`), unreachable: true},
		{name: "stream cut between elements", handler: cutAfter(`[{"group":"","version":"v1","resource":"configmaps"},`), unreachable: true},
		{name: "stream cut right after a complete element", handler: cutAfter(`[{"group":"","version":"v1","resource":"configmaps"}`), unreachable: true},
		{name: "200 that is not JSON: an answer, a wrong one", handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("<html>proxy error</html>"))
		})},
		{name: "control: 500 is a verdict", status: true, handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, `{"message":"render failed"}`, http.StatusInternalServerError)
		})},
		{name: "control: 503 from a proxy is a verdict too", status: true, handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "no healthy upstream", http.StatusServiceUnavailable)
		})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			url := tc.url
			if tc.handler != nil {
				srv := httptest.NewServer(tc.handler)
				defer srv.Close()
				url = srv.URL
			}
			res, err := inspect(url)
			if err == nil {
				t.Fatalf("Resources returned %d resources and no error", len(res))
			}
			if got := IsUnreachable(err); got != tc.unreachable {
				t.Errorf("IsUnreachable = %v, want %v: %v", got, tc.unreachable, err)
			}
			if _, got := AsStatusError(err); got != tc.status {
				t.Errorf("StatusError = %v, want %v: %v", got, tc.status, err)
			}
		})
	}
}

// The closing ']' is required. dec.More() is false at an early end too, so a response that ENDED
// cleanly after some elements used to come back as the complete list, and the cdc generated RBAC
// from the part before the cut. A clean end is the server's answer (so not unreachable), and wrong.
func TestResources_AnArrayThatDoesNotEndIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Ends right after a COMPLETE element: no trailing comma for the next Decode to trip on, so
		// only the closing-']' check can refuse it (a trailing comma fails in Decode by itself).
		_, _ = w.Write([]byte(`[{"group":"","version":"v1","resource":"configmaps"}`))
	}))
	defer srv.Close()
	res, err := inspect(srv.URL)
	if err == nil {
		t.Fatalf("a truncated array returned %d resources as the whole answer: %+v", len(res), res)
	}
	if IsUnreachable(err) {
		t.Errorf("a cleanly ended response read as unreachable: %v", err)
	}

	// CONTROL: the same element, closed, is one resource.
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"group":"","version":"v1","resource":"configmaps"}]`))
	}))
	defer ok.Close()
	res, err = inspect(ok.URL)
	if err != nil || len(res) != 1 {
		t.Fatalf("control: a complete array gave %d resources, err %v", len(res), err)
	}
}

package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// #697. The header is how the cdc learns this build's version; the literal is a contract with it
// (go/composition-dynamic-controller/internal/chartinspector pins the same string).
func TestWithVersionHeader(t *testing.T) {
	if VersionHeader != "X-Chart-Inspector-Version" {
		t.Fatalf("VersionHeader = %q: the cdc reads this exact name", VersionHeader)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/resources", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	mux.HandleFunc("/healthz", func(http.ResponseWriter, *http.Request) {})
	srv := httptest.NewServer(WithVersionHeader("2.13.131", mux))
	defer srv.Close()

	// the success path, a failure, and a path no route serves: all carry it.
	for _, path := range []string{"/healthz", "/resources", "/nope"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if got := resp.Header.Get(VersionHeader); got != "2.13.131" {
			t.Errorf("%s (%d): %s = %q, want 2.13.131", path, resp.StatusCode, VersionHeader, got)
		}
	}
}

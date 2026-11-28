package chartinspector

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// #697. The header name is a contract with chart-inspector (internal/handlers.VersionHeader), which
// pins the same literal.
func TestVersionHeaderIsTheContract(t *testing.T) {
	if VersionHeader != "X-Chart-Inspector-Version" {
		t.Fatalf("VersionHeader = %q", VersionHeader)
	}
}

func TestVersionReadsTheHeaderFromHealthz(t *testing.T) {
	for _, tc := range []struct{ name, sent, want string }{{"sent", "2.13.131", "2.13.131"}, {"an older inspector sends none", "", ""}} {
		t.Run(tc.name, func(t *testing.T) {
			var path string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				path = r.URL.Path
				if tc.sent != "" {
					w.Header().Set(VersionHeader, tc.sent)
				}
			}))
			defer srv.Close()
			ci := NewChartInspector(srv.URL)
			got, err := ci.Version(context.Background())
			if err != nil || got != tc.want {
				t.Fatalf("Version = %q, %v; want %q", got, err, tc.want)
			}
			if path != "/healthz" {
				t.Errorf("asked %q, want /healthz: a probe must not cost a render", path)
			}
		})
	}
}

func TestVersionOfAnUnreachableInspectorIsAnUnreachableError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	ci := NewChartInspector(srv.URL)
	if _, err := ci.Version(context.Background()); !IsUnreachable(err) {
		t.Fatalf("err = %v, want an unreachable error", err)
	}
}

func TestResourcesReportsTheVersionHeaderOfEveryResponse(t *testing.T) {
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(VersionHeader, "2.13.131")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	params := Parameters{CompositionName: "a", CompositionNamespace: "b", CompositionVersion: "v1", CompositionResource: "r",
		CompositionDefinitionName: "d", CompositionDefinitionNamespace: "n", ReleaseName: "rel"}
	for _, code := range []int{http.StatusOK, http.StatusInternalServerError} {
		status = code
		var seen []string
		ci := NewChartInspector(srv.URL)
		ci.WithVersionObserver(func(v string) { seen = append(seen, v) })
		_, _ = ci.Resources(context.Background(), params)
		if len(seen) != 1 || seen[0] != "2.13.131" {
			t.Errorf("status %d: observer saw %v, want [2.13.131]", code, seen)
		}
	}
}

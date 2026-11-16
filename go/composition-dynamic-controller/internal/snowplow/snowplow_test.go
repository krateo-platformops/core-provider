package snowplow

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestResolve(t *testing.T) {
	var gotPath, gotQuery, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotAuth = r.Header.Get("Authorization")
		// snowplow returns the resolved RESTAction CR; .status carries the keyed .api map.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"apiVersion": "templates.krateo.io/v1",
			"kind":       "RESTAction",
			"metadata":   map[string]any{"name": "status-sources", "namespace": "demo"},
			"status": map[string]any{
				"svc":    map[string]any{"items": []any{map[string]any{"status": map[string]any{"ip": "1.2.3.4"}}}},
				"health": map[string]any{"status": "UP"},
			},
		})
	}))
	defer srv.Close()

	c := New(srv.URL, func(context.Context) (string, error) { return "jwt-123", nil })
	res, err := c.Resolve(context.Background(),
		ApiRef{Name: "status-sources", Namespace: "demo"},
		map[string]any{"compositionId": "9b1c", "namespace": "apps"},
	)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if gotPath != "/call" {
		t.Errorf("path = %q, want /call", gotPath)
	}
	if gotAuth != "Bearer jwt-123" {
		t.Errorf("auth = %q", gotAuth)
	}
	for _, want := range []string{"resource=restactions", "name=status-sources", "namespace=demo", "apiVersion="} {
		if !containsQuery(gotQuery, want) {
			t.Errorf("query %q missing %q", gotQuery, want)
		}
	}
	if !containsQuery(gotQuery, "extras=") {
		t.Errorf("extras not sent: %q", gotQuery)
	}

	// the keyed .api map is returned
	api := res.API
	wantHealth := map[string]any{"status": "UP"}
	if h, _ := api["health"].(map[string]any); !reflect.DeepEqual(h, wantHealth) {
		t.Errorf("api.health = %v, want %v", api["health"], wantHealth)
	}
	if _, ok := api["svc"]; !ok {
		t.Errorf("api.svc missing: %v", api)
	}
}

func TestResolve_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "you lack permission", http.StatusForbidden)
	}))
	defer srv.Close()
	c := New(srv.URL, func(context.Context) (string, error) { return "t", nil })
	_, err := c.Resolve(context.Background(), ApiRef{Name: "x", Namespace: "y"}, nil)
	if err == nil {
		t.Fatal("expected an error on 403")
	}
}

func TestResolve_RequiresRef(t *testing.T) {
	c := New("http://x", nil)
	if _, err := c.Resolve(context.Background(), ApiRef{Name: "x"}, nil); err == nil {
		t.Error("expected error for missing namespace")
	}
}

func containsQuery(q, sub string) bool {
	return len(q) >= len(sub) && (q == sub || indexOf(q, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

package chartinspector

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

func TestNewChartInspector(t *testing.T) {
	server := "http://localhost:8080"
	inspector := NewChartInspector(server)

	if inspector.server != server {
		t.Errorf("expected server %s, got %s", server, inspector.server)
	}
	if inspector.httpClient == nil {
		t.Error("expected non-nil http client")
	}
	// The client must carry an otelhttp transport so the outbound request to
	// chart-inspector injects the W3C traceparent header from the active span.
	if _, ok := inspector.httpClient.Transport.(*otelhttp.Transport); !ok {
		t.Errorf("expected otelhttp transport, got %T", inspector.httpClient.Transport)
	}
}

func TestChartInspector_WithHTTPClient(t *testing.T) {
	inspector := NewChartInspector("http://localhost:8080")
	customClient := &http.Client{}

	inspector.WithHTTPClient(customClient)

	if inspector.httpClient != customClient {
		t.Error("expected custom http client to be set")
	}
}

func TestChartInspector_WithServer(t *testing.T) {
	inspector := NewChartInspector("http://localhost:8080")
	newServer := "http://example.com"

	inspector.WithServer(newServer)

	if inspector.server != newServer {
		t.Errorf("expected server %s, got %s", newServer, inspector.server)
	}
}

func TestChartInspector_Validate(t *testing.T) {
	inspector := NewChartInspector("http://localhost:8080")

	tests := []struct {
		name    string
		params  Parameters
		wantErr bool
		errMsg  string
	}{
		{
			name: "valid parameters",
			params: Parameters{
				CompositionName:                "test-composition",
				CompositionNamespace:           "test-namespace",
				CompositionVersion:             "v1",
				CompositionResource:            "compositions",
				CompositionDefinitionName:      "test-def",
				CompositionDefinitionNamespace: "test-def-namespace",
				ReleaseName:                    "test-release",
			},
			wantErr: false,
		},
		{
			name:    "missing composition name",
			params:  Parameters{},
			wantErr: true,
			errMsg:  "compositionName is required",
		},
		{
			name: "missing composition namespace",
			params: Parameters{
				CompositionName: "test",
			},
			wantErr: true,
			errMsg:  "compositionNamespace is required",
		},
		{
			name: "missing composition version",
			params: Parameters{
				CompositionName:      "test",
				CompositionNamespace: "test-ns",
			},
			wantErr: true,
			errMsg:  "compositionVersion is required",
		},
		{
			name: "missing composition resource",
			params: Parameters{
				CompositionName:      "test",
				CompositionNamespace: "test-ns",
				CompositionVersion:   "v1",
			},
			wantErr: true,
			errMsg:  "compositionResource is required",
		},
		{
			name: "missing composition definition name",
			params: Parameters{
				CompositionName:      "test",
				CompositionNamespace: "test-ns",
				CompositionVersion:   "v1",
				CompositionResource:  "compositions",
			},
			wantErr: true,
			errMsg:  "compositionDefinitionName is required",
		},
		{
			name: "missing composition definition namespace",
			params: Parameters{
				CompositionName:           "test",
				CompositionNamespace:      "test-ns",
				CompositionVersion:        "v1",
				CompositionResource:       "compositions",
				CompositionDefinitionName: "test-def",
			},
			wantErr: true,
			errMsg:  "compositionDefinitionNamespace is required",
		},
		{
			name: "missing release name",
			params: Parameters{
				CompositionName:                "test",
				CompositionNamespace:           "test-ns",
				CompositionVersion:             "v1",
				CompositionResource:            "compositions",
				CompositionDefinitionName:      "test-def",
				CompositionDefinitionNamespace: "test-def-ns",
			},
			wantErr: true,
			errMsg:  "releaseName is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := inspector.Validate(tt.params)
			if tt.wantErr {
				if err == nil {
					t.Error("expected error but got none")
				} else if err.Error() != tt.errMsg {
					t.Errorf("expected error %s, got %s", tt.errMsg, err.Error())
				}
			} else if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestChartInspector_Resources(t *testing.T) {
	tests := []struct {
		name           string
		params         Parameters
		serverResponse string
		statusCode     int
		wantErr        bool
		expectedLen    int
	}{
		{
			name: "successful request",
			params: Parameters{
				CompositionName:                "test-composition",
				CompositionNamespace:           "test-namespace",
				CompositionVersion:             "v1",
				CompositionResource:            "compositions",
				CompositionDefinitionName:      "test-def",
				CompositionDefinitionNamespace: "test-def-namespace",
				ReleaseName:                    "test-release",
			},
			serverResponse: `[{"group":"apps","version":"v1","resource":"deployments","name":"test","namespace":"default"}]`,
			statusCode:     http.StatusOK,
			wantErr:        false,
			expectedLen:    1,
		},
		{
			name: "server error",
			params: Parameters{
				CompositionName:                "test-composition",
				CompositionNamespace:           "test-namespace",
				CompositionVersion:             "v1",
				CompositionResource:            "compositions",
				CompositionDefinitionName:      "test-def",
				CompositionDefinitionNamespace: "test-def-namespace",
				ReleaseName:                    "test-release",
			},
			serverResponse: "internal server error",
			statusCode:     http.StatusInternalServerError,
			wantErr:        true,
		},
		{
			name: "invalid json response",
			params: Parameters{
				CompositionName:                "test-composition",
				CompositionNamespace:           "test-namespace",
				CompositionVersion:             "v1",
				CompositionResource:            "compositions",
				CompositionDefinitionName:      "test-def",
				CompositionDefinitionNamespace: "test-def-namespace",
				ReleaseName:                    "test-release",
			},
			serverResponse: "invalid json",
			statusCode:     http.StatusOK,
			wantErr:        true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/resources" {
					t.Errorf("expected path /resources, got %s", r.URL.Path)
				}
				if r.Method != http.MethodGet {
					t.Errorf("expected GET method, got %s", r.Method)
				}

				w.WriteHeader(tt.statusCode)
				w.Write([]byte(tt.serverResponse))
			}))
			defer server.Close()

			inspector := NewChartInspector(server.URL)
			resources, err := inspector.Resources(context.Background(), tt.params)

			if tt.wantErr {
				if err == nil {
					t.Error("expected error but got none")
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				if len(resources) != tt.expectedLen {
					t.Errorf("expected %d resources, got %d", tt.expectedLen, len(resources))
				}
			}
		})
	}
}

func TestSetQueryIfNotEmpty(t *testing.T) {
	tests := []struct {
		name     string
		key      string
		value    string
		expected bool
	}{
		{
			name:     "non-empty value",
			key:      "test",
			value:    "value",
			expected: true,
		},
		{
			name:     "empty value",
			key:      "test",
			value:    "",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			query := url.Values{}
			setQueryIfNotEmpty(&query, tt.key, tt.value)

			has := query.Has(tt.key)
			if has != tt.expected {
				t.Errorf("expected query to have key %s: %v, got %v", tt.key, tt.expected, has)
			}

			if tt.expected && query.Get(tt.key) != tt.value {
				t.Errorf("expected query value %s, got %s", tt.value, query.Get(tt.key))
			}
		})
	}
}

func TestChartInspector_ResourcesQueryParameters(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		expectedParams := map[string]string{
			"compositionName":                "test-composition",
			"compositionNamespace":           "test-namespace",
			"compositionVersion":             "v1",
			"compositionResource":            "compositions",
			"compositionDefinitionName":      "test-def",
			"compositionDefinitionNamespace": "test-def-namespace",
			"compositionGroup":               "custom.group",
			"releaseName":                    "test-release",
		}

		for key, expected := range expectedParams {
			if got := query.Get(key); got != expected {
				t.Errorf("expected query param %s=%s, got %s", key, expected, got)
			}
		}

		w.WriteHeader(http.StatusOK)
		w.Write([]byte("[]"))
	}))
	defer server.Close()

	inspector := NewChartInspector(server.URL)
	params := Parameters{
		CompositionName:                "test-composition",
		CompositionNamespace:           "test-namespace",
		CompositionVersion:             "v1",
		CompositionResource:            "compositions",
		CompositionDefinitionName:      "test-def",
		CompositionDefinitionNamespace: "test-def-namespace",
		ReleaseName:                    "test-release",
		CompositionGroup:               "custom.group",
	}

	_, err := inspector.Resources(context.Background(), params)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

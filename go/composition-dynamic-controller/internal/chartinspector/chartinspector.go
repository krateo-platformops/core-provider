package chartinspector

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

type Resource struct {
	Group     string `json:"group"`
	Version   string `json:"version"`
	Resource  string `json:"resource"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	// Verbs to grant on this resource. Set by chart-inspector's tracer: [get,list,watch] for a
	// collection read (Helm `lookup` of a set), ["*"] for a named observation. Absent (older
	// inspector) is treated as ["*"] by rbacgen — backward-compatible.
	Verbs []string `json:"verbs,omitempty"`
}

type Parameters struct {
	CompositionName                string `json:"compositionName"`                // The name of the composition. Required.
	CompositionNamespace           string `json:"compositionNamespace"`           // The namespace of the composition. Required.
	CompositionGroup               string `json:"compositionGroup"`               // The group of the composition. Optional, defaults to "composition.krateo.io"
	CompositionVersion             string `json:"compositionVersion"`             // The version of the composition. Required.
	CompositionResource            string `json:"compositionResource"`            // The resource name of the composition. Required.
	CompositionDefinitionName      string `json:"compositionDefinitionName"`      // The name of the composition definition. Required.
	CompositionDefinitionNamespace string `json:"compositionDefinitionNamespace"` // The namespace of the composition definition. Required.
	CompositionDefinitionGroup     string `json:"compositionDefinitionGroup"`     // The group of the composition definition. Optional, defaults to "core.krateo.io"
	CompositionDefinitionVersion   string `json:"compositionDefinitionVersion"`   // The version of the composition definition. Optional, defaults to "v1alpha1"
	CompositionDefinitionResource  string `json:"compositionDefinitionResource"`  // The resource name of the composition definition. Optional, defaults to "compositiondefinitions"
}

type ChartInspectorInterface interface {
	Resources(ctx context.Context, params Parameters) ([]Resource, error)
}

type ChartInspector struct {
	server     string
	httpClient *http.Client
}

var _ ChartInspectorInterface = &ChartInspector{}

func NewChartInspector(server string) ChartInspector {
	// Wrap the default transport with otelhttp so the outbound request to
	// chart-inspector injects the W3C traceparent header (continuing the active
	// reconcile span) and emits a client span. When no global tracer provider /
	// propagator is registered this transport is a cheap pass-through, so the
	// off-path stays byte-identical; the unstructured-runtime installs the W3C
	// propagator unconditionally, so the active reconcile span always propagates.
	httpcli := &http.Client{
		// Rendering a large umbrella (the installer) is a helm dry-run install + a whole-cluster
		// Pass-B lookup; on a fully-deployed cluster it takes ~1 min. At 60s this client timed out and
		// the version upgrade wedged (D9, 2026-07-08). 300s matches chart-inspector's WriteTimeout.
		Timeout:   300 * time.Second,
		Transport: otelhttp.NewTransport(http.DefaultTransport),
	}

	return ChartInspector{server: server, httpClient: httpcli}
}

func (c *ChartInspector) WithHTTPClient(httpClient *http.Client) {
	c.httpClient = httpClient
}

func (c *ChartInspector) WithServer(server string) {
	c.server = server
}

func (c *ChartInspector) Validate(params Parameters) error {
	if params.CompositionName == "" {
		return fmt.Errorf("compositionName is required")
	}
	if params.CompositionNamespace == "" {
		return fmt.Errorf("compositionNamespace is required")
	}
	if params.CompositionVersion == "" {
		return fmt.Errorf("compositionVersion is required")
	}
	if params.CompositionResource == "" {
		return fmt.Errorf("compositionResource is required")
	}
	if params.CompositionDefinitionName == "" {
		return fmt.Errorf("compositionDefinitionName is required")
	}
	if params.CompositionDefinitionNamespace == "" {
		return fmt.Errorf("compositionDefinitionNamespace is required")
	}
	return nil
}

func (c *ChartInspector) Resources(ctx context.Context, params Parameters) ([]Resource, error) {
	if err := c.Validate(params); err != nil {
		return nil, fmt.Errorf("validating parameters: %w", err)
	}
	u, err := url.JoinPath(c.server, "/resources")
	if err != nil {
		return nil, fmt.Errorf("joining server url: %w", err)
	}
	// Use the reconcile ctx so the otelhttp transport injects the traceparent
	// header from the active span, linking the chart-inspector server span.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	query := req.URL.Query()
	setQueryIfNotEmpty(&query, "compositionName", params.CompositionName)
	setQueryIfNotEmpty(&query, "compositionNamespace", params.CompositionNamespace)
	setQueryIfNotEmpty(&query, "compositionDefinitionName", params.CompositionDefinitionName)
	setQueryIfNotEmpty(&query, "compositionDefinitionNamespace", params.CompositionDefinitionNamespace)
	setQueryIfNotEmpty(&query, "compositionDefinitionGroup", params.CompositionDefinitionGroup)
	setQueryIfNotEmpty(&query, "compositionDefinitionVersion", params.CompositionDefinitionVersion)
	setQueryIfNotEmpty(&query, "compositionDefinitionResource", params.CompositionDefinitionResource)
	setQueryIfNotEmpty(&query, "compositionGroup", params.CompositionGroup)
	setQueryIfNotEmpty(&query, "compositionVersion", params.CompositionVersion)
	setQueryIfNotEmpty(&query, "compositionResource", params.CompositionResource)

	req.URL.RawQuery = query.Encode()

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("getting chartinspector: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		bbody, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("reading response body: %w", err)
		}

		return nil, fmt.Errorf("unexpected status code: %d - response body: %s", resp.StatusCode, string(bbody))
	}

	defer resp.Body.Close()

	dec := json.NewDecoder(resp.Body)

	// If inspector returns an array: stream each element
	var resources []Resource
	// expect a JSON array
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("decoding chartinspector response: %w", err)
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '[' {
		// fall back to decode whole payload
		if err := dec.Decode(&resources); err != nil {
			return nil, fmt.Errorf("decoding chartinspector response: %w", err)
		}
	} else {
		for dec.More() {
			var r Resource
			if err := dec.Decode(&r); err != nil {
				return nil, fmt.Errorf("decoding chartinspector element: %w", err)
			}
			resources = append(resources, r)
		}
		// read closing ']'
		_, _ = dec.Token()
	}

	return resources, nil
}

func setQueryIfNotEmpty(query *url.Values, key, value string) {
	if value != "" {
		query.Set(key, value)
	}
}

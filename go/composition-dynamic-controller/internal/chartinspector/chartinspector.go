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

	// ReleaseName is the helm release name the CALLER has resolved for this composition. REQUIRED.
	//
	// It is sent because chart-inspector used to re-derive it from the composition's
	// krateo.io/release-name LABEL, read off the live object it fetches itself — and the caller's
	// value is not always on the live object yet. #303's adoption path is exactly that case: a
	// recreated CR has no label (its absence is what triggers adoption), the cdc resolves the
	// adopted name in memory, and Observe is a pure read that must not persist it (#110, #130). So
	// chart-inspector dry-ran an install with release name "" and helm answered "no name provided",
	// which failed Observe, which meant the runtime never reached Update — the only phase that
	// would have written the label. A deadlock, retried forever (#329).
	//
	// Found by the openstack-ironic-operator-kog session, who constructed the precondition on a
	// throwaway chart and then confirmed the cause by setting the label by hand and changing
	// nothing else: the composition went from a permanent retry loop to Synced=True at revision 3.
	//
	// THE CALLER KNOWS THE NAME. A callee that re-derives what the caller already computed is
	// coupled to the caller's persistence schedule, which is not a thing a callee can see.
	ReleaseName string `json:"releaseName"`
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

// Validate refuses, before it is sent, every request chart-inspector's parameter checks would
// refuse.
//
// The set is chart-inspector's own: its /resources handler answers 400 when any of the first six is
// empty ("missing required query parameters") and, separately, when releaseName is (#329).
// ReleaseName was documented REQUIRED on Parameters and was not checked here, so an empty one went
// over the wire and came back as a 400.
//
// THAT MATTERS BEYOND THE ROUND TRIP (#325). A 400 from chart-inspector is how the cdc learns that a
// chart's values do not satisfy its values.schema.json, and the cdc can read a 400 that way only if
// a parameter-check 400 cannot reach it: chart-inspector's Status body carries reason "BadRequest"
// for both, so nothing in the response tells them apart. What tells them apart is that this client
// never sends the request a parameter check exists to reject. Each failure is a
// *MissingParameterError, so a caller classifies it by type and not as "could not reach".
func (c *ChartInspector) Validate(params Parameters) error {
	for _, p := range []struct{ name, value string }{
		{"compositionName", params.CompositionName},
		{"compositionNamespace", params.CompositionNamespace},
		{"compositionVersion", params.CompositionVersion},
		{"compositionResource", params.CompositionResource},
		{"compositionDefinitionName", params.CompositionDefinitionName},
		{"compositionDefinitionNamespace", params.CompositionDefinitionNamespace},
		{"releaseName", params.ReleaseName},
	} {
		if p.value == "" {
			return &MissingParameterError{Name: p.name}
		}
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
	setQueryIfNotEmpty(&query, "releaseName", params.ReleaseName)

	req.URL.RawQuery = query.Encode()

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("getting chartinspector: %w", err)
	}
	// Closed on EVERY path, as http.Client requires of a caller. This defer used to sit below the
	// non-200 return, so a refused or failed request's body was never closed.
	//
	// WHAT THAT COST IS NARROWER THAN "A LEAK", and it is stated here so nobody repeats the larger
	// claim. The non-200 arm reads the body with io.ReadAll, and both layers under this client
	// release on a read that reaches io.EOF, without a Close: net/http's bodyEOFSignal hands the
	// connection back to the pool (transport.go, readLoop's waitForBodyRead), and otelhttp's
	// wrappedBody ends the client span (otelhttp v0.70.0 transport.go). So the default transport
	// did not lose a connection per non-200. What it relied on was how two implementations behave,
	// not what their contract promises (net/http asks for the body to be read to EOF AND closed),
	// plus the arm happening to read to EOF; a transport that releases only on Close would have held
	// one per refused request.
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		bbody, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("reading response body: %w", err)
		}

		return nil, &StatusError{Code: resp.StatusCode, Body: string(bbody)}
	}

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

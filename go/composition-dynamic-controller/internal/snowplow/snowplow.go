// Package snowplow is a small HTTP client for resolving a RESTAction through snowplow's
// /call endpoint, mirroring the internal/chartinspector pattern (a synchronous in-cluster
// service call during reconcile). snowplow resolves the referenced RESTAction under the
// caller's identity (an authn-issued service JWT, see the Token provider) and returns the
// CR with its .status populated with the keyed `.api.<callName>` response map — which the
// CDC feeds to the status-projection engine as the "api" source.
package snowplow

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// ApiRef identifies the RESTAction to resolve.
type ApiRef struct {
	Name      string
	Namespace string
}

// TokenFunc returns the Bearer token to authenticate to snowplow (an authn-issued service
// JWT). It is called per resolve so a rotated/expired token is refreshed by the provider.
type TokenFunc func(ctx context.Context) (string, error)

// Client resolves RESTActions via snowplow's /call endpoint.
type Client struct {
	server     string
	httpClient *http.Client
	token      TokenFunc
}

// New returns a snowplow client for the given base URL (e.g.
// http://snowplow.krateo-system.svc.cluster.local:8081). token provides the Bearer JWT.
func New(server string, token TokenFunc) *Client {
	return &Client{
		server:     server,
		httpClient: &http.Client{Timeout: 60 * time.Second},
		token:      token,
	}
}

func (c *Client) WithHTTPClient(h *http.Client) *Client { c.httpClient = h; return c }

const restActionAPIVersion = "templates.krateo.io/v1"

// Resolution is a resolved RESTAction: the keyed `.api` response map, and snowplow's verdict on
// its stages. A 200 is not a complete resolve: a stage with continueOnError can fail and its data
// is then simply missing from API, which only Outcomes says.
type Resolution struct {
	API      map[string]any
	Outcomes StageOutcomes
}

// Resolve resolves the referenced RESTAction with the given extras (the per-instance
// request-extras merged over the RESTAction's inline apiRef.extras by snowplow) and returns
// the keyed `.api` response map from the resolved CR's status, with the reply's stage outcomes.
func (c *Client) Resolve(ctx context.Context, ref ApiRef, extras map[string]any) (Resolution, error) {
	if ref.Name == "" || ref.Namespace == "" {
		return Resolution{}, fmt.Errorf("apiRef name and namespace are required")
	}

	u, err := url.JoinPath(c.server, "/call")
	if err != nil {
		return Resolution{}, fmt.Errorf("joining snowplow url: %w", err)
	}
	q := url.Values{}
	q.Set("apiVersion", restActionAPIVersion)
	q.Set("resource", "restactions")
	q.Set("namespace", ref.Namespace)
	q.Set("name", ref.Name)
	if len(extras) > 0 {
		b, err := json.Marshal(extras)
		if err != nil {
			return Resolution{}, fmt.Errorf("encoding extras: %w", err)
		}
		q.Set("extras", string(b))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u+"?"+q.Encode(), nil)
	if err != nil {
		return Resolution{}, fmt.Errorf("creating request: %w", err)
	}
	if c.token != nil {
		tok, err := c.token(ctx)
		if err != nil {
			return Resolution{}, fmt.Errorf("acquiring snowplow token: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return Resolution{}, fmt.Errorf("calling snowplow: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return Resolution{}, fmt.Errorf("snowplow returned %d: %s", resp.StatusCode, string(body))
	}
	outcomes := ParseStageOutcomes(resp.Header)

	// snowplow returns the resolved RESTAction CR; .status is the keyed .api response map.
	var cr struct {
		Status map[string]any `json:"status"`
	}
	if err := json.Unmarshal(body, &cr); err != nil {
		return Resolution{}, fmt.Errorf("decoding snowplow response: %w", err)
	}
	if cr.Status == nil {
		return Resolution{API: map[string]any{}, Outcomes: outcomes}, nil
	}
	return Resolution{API: cr.Status, Outcomes: outcomes}, nil
}

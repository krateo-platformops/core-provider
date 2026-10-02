// Package restactionrbac is core-provider's client for snowplow's dispatch-free
// GET /rbac endpoint (snowplow PR #44). Given a CompositionDefinition's apiRef,
// it returns the read-set — the (group, version, resource, namespace, verb) rows
// the referenced RESTAction's in-cluster calls would touch — which core-provider
// turns into RBAC bound to the per-composition group krateo:cdc:<resource>-<apiVersion>.
//
// It mirrors internal/chartinspector's shape (stdlib net/http + encoding/json):
// the inspection runs under snowplow's own identity, the composition context is
// passed as data, and a partial read-set is never returned — an unresolvable
// RESTAction fails loud as 422, surfaced here as ErrIncomplete.
//
// Design: docs/design/apiref-rbac-generation.md.
package restactionrbac

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// ErrIncomplete is returned when snowplow answers 422 — at least one api[] stage
// could not be enumerated without dispatching (residual ${…}, dependsOn, or a
// non-kube path). The caller must NOT write partial RBAC; it surfaces the
// composition's ApiRefRBACIncomplete condition instead. The 422 body (naming the
// unresolvable stages) is wrapped for diagnostics.
var ErrIncomplete = errors.New("restactionrbac: RESTAction read-set is incomplete (unresolvable stage)")

// Resource is one read-set row: a (group, version, resource, namespace, verb)
// grant the RESTAction needs. Mirrors snowplow's api.Resource verbatim. Verb is
// per row (get|list|<uaf.Verb>); Namespace "" means cluster-scoped.
type Resource struct {
	Group     string `json:"group"`
	Version   string `json:"version"`
	Resource  string `json:"resource"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name,omitempty"`
	Verb      string `json:"verb"`
	// NonReadVerb is snowplow's marker that this row's Verb is NOT a read verb — the row came from
	// a userAccessFilter asking "may this user do X?", not from a read the RESTAction performs
	// (snowplow#179). It was previously absent from this struct, so the flag was silently dropped
	// at decode and the verb was copied into a generated Role verbatim, turning a permission CHECK
	// into a permission GRANT.
	NonReadVerb bool `json:"nonReadVerb,omitempty"`
}

// response is snowplow's GET /rbac 200 body.
type response struct {
	RESTAction struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"restaction"`
	ReadSet []Resource `json:"readSet"`
}

// Params identifies the RESTAction (apiRef) to inspect plus the resolution
// context. Extras is the inline apiRef.extras merged with any per-instance
// context; it seeds the jq root snowplow evaluates api[].path against, exactly
// as the live /call resolution does.
type Params struct {
	APIRefName      string
	APIRefNamespace string
	Extras          map[string]any
}

// TokenFunc returns the Bearer token authenticating to snowplow. snowplow's GET /rbac is gated by
// the same JWT middleware as /call, so an authn-issued service JWT is required (see internal/tools/authn).
type TokenFunc func(context.Context) (string, error)

// Client calls snowplow's GET /rbac. Server is snowplow's base URL (the same
// service core-provider already injects into the CDC config as URL_SNOWPLOW).
type Client struct {
	server     string
	httpClient *http.Client
	token      TokenFunc
}

// New returns a Client for the snowplow base URL.
func New(server string) *Client {
	return &Client{
		server:     server,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// WithToken sets the Bearer-token source for authenticating to /rbac. Without it the request is
// unauthenticated and snowplow answers 401.
func (c *Client) WithToken(fn TokenFunc) *Client {
	c.token = fn
	return c
}

// WithHTTPClient overrides the HTTP client (tests, custom transports).
func (c *Client) WithHTTPClient(h *http.Client) *Client {
	c.httpClient = h
	return c
}

func (p Params) validate() error {
	if p.APIRefName == "" {
		return fmt.Errorf("restactionrbac: apiRefName is required")
	}
	if p.APIRefNamespace == "" {
		return fmt.Errorf("restactionrbac: apiRefNamespace is required")
	}
	return nil
}

// Inspect calls GET /rbac and returns the read-set. It returns ErrIncomplete on
// 422 (unresolvable RESTAction — caller must fail loud, not write partial RBAC)
// and a plain error on any other non-200.
func (c *Client) Inspect(ctx context.Context, params Params) ([]Resource, error) {
	if err := params.validate(); err != nil {
		return nil, err
	}

	u, err := url.JoinPath(c.server, "/rbac")
	if err != nil {
		return nil, fmt.Errorf("restactionrbac: joining server url: %w", err)
	}

	q := url.Values{}
	q.Set("apiRefName", params.APIRefName)
	q.Set("apiRefNamespace", params.APIRefNamespace)
	if len(params.Extras) > 0 {
		extras, err := json.Marshal(params.Extras)
		if err != nil {
			return nil, fmt.Errorf("restactionrbac: encoding extras: %w", err)
		}
		q.Set("extras", string(extras))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u+"?"+q.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("restactionrbac: creating request: %w", err)
	}
	if c.token != nil {
		tok, err := c.token(ctx)
		if err != nil {
			return nil, fmt.Errorf("restactionrbac: obtaining auth token: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("restactionrbac: calling snowplow /rbac: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		var body response
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			return nil, fmt.Errorf("restactionrbac: decoding /rbac response: %w", err)
		}
		return body.ReadSet, nil
	case http.StatusUnprocessableEntity:
		detail, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("%w: %s", ErrIncomplete, string(detail))
	default:
		detail, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("restactionrbac: unexpected status %d from /rbac: %s", resp.StatusCode, string(detail))
	}
}

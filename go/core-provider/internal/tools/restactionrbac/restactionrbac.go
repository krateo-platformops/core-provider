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
	"reflect"
	"sort"
	"strings"
	"time"

	contexttools "github.com/krateo-platformops/core-provider/internal/tools/context"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
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
	// NamespaceUnknown is snowplow's marker that this row reads a NAMESPACED resource whose
	// namespace is not yet determined (snowplow 1.12.38). That is the normal case for this
	// endpoint's caller: core-provider asks BEFORE any composition instance exists, so the stage's
	// namespace interpolation has nothing to resolve against.
	//
	// It exists because an empty Namespace is byte-identical to a genuinely CLUSTER-SCOPED row, and
	// snowplow's dedupe keys on that field, so the two merged and nothing downstream could tell
	// them apart. Generate must therefore NOT read Namespace == "" as "cluster-scoped" on its own
	// — see the deferral there (#164).
	NamespaceUnknown bool `json:"namespaceUnknown,omitempty"`
}

// readSetFields is every JSON key this package models on a read-set row, derived from the struct
// rather than listed by hand so it cannot drift from it.
func readSetFields() map[string]bool {
	out := map[string]bool{}
	t := reflect.TypeOf(Resource{})
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		if name, _, _ := strings.Cut(tag, ","); name != "" && name != "-" {
			out[name] = true
		}
	}
	return out
}

// unknownReadSetFields returns the read-set keys snowplow sent that this package does not model.
//
// This is the instrument the NonReadVerb and NamespaceUnknown gaps both needed and neither had.
// Twice now snowplow has shipped a field that this struct did not decode, and both times the flag
// was dropped in silence and found later by reading the code — once after it had already turned a
// permission CHECK into a permission GRANT (snowplow#179), once before Phase 3 could widen a
// namespaced read to a ClusterRole (#164). Nothing failed, because dropping an unknown JSON key is
// exactly what encoding/json is specified to do.
//
// So the drop is made observable instead of silent. It deliberately does NOT fail the call:
// snowplow adding a field must stay backward compatible, and refusing to decode would turn their
// additive change into our outage. A log line is the right severity — it costs nothing when there
// is nothing to say, and it names the field the next time this happens.
func unknownReadSetFields(raw []byte) []string {
	var probe struct {
		ReadSet []map[string]json.RawMessage `json:"readSet"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil // the typed decode reports the real error
	}
	known, seen := readSetFields(), map[string]bool{}
	var out []string
	for _, row := range probe.ReadSet {
		for k := range row {
			if !known[k] && !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	sort.Strings(out)
	return out
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
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("restactionrbac: reading /rbac response: %w", err)
		}
		var body response
		if err := json.Unmarshal(raw, &body); err != nil {
			return nil, fmt.Errorf("restactionrbac: decoding /rbac response: %w", err)
		}
		if extra := unknownReadSetFields(raw); len(extra) > 0 {
			contexttools.LoggerFromCtx(ctx, logging.NewNopLogger()).Info(
				"snowplow /rbac sent read-set fields this build does not model; they are being dropped at decode",
				"fields", strings.Join(extra, ","), "apiRef", params.APIRefNamespace+"/"+params.APIRefName)
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

// Package authn is a small client for the Kubernetes intra-service auth login strategy: the
// CDC presents its own projected (audience-bound) ServiceAccount token to authn's
// /serviceaccount/login endpoint and receives an authn-issued JWT. That JWT is the Bearer the
// snowplow client uses to resolve RESTActions under the CDC's identity. See the authn design
// doc kubernetes-intra-service-auth.md.
//
// The response body is decoded for `accessToken` and NOTHING ELSE — no clientconfig, no `data`
// envelope. The JWT's claims are what identify the caller downstream; this package never sees
// or needs the clientconfig those claims refer to, and a comment here previously implied it
// received one.
//
// Token() satisfies snowplow.TokenFunc: it reads the projected SA token fresh on every miss
// (projected tokens rotate on disk), exchanges it for a JWT, and caches the JWT until shortly
// before its own expiry so the exchange is amortised across reconciles.
package authn

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"
)

// DefaultTokenPath is the conventional mount path of the projected ServiceAccount token.
// The CDC Deployment projects a token bound to the authn audience at this path.
const DefaultTokenPath = "/var/run/secrets/krateo.io/serviceaccount/token"

// refreshSkew is how long before a cached JWT's expiry we proactively re-exchange, so a token
// is never handed out moments before it would be rejected downstream.
const refreshSkew = 60 * time.Second

// Client exchanges a projected ServiceAccount token for an authn-issued JWT and caches it.
type Client struct {
	server     string
	tokenPath  string
	httpClient *http.Client

	now func() time.Time // injectable clock for tests

	mu     sync.Mutex
	cached string
	expiry time.Time
}

// New returns an authn client for the given base URL (e.g.
// http://authn.krateo-system.svc.cluster.local:8082). tokenPath is the projected SA token
// file; if empty, DefaultTokenPath is used.
func New(server, tokenPath string) *Client {
	if tokenPath == "" {
		tokenPath = DefaultTokenPath
	}
	return &Client{
		server:     server,
		tokenPath:  tokenPath,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		now:        time.Now,
	}
}

func (c *Client) WithHTTPClient(h *http.Client) *Client { c.httpClient = h; return c }

// Token returns a valid authn JWT, exchanging the projected SA token when the cache is empty
// or near expiry. It satisfies snowplow.TokenFunc.
func (c *Client) Token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.cached != "" && c.now().Before(c.expiry.Add(-refreshSkew)) {
		return c.cached, nil
	}

	jwt, exp, err := c.exchange(ctx)
	if err != nil {
		return "", err
	}
	c.cached = jwt
	c.expiry = exp
	return jwt, nil
}

// exchange reads the projected SA token and POSTs it to authn's /serviceaccount/login,
// returning the issued JWT and its expiry.
func (c *Client) exchange(ctx context.Context) (string, time.Time, error) {
	saToken, err := os.ReadFile(c.tokenPath)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("reading projected service account token %q: %w", c.tokenPath, err)
	}

	u, err := url.JoinPath(c.server, "/serviceaccount/login")
	if err != nil {
		return "", time.Time{}, fmt.Errorf("joining authn url: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+string(saToken))
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Never reached authn. On a spoke this is the signature of "no authn installed into this
		// target", which is exactly the case that used to degrade silently (#251).
		return "", time.Time{}, &UnreachableError{Server: c.server, Err: err}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		// Typed, so the caller can distinguish rejected-caller from authn-broke without matching
		// on message text. Carries authn's own details.causes[0].reason when present.
		return "", time.Time{}, parseStatusError(resp.StatusCode, body)
	}

	var out struct {
		AccessToken string `json:"accessToken"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", time.Time{}, fmt.Errorf("decoding authn response: %w", err)
	}
	if out.AccessToken == "" {
		return "", time.Time{}, fmt.Errorf("authn response carried no accessToken")
	}

	// Derive expiry from the JWT's exp claim so caching tracks the real lifetime; fall back to
	// a conservative window if the token is unparseable (still usable, just refreshed sooner).
	exp := jwtExpiry(out.AccessToken)
	if exp.IsZero() {
		exp = c.now().Add(5 * time.Minute)
	}
	return out.AccessToken, exp, nil
}

// jwtExpiry decodes (without verifying) the JWT payload and returns its exp claim as a time.
// The signature is irrelevant here — the CDC only consumes the token, it does not trust it.
// A zero time is returned when the token has no parseable exp.
func jwtExpiry(token string) time.Time {
	parts := splitJWT(token)
	if len(parts) != 3 {
		return time.Time{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp == 0 {
		return time.Time{}
	}
	return time.Unix(claims.Exp, 0)
}

func splitJWT(token string) []string {
	out := make([]string, 0, 3)
	start := 0
	for i := 0; i < len(token); i++ {
		if token[i] == '.' {
			out = append(out, token[start:i])
			start = i + 1
		}
	}
	out = append(out, token[start:])
	return out
}

//go:build e2e

// This is a real end-to-end test of the CDC apiRef chain against a live kind cluster running
// real authn and real snowplow (see hack/apiref-e2e). It exercises the actual CDC client code:
//
//	projected SA token (file) --authn.Client.Token--> /serviceaccount/login (TokenReview -> JWT)
//	  --snowplow.Client.Resolve(Bearer JWT, extras)--> snowplow /call (resolves a RESTAction)
//	  --> the RESTAction echoes the request extras back --> .api.echo.args
//
// proving the JWT and the extras (static + per-instance, request-wins) flow through end to end.
//
// Driven by env (set by the harness):
//
//	APIREF_E2E_AUTHN_URL, APIREF_E2E_SNOWPLOW_URL, APIREF_E2E_TOKEN_PATH,
//	APIREF_E2E_APIREF_NAME, APIREF_E2E_APIREF_NAMESPACE
//
// Run: go test -tags e2e ./internal/composition/ -run TestE2E_ApiRefChain -v
package composition

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/krateo-platformops/composition-dynamic-controller/internal/authn"
	"github.com/krateo-platformops/composition-dynamic-controller/internal/snowplow"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func envOrSkip(t *testing.T, key string) string {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		t.Skipf("%s not set; run via the apiref-e2e harness", key)
	}
	return v
}

func TestE2E_ApiRefChain_AuthnJWT_Snowplow_Extras(t *testing.T) {
	authnURL := envOrSkip(t, "APIREF_E2E_AUTHN_URL")
	snowplowURL := envOrSkip(t, "APIREF_E2E_SNOWPLOW_URL")
	tokenPath := envOrSkip(t, "APIREF_E2E_TOKEN_PATH")
	refName := envOrSkip(t, "APIREF_E2E_APIREF_NAME")
	refNamespace := envOrSkip(t, "APIREF_E2E_APIREF_NAMESPACE")

	// Real CDC clients: authn token provider feeds the snowplow client's Bearer.
	authnClient := authn.New(authnURL, tokenPath)
	snowplowClient := snowplow.New(snowplowURL, authnClient.Token)
	resolver := NewSnowplowAPIResolver(
		snowplowClient,
		snowplow.ApiRef{Name: refName, Namespace: refNamespace},
		map[string]any{"region": "eu"}, // static extras (CompositionDefinition apiRef.extras)
	)

	// A composition instance: its name/namespace/uid become per-instance extras (request-wins).
	mg := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "composition.krateo.io/v1-0-0",
		"kind":       "FireworksApp",
		"metadata": map[string]any{
			"name":      "demo-app",
			"namespace": "apps",
			"uid":       "uid-e2e-123",
		},
	}}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	api, err := resolver.Resolve(ctx, mg)
	if err != nil {
		t.Fatalf("Resolve (real authn JWT + real snowplow): %v", err)
	}
	t.Logf("resolved .api = %#v", api)

	// .api.echo is the echo server's response; .args reflects the templated request extras.
	echo, ok := api["echo"].(map[string]any)
	if !ok {
		t.Fatalf("api.echo missing or wrong type: %#v", api)
	}
	args, ok := echo["args"].(map[string]any)
	if !ok {
		t.Fatalf("api.echo.args missing or wrong type: %#v", echo)
	}

	// go-httpbin returns single query values as strings; tolerate []any too.
	get := func(k string) string {
		switch v := args[k].(type) {
		case string:
			return v
		case []any:
			if len(v) > 0 {
				if s, ok := v[0].(string); ok {
					return s
				}
			}
		}
		return ""
	}

	want := map[string]string{
		"cn":     "demo-app",    // per-instance: compositionName
		"cns":    "apps",        // per-instance: compositionNamespace
		"cid":    "uid-e2e-123", // per-instance: compositionId
		"region": "eu",          // static apiRef.extras
	}
	for k, w := range want {
		if got := get(k); got != w {
			t.Errorf("extra %q round-trip: got %q, want %q (full args: %#v)", k, got, w, args)
		}
	}
}

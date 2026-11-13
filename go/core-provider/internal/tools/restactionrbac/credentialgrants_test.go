package restactionrbac

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// snowplow#1168 (design, 2026-11-13) adds a top-level `credentialGrants` to GET /rbac: the endpoint
// credentials (namespace, name, verb "use") a RESTAction needs an ADMIN to grant. It is advisory and
// must NEVER become a generated Role: granting it from the RESTAction author's refs recreates the
// confused deputy the check exists to close, and a resource-level `use secrets` grant (the read-set
// contract drops names) would grant every credential in the namespace.
//
// So Inspect must return EXACTLY the read set it returns without the field: nothing of
// credentialGrants may reach the rules core-provider renders. This pins today's behaviour (the
// decoder models only restaction and readSet) so that a future change cannot start mapping it.
func TestInspect_CredentialGrantsNeverReachTheReadSet(t *testing.T) {
	readSet := []map[string]any{
		{"group": "", "version": "v1", "resource": "services", "namespace": "apps", "verb": "list"},
	}
	serve := func(withGrants bool) []Resource {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			body := map[string]any{
				"restaction": map[string]string{"name": "ra", "namespace": "krateo-system"},
				"readSet":    readSet,
			}
			if withGrants {
				body["credentialGrants"] = []map[string]any{
					{"group": "", "version": "v1", "resource": "secrets", "namespace": "apps", "name": "endpoint-creds", "verb": "use"},
				}
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(body)
		}))
		defer srv.Close()
		got, err := New(srv.URL).Inspect(context.Background(), Params{APIRefName: "ra", APIRefNamespace: "krateo-system"})
		if err != nil {
			t.Fatalf("Inspect (credentialGrants=%v): %v", withGrants, err)
		}
		return got
	}

	without, with := serve(false), serve(true)
	if !reflect.DeepEqual(with, without) {
		t.Fatalf("credentialGrants changed the read set:\n with    %+v\n without %+v", with, without)
	}
	for _, r := range with {
		if r.Resource == "secrets" || r.Verb == "use" {
			t.Errorf("a credential grant leaked into the read set: %+v", r)
		}
	}
	if len(without) != 1 {
		t.Fatalf("control: the read set itself must decode (got %+v)", without)
	}
}

package compositiondefinitions

import "testing"

// #251 item 4 (Diego, 2026-11-14): the authn a composition controller calls is the one in the
// cluster it runs in. A remote target's spec.authnUrl wins there; core-provider's own URL (the
// chart value installer#578 fills) is used for a local controller and NEVER for a remote one,
// because a remote controller's token is minted by the target's apiserver and the hub's authn
// cannot validate it. Never empty.
func TestControllerAuthnURL(t *testing.T) {
	const local = "http://authn.krateo-system.svc.cluster.local:8082"
	for _, tc := range []struct {
		name                       string
		remote                     bool
		target, localURL, ns, want string
	}{
		{"remote with spec.authnUrl", true, "https://authn.spoke.example:9443", local, "krateo-system", "https://authn.spoke.example:9443"},
		{"remote without: the target's in-cluster authn, never the hub's URL", true, "", "http://authn.hub-only.example:8082", "auth-system", "http://authn.auth-system.svc.cluster.local:8082"},
		{"local: core-provider's own URL", false, "", "http://authn.platform.svc.cluster.local:8082", "krateo-system", "http://authn.platform.svc.cluster.local:8082"},
		{"local ignores a target URL it cannot have", false, "https://authn.spoke.example:9443", local, "krateo-system", local},
		{"local without its own URL: the in-cluster authn", false, "", "", "auth-system", "http://authn.auth-system.svc.cluster.local:8082"},
		{"nothing set: the controller's built-in default", false, "", "", "", "http://authn.krateo-system.svc.cluster.local:8082"},
	} {
		got := controllerAuthnURL(tc.remote, tc.target, tc.localURL, tc.ns)
		if got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
		if got == "" {
			t.Errorf("%s: empty URL_AUTHN would override the controller's default with an empty string", tc.name)
		}
	}
}

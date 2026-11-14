package compositiondefinitions

import "fmt"

// controllerAuthnURL is the authn base URL a composition controller is given (URL_AUTHN), for where
// that controller runs (#251 item 4, Diego 2026-11-14).
//
// The controller exchanges its projected ServiceAccount token, minted by the apiserver of the cluster
// it runs in, for a JWT. Only an authn in that same cluster can validate it, so:
//   - remote target: the KubernetesTarget's spec.authnUrl when set;
//   - local: core-provider's own authn URL (CORE_PROVIDER_AUTHN_URL, chart value apiRefRBAC.authnUrl,
//     which installer#578 fills), since core-provider and the controller share the cluster;
//   - otherwise, in either case, the in-cluster authn Service in the authn namespace, the one
//     core-provider writes the controller's authn mapping into.
//
// Never empty: the controller reads URL_AUTHN with plumbing's env.String, which returns a set-but-
// empty value as "" rather than its default.
func controllerAuthnURL(remote bool, targetAuthnURL, localAuthnURL, authnNamespace string) string {
	if remote && targetAuthnURL != "" {
		return targetAuthnURL
	}
	if !remote && localAuthnURL != "" {
		return localAuthnURL
	}
	if authnNamespace == "" {
		authnNamespace = defaultAuthnNamespace
	}
	return fmt.Sprintf("http://authn.%s.svc.cluster.local:8082", authnNamespace)
}

// defaultAuthnNamespace matches the composition controller's own built-in URL_AUTHN default
// (http://authn.krateo-system.svc.cluster.local:8082), so an install that sets nothing renders the
// URL the controller already used.
const defaultAuthnNamespace = "krateo-system"

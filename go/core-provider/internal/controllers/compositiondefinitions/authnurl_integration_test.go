//go:build integration
// +build integration

package compositiondefinitions

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
)

// assertControllersCarryAuthnURL is #251 item 4 through the controller's REAL deploy path: every
// composition-controller ConfigMap the reconciler wrote carries URL_AUTHN, and for these local
// definitions it is the in-cluster authn in krateo-system. That is the composition controller's own
// built-in default, so an install that sets nothing keeps the URL it had.
//
// The expected value is a literal, not controllerAuthnURL's output, so this cannot agree with the code
// by construction; it is valid only while the test process sets neither input, checked first.
// A controller ConfigMap is recognised by COMPOSITION_CONTROLLER_LEASE_NAME (#505), which only the
// cdc ConfigMap carries; the count of them is printed and must be at least wantAtLeast.
func assertControllersCarryAuthnURL(ctx context.Context, t *testing.T, cfg *envconf.Config, wantAtLeast int) {
	t.Helper()
	if AuthnURL != "" || AuthnNamespace != "krateo-system" {
		t.Fatalf("this assertion assumes no CORE_PROVIDER_AUTHN_URL and the default authn namespace; got %q, %q", AuthnURL, AuthnNamespace)
	}
	const want = "http://authn.krateo-system.svc.cluster.local:8082"
	cs := kubernetes.NewForConfigOrDie(cfg.Client().RESTConfig())
	cms, err := cs.CoreV1().ConfigMaps(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, cm := range cms.Items {
		if _, ok := cm.Data["COMPOSITION_CONTROLLER_LEASE_NAME"]; !ok {
			continue
		}
		n++
		if got, ok := cm.Data["URL_AUTHN"]; !ok || got != want {
			t.Errorf("controller ConfigMap %s/%s: URL_AUTHN = %q (present %v), want %q", cm.Namespace, cm.Name, got, ok, want)
		}
	}
	t.Logf("controller ConfigMaps checked for URL_AUTHN: %d", n)
	if n < wantAtLeast {
		t.Fatalf("found %d controller ConfigMaps, want at least %d: the check above ran over too few to mean anything", n, wantAtLeast)
	}
}

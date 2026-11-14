package templates

import (
	"strings"
	"testing"
)

func TestControllerLeaseName(t *testing.T) {
	if got, want := ControllerLeaseName("composition.krateo.io", "v1-0-0", "fireworksapps"), "v1-0-0.fireworksapps.composition.krateo.io"; got != want {
		t.Errorf("ControllerLeaseName = %q, want %q", got, want)
	}
	if v := Values(Renderoptions{Group: "composition.krateo.io", Version: "v1-0-0", Resource: "fireworksapps"})["controllerLease"]; v != "v1-0-0.fireworksapps.composition.krateo.io" {
		t.Errorf("Values()[controllerLease] = %v: the ConfigMap and the Role read the lease name from here", v)
	}

	// Over a DNS subdomain's 253: the version is kept and the rest hashed, still unique per Kind.
	long := strings.Repeat("a", 240) + ".example.io"
	a, b := ControllerLeaseName(long, "v1-0-0", "things"), ControllerLeaseName(long, "v1-0-0", "others")
	if len(a) > 253 || !strings.HasPrefix(a, "v1-0-0.") || a == b {
		t.Errorf("long names: %q (%d), %q; want <= 253, version kept, distinct per Kind", a, len(a), b)
	}
	if ControllerLeaseName(long, "v1-0-0", "things") == ControllerLeaseName(long, "v2-0-0", "things") {
		t.Error("two versions of one Kind must not share a lease")
	}
}

//go:build envtest

package servedkinds

import (
	"os"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// #589 against a real API server, through the mapper the manager uses (controller-runtime's
// default MapperProvider, apiutil.NewDynamicRESTMapper). That mapper does not return the plain
// meta.NoKindMatchError the unit test's DefaultRESTMapper does: with aggregated discovery a group
// the server does not serve comes back as an ErrResourceDiscoveryFailed wrapping a
// NoResourceMatchError. Check must still classify it as not served.
//
// CONTROL: the same check after installing the generated CRDs from crds/ must pass, on a fresh
// mapper AND on the one that already saw them missing, so the first arm measures "not installed",
// not a mapper that cannot resolve anything.
func TestCheckAgainstAnAPIServer(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Fatal("KUBEBUILDER_ASSETS is not set: this test exists to run against a real apiserver, and a skip would read as a pass")
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest apiserver: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	newMapper := func() meta.RESTMapper {
		hc, err := rest.HTTPClientFor(cfg)
		if err != nil {
			t.Fatal(err)
		}
		m, err := apiutil.NewDynamicRESTMapper(cfg, hc)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}

	seenMissing := newMapper()
	err = Check(seenMissing, Required, "2.13.114")
	if !IsNotServed(err) {
		t.Fatalf("no CRDs installed: Check = %v (%T), want *NotServedError", err, err)
	}
	if ns := err.(*NotServedError); len(ns.Missing) != len(Required) {
		t.Errorf("no CRDs installed: Missing = %v, want all %d Required Kinds", ns.Missing, len(Required))
	}
	t.Logf("before install: %v", err)

	if _, err := envtest.InstallCRDs(cfg, envtest.CRDInstallOptions{Paths: []string{"../../../crds"}, ErrorIfPathMissing: true}); err != nil {
		t.Fatalf("install the generated CRDs: %v", err)
	}
	if err := Check(newMapper(), Required, "2.13.114"); err != nil {
		t.Errorf("CRDs installed, fresh mapper: Check = %v, want nil", err)
	}
	if err := Check(seenMissing, Required, "2.13.114"); err != nil {
		t.Errorf("CRDs installed, the mapper that saw them missing: Check = %v, want nil", err)
	}
}

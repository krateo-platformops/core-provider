//go:build envtest

package crd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/krateo-platformops/core-provider/internal/tools/crd/generation"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// bigSpecSchema is a values schema whose generated CRD version is roughly targetBytes, the size that
// matters on krateo-057 (installers: ~600 KB per served version, so two fit etcd's default 1.5 MiB
// request limit and three do not).
func bigSpecSchema(targetBytes int) []byte {
	var b strings.Builder
	b.WriteString(`{"type":"object","properties":{`)
	for i := 0; b.Len() < targetBytes; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `"field%06d":{"type":"string","description":"%s"}`, i, strings.Repeat("d", 80))
	}
	b.WriteString(`}}`)
	return []byte(b.String())
}

// #484 on a REAL apiserver and etcd. Control: adding a third ~600 KB version to a CRD that serves two
// is refused exactly as on krateo-057 ("etcdserver: request is too large"). Then the same add with
// the stale version in drop succeeds, in ONE spec write (generation +1), leaving the current and the
// new version served and the dropped one gone from spec.versions and status.storedVersions.
func TestApplyOrUpdateCRDDroppingKeepsTheWriteUnderTheEtcdLimitEnvtest(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Fatal("KUBEBUILDER_ASSETS is not set: this test exists to run against a real apiserver, and a skip would read as a pass")
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	sc := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(sc)
	_ = apiextensionsv1.AddToScheme(sc)
	cl, err := client.New(cfg, client.Options{Scheme: sc})
	if err != nil {
		t.Fatal(err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	gen := func(version string) *apiextensionsv1.CustomResourceDefinition {
		t.Helper()
		c, err := generation.GenerateCRD(bigSpecSchema(560000), schema.GroupVersionKind{Group: "composition.krateo.io", Version: version, Kind: "Installer"})
		if err != nil {
			t.Fatalf("GenerateCRD %s: %v", version, err)
		}
		return c
	}
	v1, v2, v3 := gen("v0-3-633"), gen("v0-3-634"), gen("v0-3-635")
	for _, c := range []*apiextensionsv1.CustomResourceDefinition{v1, v2} {
		if _, err := ApplyOrUpdateCRD(ctx, cl, dyn, c); err != nil {
			t.Fatalf("setup: applying %s: %v", c.Spec.Versions[0].Name, err)
		}
	}
	get := func() *apiextensionsv1.CustomResourceDefinition {
		t.Helper()
		got := &apiextensionsv1.CustomResourceDefinition{}
		if err := cl.Get(ctx, types.NamespacedName{Name: v1.Name}, got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	served := func(c *apiextensionsv1.CustomResourceDefinition) []string {
		var out []string
		for _, v := range c.Spec.Versions {
			if v.Served {
				out = append(out, v.Name)
			}
		}
		return out
	}
	before := get()
	if got := fmt.Sprint(served(before)); got != "[v0-3-633 v0-3-634]" {
		t.Fatalf("setup: served = %s, want the two-version 057 state", got)
	}

	// Control: the three-version write is refused by etcd, as on 057.
	if _, err := ApplyOrUpdateCRD(ctx, cl, dyn, gen("v0-3-635")); err == nil || !strings.Contains(err.Error(), "request is too large") {
		t.Fatalf("control: adding a third ~600 KB version must be refused 'request is too large' (the 057 wedge), got %v", err)
	}
	if got := fmt.Sprint(served(get())); got != "[v0-3-633 v0-3-634]" {
		t.Fatalf("control: the refused write changed the CRD: served = %s", got)
	}

	// Refused outright: dropping the version being applied is a caller bug.
	if _, err := ApplyOrUpdateCRDDropping(ctx, cl, dyn, gen("v0-3-635"), map[string]bool{"v0-3-635": true}); err == nil {
		t.Error("dropping the version being applied was accepted")
	}

	gen0 := get().Generation
	if _, err := ApplyOrUpdateCRDDropping(ctx, cl, dyn, v3, map[string]bool{"v0-3-634": true}); err != nil {
		t.Fatalf("the add with the stale version dropped in the same write failed: %v", err)
	}
	after := get()
	if got := fmt.Sprint(served(after)); got != "[v0-3-633 v0-3-635]" {
		t.Errorf("served = %s, want [v0-3-633 v0-3-635]: the current version kept, the stale one dropped, the new one added", got)
	}
	if after.Generation != gen0+1 {
		t.Errorf("generation went %d -> %d, want exactly one spec write", gen0, after.Generation)
	}
	for _, sv := range after.Status.StoredVersions {
		if sv == "v0-3-634" {
			t.Errorf("storedVersions still names the dropped version: %v", after.Status.StoredVersions)
		}
	}
}

//go:build envtest

// core-provider#669 for the one CompositionDefinition status core-provider writes outside its
// reconciler: a remote target's shadow definition (remoteseed.go). Against a real apiserver, with the
// CompositionDefinition CRD core-provider projects onto a spoke (the embedded asset), so a field that
// CRD does not declare is pruned and reads back absent.
//
//	Run: KUBEBUILDER_ASSETS=$(setup-envtest use 1.36.0 -p path) \
//	go test -tags envtest ./internal/tools/deploy/ -run RemoteSeedWrittenByEnvtest -v
package deploy

import (
	"context"
	"os"
	"testing"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/tools/writtenby"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestRemoteSeedWrittenByEnvtest(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Fatal("KUBEBUILDER_ASSETS is not set: this test exists to run against a real apiserver, and a skip would read as a pass")
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	kube, err := client.New(cfg, client.Options{Scheme: clientgoscheme.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	dyn := dynamic.NewForConfigOrDie(cfg)
	ctx := context.Background()

	seed := func(w compositiondefinitionsv1alpha1.WrittenBy) {
		t.Helper()
		if err := SeedRemoteTarget(ctx, kube, dyn, RemoteSeedOptions{
			Namespace: "team-a", CDName: "app-a",
			Chart:      &compositiondefinitionsv1alpha1.ChartInfo{Url: "oci://ghcr.io/example/krateo/demo", Version: "1.0.0"},
			PackageURL: "oci://ghcr.io/example/krateo/demo:1.0.0",
			APIVersion: "composition.krateo.io/v1-0-0", Kind: "Demo", Resource: "demos",
			WrittenBy: w,
		}); err != nil {
			t.Fatalf("SeedRemoteTarget: %v", err)
		}
	}
	shadow := func() (map[string]string, string) {
		t.Helper()
		u, err := dyn.Resource(compositionDefinitionGVR).Namespace("team-a").Get(ctx, "app-a", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		w, _, _ := unstructured.NestedStringMap(u.Object, "status", "writtenBy")
		if got, _, _ := unstructured.NestedString(u.Object, "status", "packageUrl"); got == "" {
			t.Fatal("control: the shadow has no status.packageUrl, so its status was never written")
		}
		return w, u.GetResourceVersion()
	}
	want := func(w compositiondefinitionsv1alpha1.WrittenBy) map[string]string {
		return map[string]string{"name": w.Name, "version": w.Version, "commit": w.Commit}
	}

	// The expectations are literals, never built with writtenby.New: a stamp constructor that dropped its
	// arguments would otherwise build the expectation as wrongly as the stamp, and agree with it.
	first, wantFirst := writtenby.New("2.13.135", "c0ffee135"), compositiondefinitionsv1alpha1.WrittenBy{Name: "core-provider", Version: "2.13.135", Commit: "c0ffee135"}
	seed(first)
	got, rv := shadow()
	if len(got) != 3 || got["name"] != "core-provider" || got["version"] != wantFirst.Version || got["commit"] != wantFirst.Commit {
		t.Fatalf("after the first seed: status.writtenBy = %v, want %v", got, want(wantFirst))
	}

	// The seed re-writes the shadow on every Observe of a remote definition; the same binary writes
	// back what is stored and the apiserver records nothing.
	seed(first)
	if _, again := shadow(); again != rv {
		t.Errorf("a re-seed by the same binary moved the shadow's resourceVersion %s -> %s", rv, again)
	}

	next, wantNext := writtenby.New("2.13.136", "c0ffee136"), compositiondefinitionsv1alpha1.WrittenBy{Name: "core-provider", Version: "2.13.136", Commit: "c0ffee136"}
	seed(next)
	if got, _ := shadow(); got["version"] != wantNext.Version || got["commit"] != wantNext.Commit {
		t.Errorf("after a seed by the next binary: status.writtenBy = %v, want %v", got, want(wantNext))
	}
}

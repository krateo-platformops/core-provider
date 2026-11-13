//go:build envtest

package compositiondefinitions

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/krateo-platformops/core-provider/apis"
	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// #454, through the REAL prune (Update's pruneStaleServedVersions) on a real apiserver: a served
// version that became prune-eligible stays served until it has been eligible for the grace window,
// and is pruned once it has. A control first proves the version really is prunable, so "still
// served" is the window and not an eligibility the test failed to set up.
func TestEnvtest_PruneWaitsTheGraceWindow(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Fatal("KUBEBUILDER_ASSETS is not set: this test exists to run against a real apiserver, and a skip would read as a pass")
	}
	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "..", "crds")}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	sc := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(sc)
	_ = apiextensionsv1.AddToScheme(sc)
	_ = apis.AddToScheme(sc)
	cl, err := client.New(cfg, client.Options{Scheme: sc})
	if err != nil {
		t.Fatal(err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	curGVK, gvrV1, _, crdName := buildTwoVersionCRD(t, ctx, cl, dyn)
	e := newExternal(cl, dyn)
	self := &compositiondefinitionsv1alpha1.CompositionDefinition{ObjectMeta: metav1.ObjectMeta{Name: "self", Namespace: "default"}}

	served := func() bool {
		t.Helper()
		got := &apiextensionsv1.CustomResourceDefinition{}
		if err := cl.Get(ctx, types.NamespacedName{Name: crdName}, got); err != nil {
			t.Fatalf("get CRD: %v", err)
		}
		for _, v := range got.Spec.Versions {
			if v.Name == "v1-0-0" {
				return true
			}
		}
		return false
	}

	// Control: v1-0-0 has no instances, references or releases, so it IS prune-eligible.
	prunable, _, err := e.prunableServedVersions(ctx, curGVK, gvrV1, "self", "default")
	if err != nil || !contains(prunable, "v1-0-0") {
		t.Fatalf("control: v1-0-0 must be prunable before the window is tested (prunable=%v err=%v)", prunable, err)
	}

	// First pass: eligible, but the window has not run. Still served, clock recorded.
	if err := e.pruneStaleServedVersions(ctx, self, curGVK, gvrV1); err != nil {
		t.Fatal(err)
	}
	if !served() {
		t.Fatal("v1-0-0 was pruned on first sight: the grace window did not hold (the #454 race)")
	}
	rec := self.Status.ServedVersionsPendingPrune
	if len(rec) != 1 || rec[0].Version != "v1-0-0" || time.Since(rec[0].EligibleSince.Time) > time.Minute {
		t.Fatalf("the first pass must record v1-0-0's clock, got %v", rec)
	}
	first := rec[0].EligibleSince

	// A second pass inside the window: still served, and the clock is NOT restarted.
	if err := e.pruneStaleServedVersions(ctx, self, curGVK, gvrV1); err != nil {
		t.Fatal(err)
	}
	if !served() || !self.Status.ServedVersionsPendingPrune[0].EligibleSince.Equal(&first) {
		t.Fatalf("inside the window: served=%v clock=%v, want served and the original clock %v", served(), self.Status.ServedVersionsPendingPrune, first)
	}

	// The window has elapsed (as after servedVersionPruneGrace): now it is pruned.
	self.Status.ServedVersionsPendingPrune[0].EligibleSince = metav1.NewTime(time.Now().Add(-servedVersionPruneGrace - time.Second))
	if err := e.pruneStaleServedVersions(ctx, self, curGVK, gvrV1); err != nil {
		t.Fatal(err)
	}
	if served() {
		t.Error("v1-0-0 stayed served after its window elapsed: the prune must happen once it is due")
	}
}

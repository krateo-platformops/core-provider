//go:build envtest

package compositiondefinitions

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/krateo-platformops/core-provider/apis"
	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	crdclient "github.com/krateo-platformops/core-provider/internal/tools/crd"
	crdutils "github.com/krateo-platformops/core-provider/internal/tools/crd/generation"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func startAddEnv(t *testing.T) (client.Client, dynamic.Interface) {
	t.Helper()
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
	return cl, dyn
}

func servedOf(t *testing.T, cl client.Client, crdName string) (served []string, gen int64) {
	t.Helper()
	got := &apiextensionsv1.CustomResourceDefinition{}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: crdName}, got); err != nil {
		t.Fatal(err)
	}
	for _, v := range got.Spec.Versions {
		if v.Served {
			served = append(served, v.Name)
		}
	}
	return served, got.Generation
}

func definitionOn(version, kind string) *compositiondefinitionsv1alpha1.CompositionDefinition {
	cd := &compositiondefinitionsv1alpha1.CompositionDefinition{ObjectMeta: metav1.ObjectMeta{Name: "self", Namespace: "default"}}
	cd.Status.ApiVersion = compositionGroup + "/" + version
	cd.Status.Kind = kind
	return cd
}

// #484, the krateo-057 shape, through applyCompositionCRD on a real apiserver. The definition is on
// v1-0-0 (an instance there); v2-0-0 was added and never used. Asking for v3-0-0 first WAITS (v2's
// grace has only just started: nothing is written, the clock is recorded), and once the grace has
// elapsed the add happens in ONE write that drops v2 — the CRD goes {v1, v2} -> {v1, v3}, never three.
func TestEnvtest_AddWaitsForTheGraceThenDropsInTheSameWrite(t *testing.T) {
	cl, dyn := startAddEnv(t)
	ctx := context.Background()
	curGVK, gvrV1, _, crdName := buildTwoVersionCRD(t, ctx, cl, dyn)
	gvkV1 := schema.GroupVersionKind{Group: compositionGroup, Version: "v1-0-0", Kind: curGVK.Kind}
	createInstance(t, ctx, dyn, gvkV1, gvrV1, "on-v1", "v1-0-0")
	e := newExternal(cl, dyn)
	cr := definitionOn("v1-0-0", curGVK.Kind)

	gvkV3 := schema.GroupVersionKind{Group: compositionGroup, Version: "v3-0-0", Kind: curGVK.Kind}
	crdV3, err := crdutils.GenerateCRD(specSchema("baz"), gvkV3)
	if err != nil {
		t.Fatal(err)
	}

	_, gen0 := servedOf(t, cl, crdName)
	_, err = e.applyCompositionCRD(ctx, cr, gvkV3, crdV3.DeepCopy())
	w, ok := asServedVersionAddWaits(err)
	if !ok {
		t.Fatalf("first add returned %v, want the add-waits outcome (v2-0-0's grace has only just started)", err)
	}
	if fmt.Sprint(w.waiting) != "[v2-0-0]" {
		t.Errorf("waiting = %v, want [v2-0-0] (v1-0-0 is the definition's own current version and is not vacated before the add)", w.waiting)
	}
	if served, gen := servedOf(t, cl, crdName); fmt.Sprint(served) != "[v1-0-0 v2-0-0]" || gen != gen0 {
		t.Fatalf("a waiting add wrote the CRD: served=%v generation %d -> %d", served, gen0, gen)
	}
	rec := cr.Status.ServedVersionsPendingPrune
	if len(rec) != 1 || rec[0].Version != "v2-0-0" || time.Since(rec[0].EligibleSince.Time) > time.Minute {
		t.Fatalf("the wait must record v2-0-0's clock, got %v", rec)
	}

	// The grace has elapsed (as after servedVersionPruneGrace).
	cr.Status.ServedVersionsPendingPrune[0].EligibleSince = metav1.NewTime(time.Now().Add(-servedVersionPruneGrace - time.Second))
	if _, err := e.applyCompositionCRD(ctx, cr, gvkV3, crdV3.DeepCopy()); err != nil {
		t.Fatalf("the add after the grace failed: %v", err)
	}
	served, gen := servedOf(t, cl, crdName)
	if fmt.Sprint(served) != "[v1-0-0 v3-0-0]" {
		t.Errorf("served = %v, want [v1-0-0 v3-0-0]: v2-0-0 dropped in the write that added v3-0-0", served)
	}
	if gen != gen0+1 {
		t.Errorf("generation %d -> %d, want exactly one spec write (drop and add together)", gen0, gen)
	}
}

// The exclusion's control. A definition whose own current version has NO instances still serves
// through it until it moves: the add must not wait for that version's grace (it would delay every bump
// of an instance-less definition by the full window) and must not drop it.
func TestEnvtest_AddDoesNotWaitForTheDefinitionsOwnCurrentVersion(t *testing.T) {
	cl, dyn := startAddEnv(t)
	ctx := context.Background()
	curGVK, gvrV1, _, crdName := buildTwoVersionCRD(t, ctx, cl, dyn)
	e := newExternal(cl, dyn)
	cr := definitionOn("v2-0-0", curGVK.Kind) // on v2; v1 has no instances and is NOT its own

	// Premise: both v1-0-0 and v2-0-0 are prune-eligible by the predicate (no instances, no references).
	prunable, _, err := e.prunableServedVersions(ctx, schema.GroupVersionKind{Group: compositionGroup, Version: "v3-0-0", Kind: curGVK.Kind}, gvrV1, "self", "default")
	if err != nil || !contains(prunable, "v1-0-0") || !contains(prunable, "v2-0-0") {
		t.Fatalf("premise: both versions must be eligible, prunable=%v err=%v", prunable, err)
	}
	// Make v1-0-0's grace elapsed so it is due, and leave v2-0-0 (own) with no clock at all.
	cr.Status.ServedVersionsPendingPrune = []compositiondefinitionsv1alpha1.PendingPrune{{Version: "v1-0-0", EligibleSince: metav1.NewTime(time.Now().Add(-servedVersionPruneGrace - time.Second))}}

	gvkV3 := schema.GroupVersionKind{Group: compositionGroup, Version: "v3-0-0", Kind: curGVK.Kind}
	crdV3, err := crdutils.GenerateCRD(specSchema("baz"), gvkV3)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.applyCompositionCRD(ctx, cr, gvkV3, crdV3); err != nil {
		t.Fatalf("add returned %v, want it to proceed: the only pending version is due, and the own current version is not vacated", err)
	}
	if served, _ := servedOf(t, cl, crdName); fmt.Sprint(served) != "[v2-0-0 v3-0-0]" {
		t.Errorf("served = %v, want [v2-0-0 v3-0-0]: v1 (vacated, due) dropped, v2 (own current) kept, v3 added", served)
	}
}

// A remote definition: the hub copy gets the SAME drop set in its own write. Two real apiservers, one
// the target and one the hub, both serving {v1, v2}. After the grace, the add leaves BOTH at {v1, v3};
// a hub apply without the drop set would append v3 to a hub copy still carrying v2 (the three-version
// object #484 exists to avoid), which this distinguishes.
func TestEnvtest_RemoteAddDropsTheSameVersionsOnTheHub(t *testing.T) {
	tcl, tdyn := startAddEnv(t)
	hcl, hdyn := startAddEnv(t)
	ctx := context.Background()
	curGVK, gvrV1, _, crdName := buildTwoVersionCRD(t, ctx, tcl, tdyn)
	// The hub copy of the same CRD, {v1, v2}.
	for _, v := range []string{"v1-0-0", "v2-0-0"} {
		c, err := crdutils.GenerateCRD(specSchema("foo"), schema.GroupVersionKind{Group: compositionGroup, Version: v, Kind: curGVK.Kind})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := crdclient.ApplyOrUpdateCRD(ctx, hcl, hdyn, c); err != nil {
			t.Fatalf("hub setup %s: %v", v, err)
		}
	}
	gvkV1 := schema.GroupVersionKind{Group: compositionGroup, Version: "v1-0-0", Kind: curGVK.Kind}
	createInstance(t, ctx, tdyn, gvkV1, gvrV1, "on-v1", "v1-0-0")

	e := newExternal(tcl, tdyn)
	e.remote, e.mgmt, e.mgmtDynamic = true, hcl, hdyn
	cr := definitionOn("v1-0-0", curGVK.Kind)
	cr.Status.ServedVersionsPendingPrune = []compositiondefinitionsv1alpha1.PendingPrune{{Version: "v2-0-0", EligibleSince: metav1.NewTime(time.Now().Add(-servedVersionPruneGrace - time.Second))}}

	gvkV3 := schema.GroupVersionKind{Group: compositionGroup, Version: "v3-0-0", Kind: curGVK.Kind}
	crdV3, err := crdutils.GenerateCRD(specSchema("baz"), gvkV3)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.applyCompositionCRD(ctx, cr, gvkV3, crdV3); err != nil {
		t.Fatalf("remote add after the grace failed: %v", err)
	}
	if served, _ := servedOf(t, tcl, crdName); fmt.Sprint(served) != "[v1-0-0 v3-0-0]" {
		t.Errorf("target served = %v, want [v1-0-0 v3-0-0]", served)
	}
	if served, _ := servedOf(t, hcl, crdName); fmt.Sprint(served) != "[v1-0-0 v3-0-0]" {
		t.Errorf("hub served = %v, want [v1-0-0 v3-0-0]: the hub write must drop the same versions as the target's", served)
	}
}

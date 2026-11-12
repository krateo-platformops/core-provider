//go:build e2e

// Package compositiondefinitions served-version pruning e2e. Runs only with -tags e2e against a
// real cluster (KUBECONFIG env) — designed for a throwaway kind cluster. Unlike the unit tests,
// this drives the REAL external.prunableServedVersions / external.pruneStaleServedVersions
// predicate against a live apiserver, to answer the consolidation-plan question: is the fork's
// version GC actually SAFE, or can it prune a version out from under a live composition instance?
//
// It builds a generated composition CRD with vacuum(storage) + v1-0-0 + v2-0-0(current) and probes:
//
//	S1 safe prune      — a stale served version with no instances/refs is prunable
//	S2 label guard     — an instance carrying composition-version=v1-0-0 protects v1-0-0
//	S3 the danger      — an instance that EXISTS at v1-0-0 but LACKS the label: is it protected?
//	S4 cross-reference — another CompositionDefinition on v1-0-0 protects it
//	S5 no-op safety    — vacuum and the current version are never prunable
//
// Requires the CompositionDefinition CRD installed (scripts install crds/ before `go test`).
package compositiondefinitions

import (
	"context"
	"testing"
	"time"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// TestE2E_Prune_S1_SafePrune: a stale served version with no instances and no other definition is
// prunable, while vacuum and the current version are never prunable (S5 folded in).
func TestE2E_Prune_S1_SafePrune(t *testing.T) {
	ctx := context.Background()
	cl, dyn := pruneE2EClients(t)
	curGVK, gvrV1, _, _ := buildTwoVersionCRD(t, ctx, cl, dyn)
	e := newExternal(cl, dyn)

	prunable, kept, err := e.prunableServedVersions(ctx, curGVK, gvrV1, "self", "default")
	if err != nil {
		t.Fatalf("prunableServedVersions: %v", err)
	}
	t.Logf("S1 prunable=%v kept=%v", prunable, kept)
	if !contains(prunable, "v1-0-0") {
		t.Errorf("S1: v1-0-0 has no instances/refs; expected prunable, got prunable=%v", prunable)
	}
	if contains(prunable, "v2-0-0") || contains(prunable, "vacuum") {
		t.Errorf("S1/S5: current version and vacuum must never be prunable, got %v", prunable)
	}
}

// TestE2E_Prune_S2_LabelGuard: an instance carrying composition-version=v1-0-0 protects v1-0-0.
func TestE2E_Prune_S2_LabelGuard(t *testing.T) {
	ctx := context.Background()
	cl, dyn := pruneE2EClients(t)
	curGVK, gvrV1, _, _ := buildTwoVersionCRD(t, ctx, cl, dyn)
	e := newExternal(cl, dyn)

	createInstance(t, ctx, dyn, schema.GroupVersionKind{Group: compositionGroup, Version: "v1-0-0", Kind: curGVK.Kind}, gvrV1, "labeled", "v1-0-0")

	prunable, kept, err := e.prunableServedVersions(ctx, curGVK, gvrV1, "self", "default")
	if err != nil {
		t.Fatalf("prunableServedVersions: %v", err)
	}
	t.Logf("S2 prunable=%v kept=%v", prunable, kept)
	if contains(prunable, "v1-0-0") {
		t.Errorf("S2: a labeled instance must protect v1-0-0 from prune, got prunable=%v", prunable)
	}
}

// TestE2E_Prune_S3_UnlabeledInstance is the key safety probe. An instance EXISTS at v1-0-0 but does
// NOT carry the composition-version label (a pre-F9 object, or one created where the policy is not
// installed). The label-list guard cannot see it. Does prune remove the version out from under it —
// and if so, is the object DATA-LOST or merely orphaned?
func TestE2E_Prune_S3_UnlabeledInstance(t *testing.T) {
	ctx := context.Background()
	cl, dyn := pruneE2EClients(t)
	curGVK, gvrV1, gvrV2, crdName := buildTwoVersionCRD(t, ctx, cl, dyn)
	e := newExternal(cl, dyn)

	createInstance(t, ctx, dyn, schema.GroupVersionKind{Group: compositionGroup, Version: "v1-0-0", Kind: curGVK.Kind}, gvrV1, "unlabeled", "")

	prunable, kept, err := e.prunableServedVersions(ctx, curGVK, gvrV1, "self", "default")
	if err != nil {
		t.Fatalf("prunableServedVersions: %v", err)
	}
	t.Logf("S3 prunable=%v kept=%v", prunable, kept)
	unlabeledIsPrunable := contains(prunable, "v1-0-0")
	t.Logf("S3 FINDING: unlabeled live instance leaves v1-0-0 prunable = %v", unlabeledIsPrunable)

	// Drive the real prune, then determine the fate of the object. Since #454 a version is pruned
	// only after it has stayed eligible for the grace window, so the definition carries a record
	// whose window has already elapsed: the state a real definition reaches servedVersionPruneGrace
	// after Observe first saw the version eligible.
	self := &compositiondefinitionsv1alpha1.CompositionDefinition{ObjectMeta: metav1.ObjectMeta{Name: "self", Namespace: "default"}}
	self.Status.ServedVersionsPendingPrune = []compositiondefinitionsv1alpha1.PendingPrune{{
		Version: "v1-0-0", EligibleSince: metav1.NewTime(time.Now().Add(-servedVersionPruneGrace - time.Second)),
	}}
	if err := e.pruneStaleServedVersions(ctx, self, curGVK, gvrV1); err != nil {
		t.Fatalf("pruneStaleServedVersions: %v", err)
	}

	// After prune: is v1-0-0 still a served version?
	got := &apiextensionsv1.CustomResourceDefinition{}
	if err := cl.Get(ctx, types.NamespacedName{Name: crdName}, got); err != nil {
		t.Fatalf("get CRD post-prune: %v", err)
	}
	var v1Served bool
	for _, v := range got.Spec.Versions {
		if v.Name == "v1-0-0" {
			v1Served = true
		}
	}
	// Is the object still retrievable through the current (v2-0-0) endpoint? None conversion serves
	// storage (vacuum) as-is, so surviving data should be readable there even after v1-0-0 is gone.
	_, getV2Err := dyn.Resource(gvrV2).Namespace("default").Get(ctx, "unlabeled", metav1.GetOptions{})
	dataSurvives := getV2Err == nil

	t.Logf("S3 OUTCOME: v1-0-0 still served=%v ; object readable via v2-0-0=%v (err=%v)", v1Served, dataSurvives, getV2Err)
	if unlabeledIsPrunable && !v1Served && dataSurvives {
		t.Logf("S3 VERDICT: unlabeled instance is ORPHANED (its version pruned) but NOT data-lost — recoverable by re-stamping the label")
	}
	if unlabeledIsPrunable && !dataSurvives {
		t.Errorf("S3 VERDICT: DATA LOSS — pruning v1-0-0 destroyed an unlabeled live object")
	}
}

// TestE2E_Prune_S4_CrossReference: a second CompositionDefinition whose status targets v1-0-0
// protects the version even with zero instances (the shared per-version controller is still needed).
func TestE2E_Prune_S4_CrossReference(t *testing.T) {
	ctx := context.Background()
	cl, dyn := pruneE2EClients(t)
	curGVK, gvrV1, _, _ := buildTwoVersionCRD(t, ctx, cl, dyn)
	e := newExternal(cl, dyn)

	// Plant an "other" CompositionDefinition pinned (via status) to v1-0-0 of this Kind.
	other := &compositiondefinitionsv1alpha1.CompositionDefinition{}
	other.SetName("other-def")
	other.SetNamespace("default")
	if err := cl.Create(ctx, other); err != nil {
		t.Fatalf("create other CD: %v", err)
	}
	t.Cleanup(func() { _ = cl.Delete(context.Background(), other) })
	other.Status.ApiVersion = compositionGroup + "/v1-0-0"
	other.Status.Kind = curGVK.Kind
	if err := cl.Status().Update(ctx, other); err != nil {
		t.Fatalf("set other CD status: %v", err)
	}

	prunable, kept, err := e.prunableServedVersions(ctx, curGVK, gvrV1, "self", "default")
	if err != nil {
		t.Fatalf("prunableServedVersions: %v", err)
	}
	t.Logf("S4 prunable=%v kept=%v", prunable, kept)
	if contains(prunable, "v1-0-0") {
		t.Errorf("S4: another definition references v1-0-0; must be kept, got prunable=%v", prunable)
	}
}

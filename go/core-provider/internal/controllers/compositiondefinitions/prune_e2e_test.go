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
	"fmt"
	"os"
	"testing"
	"time"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	crdclient "github.com/krateo-platformops/core-provider/internal/tools/crd"
	crdutils "github.com/krateo-platformops/core-provider/internal/tools/crd/generation"
	"github.com/krateo-platformops/core-provider/internal/tools/deploy"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const compositionGroup = "composition.krateo.io"

func pruneE2EClients(t *testing.T) (client.Client, dynamic.Interface) {
	t.Helper()
	if os.Getenv("KUBECONFIG") == "" {
		t.Skip("KUBECONFIG must be set")
	}
	rc, err := clientcmd.BuildConfigFromFlags("", os.Getenv("KUBECONFIG"))
	if err != nil {
		t.Fatalf("rest config: %v", err)
	}
	sc := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(sc)
	_ = apiextensionsv1.AddToScheme(sc)
	_ = compositiondefinitionsv1alpha1.SchemeBuilder.AddToScheme(sc)
	cl, err := client.New(rc, client.Options{Scheme: sc})
	if err != nil {
		t.Fatalf("kube client: %v", err)
	}
	dyn, err := dynamic.NewForConfig(rc)
	if err != nil {
		t.Fatalf("dynamic client: %v", err)
	}
	return cl, dyn
}

func specSchema(field string) []byte {
	return []byte(fmt.Sprintf(`{"type":"object","properties":{%q:{"type":"string"}}}`, field))
}

func waitEstablished(t *testing.T, cl client.Client, name string) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := cl.Get(context.Background(), types.NamespacedName{Name: name}, crd); err == nil {
			for _, c := range crd.Status.Conditions {
				if c.Type == apiextensionsv1.Established && c.Status == apiextensionsv1.ConditionTrue {
					return
				}
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("CRD %s not established", name)
}

// buildTwoVersionCRD applies v1-0-0 then v2-0-0 through the REAL ApplyOrUpdateCRD, yielding a CRD
// with versions [vacuum(storage), v1-0-0, v2-0-0]. Returns the current gvk (v2-0-0), the gvr for
// v1-0-0, the plural, and the CRD name.
func buildTwoVersionCRD(t *testing.T, ctx context.Context, cl client.Client, dyn dynamic.Interface) (curGVK schema.GroupVersionKind, gvrV1, gvrV2 schema.GroupVersionResource, crdName string) {
	t.Helper()
	kind := fmt.Sprintf("Prune%d", time.Now().UnixNano()%1000000)
	gvkV1 := schema.GroupVersionKind{Group: compositionGroup, Version: "v1-0-0", Kind: kind}
	gvkV2 := schema.GroupVersionKind{Group: compositionGroup, Version: "v2-0-0", Kind: kind}

	crd1, err := crdutils.GenerateCRD(specSchema("foo"), gvkV1)
	if err != nil {
		t.Fatalf("GenerateCRD v1: %v", err)
	}
	crd2, err := crdutils.GenerateCRD(specSchema("bar"), gvkV2)
	if err != nil {
		t.Fatalf("GenerateCRD v2: %v", err)
	}
	crdName = crd1.Name
	t.Cleanup(func() {
		c := &apiextensionsv1.CustomResourceDefinition{}
		c.Name = crdName
		_ = cl.Delete(context.Background(), c)
	})

	gvrV1, err = crdclient.ApplyOrUpdateCRD(ctx, cl, dyn, crd1)
	if err != nil {
		t.Fatalf("ApplyOrUpdateCRD v1: %v", err)
	}
	gvrV2, err = crdclient.ApplyOrUpdateCRD(ctx, cl, dyn, crd2)
	if err != nil {
		t.Fatalf("ApplyOrUpdateCRD v2: %v", err)
	}
	waitEstablished(t, cl, crdName)
	return gvkV2, gvrV1, gvrV2, crdName
}

func newExternal(cl client.Client, dyn dynamic.Interface) *external {
	return &external{
		mgmt:    cl,
		kube:    cl,
		dynamic: dyn,
		log:     logging.NewNopLogger(),
	}
}

// createInstance creates a composition instance through the given served endpoint, optionally
// stamping the composition-version label (the real MutatingAdmissionPolicy would do this on CREATE;
// here we set it explicitly so the label-present vs label-absent cases are deterministic).
func createInstance(t *testing.T, ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionKind, gvrRes schema.GroupVersionResource, name, versionLabel string) {
	t.Helper()
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvr)
	obj.SetName(name)
	obj.SetNamespace("default")
	if versionLabel != "" {
		obj.SetLabels(map[string]string{deploy.CompositionVersionLabel: versionLabel})
	}
	if _, err := dyn.Resource(gvrRes).Namespace("default").Create(ctx, obj, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create instance %s: %v", name, err)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

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

	// Drive the real prune, then determine the fate of the object.
	if err := e.pruneStaleServedVersions(ctx, curGVK, gvrV1, "self", "default"); err != nil {
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

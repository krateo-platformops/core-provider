package compositiondefinitions

import (
	"context"
	"testing"

	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var crdGVR = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}

// fakeMgmtDynamic seeds a fake dynamic client with obj (converted to unstructured) at the well-known CRD
// GVR — the target of syncHubCompositionCRDVersions' final kube.Apply, which reads/writes the hub CRD's
// spec.versions through mgmtDynamic (the storedVersions trim above it still goes through the typed mgmt
// client's Status().Update, untouched by this migration).
func fakeMgmtDynamic(t *testing.T, obj *apiextensionsv1.CustomResourceDefinition) *dynamicfake.FakeDynamicClient {
	t.Helper()
	sch := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(sch, map[schema.GroupVersionResource]string{crdGVR: "CustomResourceDefinitionList"})
	if obj == nil {
		return dyn
	}
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		t.Fatalf("converting seed CRD to unstructured: %v", err)
	}
	u := &unstructured.Unstructured{Object: raw}
	u.SetAPIVersion("apiextensions.k8s.io/v1")
	u.SetKind("CustomResourceDefinition")
	if _, err := dyn.Resource(crdGVR).Create(context.Background(), u, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seeding dynamic fake with CRD: %v", err)
	}
	return dyn
}

// getHubCRDViaDynamic reads name back through the dynamic fake and converts it to a typed CRD, mirroring
// what the migrated kube.Apply call actually wrote (unlike mgmt.Get, which never sees this write).
func getHubCRDViaDynamic(t *testing.T, dyn *dynamicfake.FakeDynamicClient, name string) *apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	u, err := dyn.Resource(crdGVR).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("re-reading hub CRD via dynamic client: %v", err)
	}
	var out apiextensionsv1.CustomResourceDefinition
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &out); err != nil {
		t.Fatalf("converting dynamic CRD to typed: %v", err)
	}
	return &out
}

func testCompositionCRD() *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "portals.composition.krateo.io"},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "composition.krateo.io",
			Names: apiextensionsv1.CustomResourceDefinitionNames{Plural: "portals", Kind: "Portal"},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
				{Name: "v1-0-0", Served: true, Storage: true},
			},
		},
	}
}

// A local CompositionDefinition must leave the hub untouched: its composition CRD already lives on
// the (local) provisioning cluster, so applyHubCompositionCRD is a strict no-op. This locks the
// "local behavior is unchanged" guarantee of docs/design/remote-composition-mirror.md — mgmtDynamic
// is deliberately nil to prove the local path returns before ever dereferencing it.
//
// The remote apply path is not unit-tested here because ApplyOrUpdateCRD blocks on a CRD-established
// watcher against a real API server; it is build-verified and exercised by the reflector integration
// tests (increment 2).
func TestApplyHubCompositionCRD_LocalIsNoOp(t *testing.T) {
	sch := runtime.NewScheme()
	if err := apiextensionsv1.AddToScheme(sch); err != nil {
		t.Fatalf("add apiextensions to scheme: %v", err)
	}
	mgmt := fakeclient.NewClientBuilder().WithScheme(sch).Build()

	e := &external{remote: false, mgmt: mgmt, mgmtDynamic: nil}

	crd := testCompositionCRD()
	if err := e.applyHubCompositionCRD(context.Background(), crd, nil); err != nil {
		t.Fatalf("expected nil error for local CD, got: %v", err)
	}

	got := &apiextensionsv1.CustomResourceDefinition{}
	err := mgmt.Get(context.Background(), client.ObjectKey{Name: crd.Name}, got)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected no CRD created on hub for local CD, got err=%v", err)
	}
}

// hubSchemeWithStatus builds a scheme + fake hub client that supports the CRD status subresource,
// so syncHubCompositionCRDVersions' storedVersions trim (a Status().Update) is exercised too.
func hubSchemeWithStatus(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	sch := runtime.NewScheme()
	if err := apiextensionsv1.AddToScheme(sch); err != nil {
		t.Fatalf("add apiextensions to scheme: %v", err)
	}
	return fakeclient.NewClientBuilder().
		WithScheme(sch).
		WithStatusSubresource(&apiextensionsv1.CustomResourceDefinition{}).
		WithObjects(objs...).
		Build()
}

func versionNameSet(crd *apiextensionsv1.CustomResourceDefinition) map[string]bool {
	set := map[string]bool{}
	for i := range crd.Spec.Versions {
		set[crd.Spec.Versions[i].Name] = true
	}
	return set
}

// syncHubCompositionCRDVersions must make the hub CRD's served-version set match the spoke's: drop
// the versions the spoke has pruned, add the ones it now serves, trim a dropped version out of the
// hub's storedVersions, and keep the composition-version printer column — mirroring the spoke prune
// so the hub does not accumulate stale served versions (hub↔spoke version skew).
func TestSyncHubCompositionCRDVersions_DropsStaleAndAddsMissing(t *testing.T) {
	hub := testCompositionCRD()
	// Hub carries a stale served version (v1-0-0, no longer on the spoke) plus a common one and the
	// vacuum storage version. v1-0-0 was once the storage version, so it lingers in storedVersions.
	hub.Spec.Versions = []apiextensionsv1.CustomResourceDefinitionVersion{
		{Name: "v1-0-0", Served: true, Storage: false},
		{Name: "v1-1-0", Served: true, Storage: false},
		{Name: "vacuum", Served: false, Storage: true},
	}
	hub.Status.StoredVersions = []string{"v1-0-0", "vacuum"}

	// Spoke's finalized set: dropped v1-0-0, added the current v1-2-0, kept v1-1-0 and vacuum. Its
	// served versions already carry the printer column (as ApplyOrUpdateCRD would leave them).
	spoke := testCompositionCRD()
	spoke.Spec.Versions = []apiextensionsv1.CustomResourceDefinitionVersion{
		{Name: "v1-1-0", Served: true, Storage: false, AdditionalPrinterColumns: []apiextensionsv1.CustomResourceColumnDefinition{{Name: "VERSION", Type: "string", JSONPath: `.metadata.labels.krateo\.io/composition-version`}}},
		{Name: "v1-2-0", Served: true, Storage: false, AdditionalPrinterColumns: []apiextensionsv1.CustomResourceColumnDefinition{{Name: "VERSION", Type: "string", JSONPath: `.metadata.labels.krateo\.io/composition-version`}}},
		{Name: "vacuum", Served: false, Storage: true},
	}

	mgmt := hubSchemeWithStatus(t, hub)
	mgmtDynamic := fakeMgmtDynamic(t, hub)
	e := &external{remote: true, mgmt: mgmt, mgmtDynamic: mgmtDynamic, log: logging.NewNopLogger()}

	if err := e.syncHubCompositionCRDVersions(context.Background(), spoke); err != nil {
		t.Fatalf("syncHubCompositionCRDVersions: %v", err)
	}

	// The final spec.versions rewrite goes through mgmtDynamic (kube.Apply), not mgmt — only the
	// storedVersions trim above it still goes through mgmt's Status().Update.
	got := getHubCRDViaDynamic(t, mgmtDynamic, hub.Name)

	names := versionNameSet(got)
	if names["v1-0-0"] {
		t.Errorf("stale hub version v1-0-0 was not pruned; versions=%v", names)
	}
	for _, want := range []string{"v1-1-0", "v1-2-0", "vacuum"} {
		if !names[want] {
			t.Errorf("expected hub version %q after sync; versions=%v", want, names)
		}
	}
	if len(got.Spec.Versions) != 3 {
		t.Errorf("expected exactly 3 hub versions after sync, got %d: %v", len(got.Spec.Versions), names)
	}

	// storedVersions must no longer reference the pruned v1-0-0 (else the apiserver would reject the
	// spec update on a real cluster).
	for _, sv := range got.Status.StoredVersions {
		if sv == "v1-0-0" {
			t.Errorf("pruned version v1-0-0 still present in hub storedVersions: %v", got.Status.StoredVersions)
		}
	}

	// The composition-version printer column must survive on every served (non-vacuum) version.
	for i := range got.Spec.Versions {
		v := &got.Spec.Versions[i]
		if v.Name == "vacuum" {
			continue
		}
		hasCol := false
		for _, c := range v.AdditionalPrinterColumns {
			if c.Name == "VERSION" {
				hasCol = true
				break
			}
		}
		if !hasCol {
			t.Errorf("version %q lost the VERSION printer column after sync", v.Name)
		}
	}
}

// A converged hub (already matching the spoke's version set) must not be rewritten — the sync is a
// no-op so it does not churn the hub CRD on every reconcile.
func TestSyncHubCompositionCRDVersions_AlreadyConvergedNoOp(t *testing.T) {
	hub := testCompositionCRD()
	hub.Spec.Versions = []apiextensionsv1.CustomResourceDefinitionVersion{
		{Name: "v1-1-0", Served: true, Storage: false},
		{Name: "vacuum", Served: false, Storage: true},
	}
	spoke := testCompositionCRD()
	spoke.Spec.Versions = []apiextensionsv1.CustomResourceDefinitionVersion{
		{Name: "v1-1-0", Served: true, Storage: false},
		{Name: "vacuum", Served: false, Storage: true},
	}

	mgmt := hubSchemeWithStatus(t, hub)
	e := &external{remote: true, mgmt: mgmt, log: logging.NewNopLogger()}

	if err := e.syncHubCompositionCRDVersions(context.Background(), spoke); err != nil {
		t.Fatalf("syncHubCompositionCRDVersions: %v", err)
	}

	got := &apiextensionsv1.CustomResourceDefinition{}
	if err := mgmt.Get(context.Background(), client.ObjectKey{Name: hub.Name}, got); err != nil {
		t.Fatalf("re-reading hub CRD: %v", err)
	}
	// A fake-client write bumps resourceVersion; an unchanged one proves the no-op path returned early.
	if got.ResourceVersion != hub.ResourceVersion {
		t.Errorf("expected no write on converged hub (rv %q), got rv %q", hub.ResourceVersion, got.ResourceVersion)
	}
}

// For a local CompositionDefinition the hub sync must be a strict no-op even if handed a spoke set:
// the composition CRD lives only on the (local) provisioning cluster. mgmt is left nil to prove the
// local path returns before touching any client.
func TestSyncHubCompositionCRDVersions_LocalIsNoOp(t *testing.T) {
	e := &external{remote: false, mgmt: nil, log: logging.NewNopLogger()}
	spoke := testCompositionCRD()
	if err := e.syncHubCompositionCRDVersions(context.Background(), spoke); err != nil {
		t.Fatalf("expected nil error for local CD, got: %v", err)
	}
}

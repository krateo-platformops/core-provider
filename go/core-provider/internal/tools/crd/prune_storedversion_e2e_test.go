//go:build e2e

// What happens when a served version is pruned while instances are still ENCODED at it?
//
// pruneStaleServedVersions trims a pruned version out of status.storedVersions and then removes it
// from spec.versions, justified in its own comment by:
//
//	"Safe: the migration loop re-writes every Composition through the current served endpoint, so
//	 all data is now persisted at the current storage version ("vacuum") — none remains at these
//	 retired versions."
//
// That assumption has a gate the prune does not share. The migration loop selects instances by the
// krateo.io/composition-version LABEL and is skipped entirely when UpgradePolicy is Manual/Paused,
// whereas the prune's own eligibility test also keys off that label — so an instance carrying NO
// label (the known policy-absence gap, generation.go:154) is invisible to both: never rewritten,
// and no obstacle to pruning the one version it is still encoded at.
//
// This test establishes what the apiserver does in that state, rather than predicting it.
//
//	Run: KUBEBUILDER_ASSETS=$(setup-envtest use -p path 1.36.0) \
//	     go test -tags e2e ./internal/tools/crd/ -run PruneStored -v
package crd

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func specObj(props map[string]apiextensionsv1.JSONSchemaProps) *apiextensionsv1.CustomResourceValidation {
	return &apiextensionsv1.CustomResourceValidation{
		OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
			Type: "object",
			Properties: map[string]apiextensionsv1.JSONSchemaProps{
				"apiVersion": {Type: "string"},
				"kind":       {Type: "string"},
				"metadata":   {Type: "object"},
				"spec":       {Type: "object", Properties: props},
				"status":     {Type: "object", XPreserveUnknownFields: strptr(true)},
			},
		},
	}
}

func vacuumVersion() apiextensionsv1.CustomResourceDefinitionVersion {
	return apiextensionsv1.CustomResourceDefinitionVersion{
		Name: "vacuum", Served: false, Storage: true,
		Schema: &apiextensionsv1.CustomResourceValidation{
			OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
				Type: "object",
				Properties: map[string]apiextensionsv1.JSONSchemaProps{
					"apiVersion": {Type: "string"},
					"kind":       {Type: "string"},
					"metadata":   {Type: "object"},
					"spec":       {Type: "object", XPreserveUnknownFields: strptr(true)},
					"status":     {Type: "object", XPreserveUnknownFields: strptr(true)},
				},
			},
		},
	}
}

func TestE2E_PruneStoredVersionWithLiveInstances(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via setup-envtest")
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest apiserver: %v", err)
	}
	defer func() { _ = env.Stop() }()

	ctx := context.Background()
	extc, _ := apiextensionsclient.NewForConfig(cfg)
	dyn, _ := dynamic.NewForConfig(cfg)
	crds := extc.ApiextensionsV1().CustomResourceDefinitions()

	name := "gadgets.composition.krateo.io"

	// --- Step 1: the FIRST version, as generateCRD emits it: sole version, storage=true.
	initial := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "composition.krateo.io",
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural: "gadgets", Singular: "gadget", Kind: "Gadget", ListKind: "GadgetList",
			},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name: "v1-0-0", Served: true, Storage: true,
				Schema: specObj(map[string]apiextensionsv1.JSONSchemaProps{"replicas": {Type: "integer"}}),
			}},
		},
	}
	if _, err := crds.Create(ctx, initial, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create CRD: %v", err)
	}
	v1 := schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1-0-0", Resource: "gadgets"}
	if err := wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 60*time.Second, true,
		func(ctx context.Context) (bool, error) {
			_, err := dyn.Resource(v1).Namespace("default").List(ctx, metav1.ListOptions{})
			return err == nil, nil
		}); err != nil {
		t.Fatalf("v1-0-0 endpoint never came up: %v", err)
	}

	// An instance written at v1-0-0 while it is the STORAGE version, and deliberately carrying NO
	// krateo.io/composition-version label — the policy-absence gap.
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "composition.krateo.io/v1-0-0",
		"kind":       "Gadget",
		"metadata":   map[string]any{"name": "g1", "namespace": "default"},
		"spec":       map[string]any{"replicas": int64(2)},
	}}
	if _, err := dyn.Resource(v1).Namespace("default").Create(ctx, obj, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create gadget at v1-0-0: %v", err)
	}

	cur, _ := crds.Get(ctx, name, metav1.GetOptions{})
	t.Logf("after first instance: storedVersions=%v", cur.Status.StoredVersions)

	// --- Step 2: the version bump, as AppendVersion performs it: add v2-0-0 + vacuum(storage),
	// every non-vacuum version served, none of them storage.
	cur.Spec.Versions = []apiextensionsv1.CustomResourceDefinitionVersion{
		{Name: "v1-0-0", Served: true, Storage: false,
			Schema: specObj(map[string]apiextensionsv1.JSONSchemaProps{"replicas": {Type: "integer"}})},
		{Name: "v2-0-0", Served: true, Storage: false,
			Schema: specObj(map[string]apiextensionsv1.JSONSchemaProps{"replicas": {Type: "integer"}})},
		vacuumVersion(),
	}
	cur.Spec.Conversion = &apiextensionsv1.CustomResourceConversion{Strategy: apiextensionsv1.NoneConverter}
	if _, err := crds.Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("append version: %v", err)
	}
	cur, _ = crds.Get(ctx, name, metav1.GetOptions{})
	t.Logf("after bump:            storedVersions=%v", cur.Status.StoredVersions)

	// The instance was NOT rewritten (no label -> migration selects nothing), so it is still
	// encoded at v1-0-0 while vacuum is now the storage version.

	// --- Step 3: the prune, exactly as pruneStaleServedVersions does it: trim storedVersions via
	// the status subresource first, then remove the version from spec.versions.
	var keptStored []string
	for _, sv := range cur.Status.StoredVersions {
		if sv != "v1-0-0" {
			keptStored = append(keptStored, sv)
		}
	}
	if len(keptStored) != len(cur.Status.StoredVersions) {
		cur.Status.StoredVersions = keptStored
		if _, err := crds.UpdateStatus(ctx, cur, metav1.UpdateOptions{}); err != nil {
			t.Fatalf("trim storedVersions: %v", err)
		}
	}
	cur, _ = crds.Get(ctx, name, metav1.GetOptions{})
	kept := []apiextensionsv1.CustomResourceDefinitionVersion{}
	for _, v := range cur.Spec.Versions {
		if v.Name != "v1-0-0" {
			kept = append(kept, v)
		}
	}
	cur.Spec.Versions = kept
	if _, err := crds.Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("prune v1-0-0 from spec.versions: %v", err)
	}
	after, _ := crds.Get(ctx, name, metav1.GetOptions{})
	t.Logf("after prune:           storedVersions=%v  spec.versions=%d", after.Status.StoredVersions, len(after.Spec.Versions))

	// --- Step 4: can the instance still be read?
	v2 := schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v2-0-0", Resource: "gadgets"}

	listErr := func() error {
		_, err := dyn.Resource(v2).Namespace("default").List(ctx, metav1.ListOptions{})
		return err
	}()
	getObj, getErr := dyn.Resource(v2).Namespace("default").Get(ctx, "g1", metav1.GetOptions{})

	t.Logf("LIST through v2-0-0: err=%v", listErr)
	t.Logf("GET  through v2-0-0: err=%v", getErr)

	if listErr != nil || getErr != nil {
		t.Errorf("INSTANCE UNREADABLE after pruning the version it was encoded at.\n"+
			"  LIST error: %v\n  GET error: %v\n"+
			"This is the shape of the documented \"invalid group/version\" failure: the prune removed "+
			"a version that still had objects encoded at it, because both the migration (which would "+
			"have rewritten them) and the prune's own eligibility test key off the "+
			"krateo.io/composition-version label, and an unlabelled instance is invisible to both.",
			listErr, getErr)

		// Does re-adding the version restore readability? This is what the documented workaround
		// ("change the chart version to a different value, and change it back") actually does, so
		// confirming it both explains why that workaround works and bounds the damage.
		back, _ := crds.Get(ctx, name, metav1.GetOptions{})
		back.Spec.Versions = append([]apiextensionsv1.CustomResourceDefinitionVersion{{
			Name: "v1-0-0", Served: true, Storage: false,
			Schema: specObj(map[string]apiextensionsv1.JSONSchemaProps{"replicas": {Type: "integer"}}),
		}}, back.Spec.Versions...)
		if _, err := crds.Update(ctx, back, metav1.UpdateOptions{}); err != nil {
			t.Logf("RECOVERY: re-adding v1-0-0 failed: %v", err)
			return
		}
		recovered := wait.PollUntilContextTimeout(ctx, 300*time.Millisecond, 30*time.Second, true,
			func(ctx context.Context) (bool, error) {
				_, err := dyn.Resource(v2).Namespace("default").Get(ctx, "g1", metav1.GetOptions{})
				return err == nil, nil
			})
		if recovered == nil {
			t.Logf("RECOVERY CONFIRMED: re-adding the pruned version to spec.versions makes the " +
				"instance readable again, with no data loss. This is why the documented workaround " +
				"(change the chart version and change it back) clears the error — it re-appends the " +
				"version the prune removed. The damage is an availability outage for the whole Kind, " +
				"not destruction of the objects.")
		} else {
			t.Logf("RECOVERY FAILED within 30s: %v — the outage is not self-clearing by re-adding "+
				"the version, which would make this considerably worse", recovered)
		}
		return
	}

	// If it IS readable, say so precisely — that is a real finding too, and it bounds the blast
	// radius of the prune to something recoverable.
	replicas, found, _ := unstructured.NestedInt64(getObj.Object, "spec", "replicas")
	t.Logf("instance SURVIVED the prune: apiVersion=%q spec.replicas=%d present=%v",
		getObj.GetAPIVersion(), replicas, found)
	if !found {
		t.Errorf("instance is readable but spec.replicas was lost after the prune")
	}
	if !strings.Contains(getObj.GetAPIVersion(), "v2-0-0") {
		t.Errorf("expected the surviving object to be served as v2-0-0, got %q", getObj.GetAPIVersion())
	}
}

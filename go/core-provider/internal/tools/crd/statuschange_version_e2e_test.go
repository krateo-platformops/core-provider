//go:build e2e

// #190 apiserver-level regression. Runs only with -tags e2e against a real cluster (KUBECONFIG).
// Reuses e2eClients/waitCRDEstablished/compositionGroup from e2e_test.go (same package).
//
// ApplyOrUpdateCRD must always create the version it reports creating. The reconcile trusts the
// returned GVR for everything that follows: the CDC Deployment and ServiceAccount are built for
// that version, the instance migration lists and relabels THROUGH that version's endpoint, and
// the served-version prune evaluates against it.
//
// The status change and the version bump are INDEPENDENT inputs and a single reconcile can carry
// both — a statusDataTemplate edit landing in the same generation as a chart version bump, or a
// core-provider upgrade that changes the embedded status.schema.json for every definition at once.
// They used to be exclusive: the status branch returned before AppendVersion could run, so the
// function reported the new GVR with err == nil having never added that version to the CRD.
package crd

import (
	"context"
	"testing"

	"github.com/krateo-platformops/core-provider/internal/tools/crd/generation"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestE2E_StatusChangeAndVersionBumpInOneReconcile(t *testing.T) {
	cl, dyn := e2eClients(t)
	ctx := context.Background()

	const kind = "Doodad"
	crdName := "doodads." + compositionGroup

	gen := func(version string, status []generation.StatusField) *apiextensionsv1.CustomResourceDefinition {
		gvk := schema.GroupVersionKind{Group: compositionGroup, Kind: kind, Version: version}
		crd, err := generation.GenerateCRD(specSchema("replicas"), gvk)
		if err != nil {
			t.Fatalf("GenerateCRD %s: %v", version, err)
		}
		if err := generation.InjectStatusFields(crd, status); err != nil {
			t.Fatalf("InjectStatusFields %s: %v", version, err)
		}
		return crd
	}

	t.Cleanup(func() {
		c := &apiextensionsv1.CustomResourceDefinition{}
		c.Name = crdName
		_ = cl.Delete(ctx, c)
	})

	// --- Reconcile 1: initial version, no declared status fields.
	gvr1, err := ApplyOrUpdateCRD(ctx, cl, dyn, gen("v1-0-0", nil))
	if err != nil {
		t.Fatalf("first ApplyOrUpdateCRD: %v", err)
	}
	waitCRDEstablished(t, cl, crdName)

	// Precondition. Without this the test could "pass" against a function that creates nothing at
	// all, which proves nothing about the ordering this test exists to pin.
	if ok, err := Lookup(ctx, cl, gvr1); err != nil || !ok {
		t.Fatalf("precondition failed: v1-0-0 absent after the first reconcile (ok=%v err=%v) — "+
			"this test proves nothing unless the baseline version is created", ok, err)
	}

	// --- Reconcile 2: the chart version moves to v2-0-0 AND a status field is declared in the
	// same generation. Both mutations must land.
	withStatus := []generation.StatusField{{ForPath: "url", Expression: "${ .spec.replicas }", Type: "string"}}
	gvr2, err := ApplyOrUpdateCRD(ctx, cl, dyn, gen("v2-0-0", withStatus))
	if err != nil {
		t.Fatalf("second ApplyOrUpdateCRD: %v", err)
	}
	if gvr2.Version != "v2-0-0" {
		t.Fatalf("expected the returned GVR to name the new version, got %q", gvr2.Version)
	}
	waitCRDEstablished(t, cl, crdName)

	exists, err := Lookup(ctx, cl, gvr2)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}

	live, err := Get(ctx, cl, gvr2.GroupResource())
	if err != nil {
		t.Fatalf("get live CRD: %v", err)
	}
	var names []string
	for _, v := range live.Spec.Versions {
		names = append(names, v.Name)
	}

	if !exists {
		t.Fatalf("ApplyOrUpdateCRD returned GVR %s with err=nil, but that version is NOT in the "+
			"CRD (live versions: %v).\n"+
			"The caller treats this as success and proceeds to: deploy a CDC Deployment+SA for a "+
			"version the apiserver does not serve, migrate composition instances THROUGH that "+
			"non-existent endpoint, and evaluate the served-version prune against it (#190).",
			gvr2.Version, names)
	}

	// The status change must have landed too — otherwise the fix traded one dropped mutation for
	// the other, which an "is v2-0-0 present" assertion alone would not notice.
	assertStatusField(t, live, "v1-0-0", "url")
	assertStatusField(t, live, "v2-0-0", "url")

	// And the earlier version must survive the combined path: instances are still labelled at it
	// until the migration relabels them, so dropping it here would strand them.
	if ok, _ := Lookup(ctx, cl, gvr1); !ok {
		t.Errorf("v1-0-0 disappeared from the CRD after the combined status+version reconcile "+
			"(live versions: %v)", names)
	}
}

// assertStatusField fails unless the named version's schema declares status.<field>.
func assertStatusField(t *testing.T, crd *apiextensionsv1.CustomResourceDefinition, version, field string) {
	t.Helper()
	for _, v := range crd.Spec.Versions {
		if v.Name != version {
			continue
		}
		if v.Schema == nil || v.Schema.OpenAPIV3Schema == nil {
			t.Errorf("version %s has no schema", version)
			return
		}
		st, ok := v.Schema.OpenAPIV3Schema.Properties["status"]
		if !ok {
			t.Errorf("version %s has no status in its schema", version)
			return
		}
		if _, ok := st.Properties[field]; !ok {
			t.Errorf("version %s: declared status field %q did not land — the status apply was "+
				"skipped by the combined path (#190)", version, field)
		}
		return
	}
	t.Errorf("version %s not present on the live CRD", version)
}

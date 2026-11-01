//go:build e2e

// #293 apiserver-level proof for the BASELINE status fields. Runs only with -tags e2e against a
// real cluster (KUBECONFIG).
//
// The sibling test next door (status_projection_e2e_test.go) already proves that a blueprint's
// DECLARED statusDataTemplate fields survive subresource pruning. Nobody pointed the same harness
// at the fields the CONTROLLER ITSELF writes, and that is the whole of #293: the cdc wrote
// status.managedDigest from 2.13.49 and status.previousManagedCount from 2.13.52, neither was
// declared in statics/status.schema.json, and a structural CRD schema prunes what it does not
// declare. Both were silently stripped on every write — 502 drift reports across 30 of 30
// inventoried compositions in 20 minutes on a fresh cluster, because the absent-stamp branch
// correctly read the absence it could never fix.
//
// The same thing had happened before, to observedGeneration, and the warning was written in that
// field's own description in the schema file. It did not help: the mistake is made while editing
// the WRITER, and the schema is a file you never open to add a status write.
//
// So the guard has to be a test rather than a note, and it has to go through a real apiserver —
// a unit test over a status field proves the function's logic and CANNOT prove the field survives
// a write.
package crd

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	crdutils "github.com/krateo-platformops/core-provider/internal/tools/crd/generation"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// TestE2E_BaselineStatusFieldsSurviveAWrite is #293's regression test.
func TestE2E_BaselineStatusFieldsSurviveAWrite(t *testing.T) {
	if os.Getenv("KUBECONFIG") == "" {
		t.Skip("KUBECONFIG must be set")
	}
	ctx := context.Background()
	cl, dyn := e2eClients(t)

	kind := fmt.Sprintf("Bl%d", time.Now().UnixNano()%100000)
	gvk := schema.GroupVersionKind{Group: compositionGroup, Version: "v1-0-0", Kind: kind}

	crd, err := crdutils.GenerateCRD(specSchema("foo"), gvk)
	if err != nil {
		t.Fatalf("GenerateCRD: %v", err)
	}
	// NO InjectStatusFields: the baseline schema is what GenerateCRD already embeds from
	// statics/status.schema.json. Injecting declared fields here would test the other mechanism
	// and hide this one.

	crdName := crd.Name
	t.Cleanup(func() {
		c := &apiextensionsv1.CustomResourceDefinition{}
		c.Name = crdName
		_ = cl.Delete(ctx, c)
	})

	gvr, err := ApplyOrUpdateCRD(ctx, cl, dyn, crd)
	if err != nil {
		t.Fatalf("ApplyOrUpdateCRD: %v", err)
	}
	waitCRDEstablished(t, cl, crdName)

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	obj.SetName("baseline")
	obj.SetNamespace("default")
	createEventually(t, ctx, dyn.Resource(gvr).Namespace("default"), obj)

	got, err := dyn.Resource(gvr).Namespace("default").Get(ctx, "baseline", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	writes := baselineStatusWrites()
	for path, val := range writes {
		if err := unstructured.SetNestedField(got.Object, val, "status", path); err != nil {
			t.Fatalf("set status.%s locally: %v", path, err)
		}
	}

	// THE PRUNING CONTROL. An undeclared field MUST be stripped. Without this arm, a schema that
	// had somehow become permissive (x-kubernetes-preserve-unknown-fields creeping in) would make
	// every assertion below pass while proving nothing about declaration — the test would confirm
	// that writes work, not that the schema declares these fields.
	const undeclared = "thisFieldIsDeliberatelyNotDeclared"
	if err := unstructured.SetNestedField(got.Object, "should-be-pruned", "status", undeclared); err != nil {
		t.Fatalf("set the control field locally: %v", err)
	}

	updated, err := dyn.Resource(gvr).Namespace("default").
		UpdateStatus(ctx, got, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("UpdateStatus rejected the baseline status: %v", err)
	}

	// Re-READ rather than trusting the update's own response, so this cannot pass on a value that
	// only ever existed in the request.
	final, err := dyn.Resource(gvr).Namespace("default").Get(ctx, "baseline", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("re-get: %v", err)
	}
	_ = updated

	for path, want := range writes {
		gotVal, found, err := unstructured.NestedFieldNoCopy(final.Object, "status", path)
		if err != nil {
			t.Errorf("status.%s: read error %v", path, err)
			continue
		}
		if !found {
			t.Errorf("status.%s was PRUNED by the apiserver — declare it in "+
				"internal/tools/crd/generation/statics/status.schema.json, or the cdc's write of it "+
				"is silently discarded on every reconcile (#293)", path)
			continue
		}
		if fmt.Sprint(gotVal) != fmt.Sprint(want) {
			t.Errorf("status.%s = %v, want %v", path, gotVal, want)
		}
	}

	if _, found, _ := unstructured.NestedFieldNoCopy(final.Object, "status", undeclared); found {
		t.Errorf("status.%s SURVIVED — the schema is not pruning undeclared fields, so every "+
			"assertion above proves only that writes work, not that these fields are declared",
			undeclared)
	}
}

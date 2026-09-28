//go:build e2e

// #142 apiserver-level proof. Runs only with -tags e2e against a real cluster (KUBECONFIG).
// Reuses e2eClients/waitCRDEstablished/compositionGroup from e2e_test.go (same package).
//
// The e2e suite already ran against a real apiserver when v1.15.0 shipped, and it passed — because
// every CRD it built happened to have no required properties under an optional parent. A suite that
// only ever sees shapes its author imagined is the same class of blind spot as a suite nobody runs.
// This adds the shape that actually broke production.
package crd

import (
	"context"
	"fmt"
	"testing"
	"time"

	crdutils "github.com/krateo-platformops/core-provider/internal/tools/crd/generation"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// requiredUnderOptionalParentSchema mirrors builder-publish: an optional parent whose child object
// declares `required`, with the required property itself carrying a default.
//
// That last detail is the trap. It reads as "the default will fill it in", which is what the first
// version of the guard assumed — but the apiserver validates a `default` literally against its own
// schema and does not apply nested defaults to it first, so `{}` fails `required` regardless.
const requiredUnderOptionalParentSchema = `{
  "type": "object",
  "properties": {
    "repository": {
      "type": "object",
      "properties": {
        "configurationRef": {
          "type": "object",
          "required": ["name"],
          "properties": {
            "name":      {"type": "string", "default": "default-config"},
            "namespace": {"type": "string"}
          }
        }
      }
    },
    "replicas": {"type": "integer", "default": 1}
  }
}`

// The regression for #142.
//
// crdgen stamped `default: {}` on configurationRef, and the apiserver rejected the ENTIRE generated
// CRD with "default.name: Required value". The CompositionDefinition could not sync at all, so one
// bad parent took down the whole definition rather than degrading one field.
//
// This asserts against the real apiserver rather than a unit fixture because the failure was the
// apiserver's own validation rejecting our output. crdgen validates its CRD before returning it, so
// GenerateCRD failing here is itself the regression — but applying it proves the acceptance rather
// than trusting our copy of the rules.
func TestE2E_RequiredPropertyUnderOptionalParent(t *testing.T) {
	ctx := context.Background()
	cl, dyn := e2eClients(t)

	kind := fmt.Sprintf("Req%d", time.Now().Unix()%100000)
	gvk := schema.GroupVersionKind{Group: compositionGroup, Version: "v1-0-0", Kind: kind}

	crd, err := crdutils.GenerateCRD([]byte(requiredUnderOptionalParentSchema), gvk)
	if err != nil {
		t.Fatalf("GenerateCRD rejected a schema with a required property under an optional parent — "+
			"this is #142: crdgen stamped default:{} on an object that requires a field, and its own "+
			"validation caught what the apiserver would have rejected: %v", err)
	}

	gvr, err := ApplyOrUpdateCRD(ctx, cl, dyn, crd)
	if err != nil {
		t.Fatalf("the apiserver rejected the generated CRD: %v", err)
	}
	t.Cleanup(func() {
		c := &apiextensionsv1.CustomResourceDefinition{}
		c.Name = crd.Name
		_ = cl.Delete(ctx, c)
	})
	waitCRDEstablished(t, cl, crd.Name)

	// A composition omitting the optional parent must still be accepted, and must NOT have acquired
	// a half-built parent. The top-level default still applies — the fix must not cost that.
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	obj.SetName("req1")
	obj.SetNamespace("default")
	// An explicitly empty spec, not an absent one. Defaults apply only to a field whose PARENT
	// exists, so omitting spec entirely would suppress spec.replicas too and the assertion below
	// would be measuring the wrong thing — an absent default that proves nothing about this fix.
	_ = unstructured.SetNestedMap(obj.Object, map[string]any{}, "spec")
	created := createEventually(t, ctx, dyn.Resource(gvr).Namespace("default"), obj)

	if _, found, _ := unstructured.NestedMap(created.Object, "spec", "repository"); found {
		t.Errorf("spec.repository was materialized, but its only descendant default sits behind a " +
			"configurationRef that cannot be defaulted — materializing it creates a present-but-empty " +
			"object for a benefit that can never arrive")
	}
	replicas, found, _ := unstructured.NestedInt64(created.Object, "spec", "replicas")
	if !found || replicas != 1 {
		t.Errorf("top-level default must still apply: spec.replicas = %v (found=%v), want 1", replicas, found)
	}

	t.Logf("OK #142: a required property under an optional parent no longer produces an invalid CRD; " +
		"spec.repository left absent, spec.replicas defaulted to 1")
}

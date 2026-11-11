//go:build e2e

package crd

import (
	"context"
	"os"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

// #393, Diego 2026-11-11: scale-to-zero is local-only in this release, so the apiserver must REJECT
// spec.controller.scaleToZero: true together with a remote spec.deploy.targetRef, and accept every
// other combination. The rule is CEL on the generated CompositionDefinition CRD; this applies that
// CRD, exactly as generated, to a real apiserver and asks it, with server-side dry-run creates.
//
// The rule is written without quote characters on purpose. gofmt's doc-comment formatter rewrote
// the first version's empty-string literal (two single quotes) into one typographic quote, which
// the apiserver would have rejected as a
// malformed rule or, worse, compiled into a different one. Only a real apiserver tells those apart.
func TestScaleToZeroIsRejectedOnRemoteDefinitions(t *testing.T) {
	ctx := context.Background()
	cl, dyn := e2eClients(t)

	raw, err := os.ReadFile("../../../crds/core.krateo.io_compositiondefinitions.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatal(err)
	}
	// PREMISE: the rule under test is in the generated CRD, so a pass below cannot be a CRD that
	// simply has no rule (every case would then be accepted, and the reject case would fail, but the
	// failure message would point at the apiserver rather than at a missing marker).
	found := 0
	for _, r := range crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].XValidations {
		if strings.Contains(r.Rule, "scaleToZero") {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("premise: the generated CRD carries %d scaleToZero rules on spec, want 1", found)
	}

	existing := &apiextensionsv1.CustomResourceDefinition{}
	err = cl.Get(ctx, client.ObjectKey{Name: crd.Name}, existing)
	switch {
	case apierrors.IsNotFound(err):
		if err := cl.Create(ctx, &crd); err != nil {
			t.Fatalf("creating the CompositionDefinition CRD: %v", err)
		}
	case err != nil:
		t.Fatal(err)
	default:
		crd.ResourceVersion = existing.ResourceVersion
		if err := cl.Update(ctx, &crd); err != nil {
			t.Fatalf("updating the CompositionDefinition CRD: %v", err)
		}
	}
	waitCRDEstablished(t, cl, crd.Name)

	ri := dyn.Resource(schema.GroupVersionResource{Group: "core.krateo.io", Version: "v1alpha1",
		Resource: "compositiondefinitions"}).Namespace("default")

	cd := func(name string, scaleToZero *bool, remote bool) *unstructured.Unstructured {
		spec := map[string]any{"chart": map[string]any{"url": "oci://ghcr.io/example/charts/demo", "version": "1.0.0"}}
		if scaleToZero != nil {
			spec["controller"] = map[string]any{"scaleToZero": *scaleToZero}
		}
		if remote {
			spec["deploy"] = map[string]any{"targetRef": map[string]any{"name": "spoke-1"}}
		}
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "core.krateo.io/v1alpha1", "kind": "CompositionDefinition",
			"metadata": map[string]any{"name": name, "namespace": "default"}, "spec": spec}}
	}
	yes, no := true, false

	cases := []struct {
		name   string
		obj    *unstructured.Unstructured
		reject bool
	}{
		{"remote + scaleToZero: true is rejected", cd("remote-stz", &yes, true), true},
		{"local + scaleToZero: true is accepted", cd("local-stz", &yes, false), false},
		{"remote without scaleToZero is accepted", cd("remote-plain", nil, true), false},
		{"remote + scaleToZero: false is accepted", cd("remote-stz-off", &no, true), false},
		{"local without scaleToZero is accepted", cd("local-plain", nil, false), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ri.Create(ctx, c.obj, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
			if !c.reject {
				if err != nil {
					t.Fatalf("want accepted, got: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("want rejected at admission, got accepted")
			}
			// The error family, then the message that names the follow-up issue.
			if !apierrors.IsInvalid(err) {
				t.Fatalf("want an Invalid (422) error from validation, got: %v", err)
			}
			if !strings.Contains(err.Error(), "local definitions only") || !strings.Contains(err.Error(), "core-provider#442") {
				t.Errorf("rejection message does not name the scope and the follow-up issue: %v", err)
			}
		})
	}
}

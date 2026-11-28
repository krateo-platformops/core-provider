package generation

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	apiext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	extvalidation "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/validation"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// A blueprint's values.schema.json may carry Krateo-only keys. Consumers that depend on the generated
// CRD NOT carrying them (and on reading them from the raw schema instead):
//   - x-krateo-required-on-create: read by the portal's create form from the raw values.schema.json
//     (portal branch feat/required-on-create);
//   - x-krateo-options: likewise a portal form hint;
//   - x-krateo-values-schema: core-provider's own published-schema marker
//     (go/core-provider/internal/tools/chart/marker.go).
//
// The apiserver refuses a CRD whose schema is not a valid JSONSchemaProps, and apiextensionsv1 models
// only x-kubernetes-* extensions. The drop happens in Unmarshal, which decodes crdgen's output with a
// non-strict serializer into the typed CRD: every unmodelled x- key vanishes there. Nothing else pins
// that. Making the CRD keep these keys is a decision (the portal would then read them from the CRD,
// and the apiserver would have to accept them); this test forces whoever changes the decoder to make it.
//
// The x-kubernetes-* controls must survive: they prove the walk sees x- keys, so "absent" means something.
func TestGeneratedCRDDropsUnmodelledXKeysAndKeepsKubernetesOnes(t *testing.T) {
	const spec = `{"type":"object","properties":{
	  "name":{"type":"string","x-krateo-required-on-create":true,"x-acme-hint":1},
	  "size":{"type":"string","enum":["s","m"],"x-krateo-options":["s","m"]},
	  "nested":{"type":"object","x-krateo-values-schema":"infer","x-acme-hint":1,
	    "properties":{
	      "inner":{"type":"string","x-krateo-required-on-create":true,"x-krateo-options":["a"]},
	      "tags":{"type":"array","items":{"type":"string"},"x-kubernetes-list-type":"set","x-acme-hint":1}
	    }},
	  "free":{"type":"object","x-kubernetes-preserve-unknown-fields":true,"x-krateo-values-schema":"infer"}
	}}`
	crd, err := GenerateCRD([]byte(spec), schema.GroupVersionKind{Group: "composition.krateo.io", Version: "v1-0-0", Kind: "XKeysProbe"})
	if err != nil {
		t.Fatal(err)
	}
	root := crd.Spec.Versions[0].Schema.OpenAPIV3Schema

	// Typed walk of the properties we put the keys on.
	spc := root.Properties["spec"]
	tags := spc.Properties["nested"].Properties["tags"]
	if tags.XListType == nil || *tags.XListType != "set" {
		t.Errorf("x-kubernetes-list-type missing on nested.tags: %v", tags.XListType)
	}
	free := spc.Properties["free"]
	if free.XPreserveUnknownFields == nil || !*free.XPreserveUnknownFields {
		t.Errorf("x-kubernetes-preserve-unknown-fields missing on free: %v", free.XPreserveUnknownFields)
	}

	// Raw walk: marshal and look for every x- key at any depth.
	raw, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	keys := map[string]int{}
	var walk func(any)
	walk = func(n any) {
		switch v := n.(type) {
		case map[string]any:
			for k, c := range v {
				if strings.HasPrefix(k, "x-") {
					keys[k]++
				}
				walk(c)
			}
		case []any:
			for _, c := range v {
				walk(c)
			}
		}
	}
	walk(doc)
	for k, n := range keys {
		if !strings.HasPrefix(k, "x-kubernetes-") {
			t.Errorf("unmodelled key %q survives %d time(s) in the generated openAPIV3Schema", k, n)
		}
	}
	for _, k := range []string{"x-kubernetes-list-type", "x-kubernetes-preserve-unknown-fields"} {
		if keys[k] != 1 {
			t.Errorf("control: %q seen %d time(s), want 1 (the walker must see x- keys)", k, keys[k])
		}
	}

	// The CRD must validate as the apiserver would validate it.
	var internal apiext.CustomResourceDefinition
	if err := apiextv1.Convert_v1_CustomResourceDefinition_To_apiextensions_CustomResourceDefinition(crd, &internal, nil); err != nil {
		t.Fatal(err)
	}
	// status is written by the apiserver on create, not by the generator; give it the one value it will have.
	internal.Status.StoredVersions = []string{crd.Spec.Versions[0].Name}
	if errs := extvalidation.ValidateCustomResourceDefinition(context.Background(), &internal); len(errs) != 0 {
		t.Errorf("generated CRD does not validate: %v", errs)
	}
}

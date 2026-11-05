package generation

import (
	"testing"

	"github.com/krateo-platformops/plumbing/crdgen"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// #362. The cache must return a CRD's warnings with it. A durable condition is computed from them on
// every Observe, and almost every Observe is a cache hit: if a hit returned no warnings, the
// condition would read "fully enforced" from the second reconcile on, which is the false all-clear
// this signal exists to prevent.
func TestGenerateCRDWithWarnings_ACacheHitReportsTheSameWarnings(t *testing.T) {
	// A GVK unique to this test, so no other test's render is already cached under the key.
	gvk := schema.GroupVersionKind{Group: "composition.krateo.io", Version: "v9-9-9", Kind: "WarnCacheProbe"}
	spec := []byte(`{"type":"object","properties":{"o":{"type":"object","not":{"required":["a"]}}}}`)

	first, err := GenerateCRDWithWarnings(spec, gvk)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Fresh {
		t.Fatal("setup: the first render was a cache hit, so this test cannot tell a hit from a miss")
	}
	lost := 0
	for _, w := range first.Warnings {
		if w.Kind.LosesEnforcement() {
			lost++
		}
	}
	if lost == 0 {
		t.Fatalf("setup: the schema produced no enforcement-loss warning, so a hit returning none would pass: %+v", first.Warnings)
	}

	second, err := GenerateCRDWithWarnings(spec, gvk)
	if err != nil {
		t.Fatal(err)
	}
	if second.Fresh {
		t.Error("the second render with an identical schema ran crdgen again; the cache did not hit")
	}
	if len(second.Warnings) != len(first.Warnings) {
		t.Fatalf("a cache hit returned %d warnings, the render that produced it %d", len(second.Warnings), len(first.Warnings))
	}
	for i := range first.Warnings {
		if second.Warnings[i] != first.Warnings[i] {
			t.Errorf("warning %d differs on the hit: %+v vs %+v", i, second.Warnings[i], first.Warnings[i])
		}
	}

	// The cached slice is a copy: a caller mutating what it got must not change the next caller's.
	second.Warnings[0] = crdgen.Warning{Message: "mutated"}
	third, _ := GenerateCRDWithWarnings(spec, gvk)
	if third.Warnings[0].Message == "mutated" {
		t.Error("the cache handed out its own slice; a caller's mutation leaked into the next render")
	}
}

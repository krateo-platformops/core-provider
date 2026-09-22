package rbac

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func objWith(ns, name string, ann map[string]string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]interface{}{}}
	u.SetName(name)
	if ns != "" {
		u.SetNamespace(ns)
	}
	if ann != nil {
		u.SetAnnotations(ann)
	}
	return u
}

// A Helm-owned object must be refused rather than written to.
//
// This is the second half of core-provider#130. The names no longer collide, so this should be
// unreachable — but the original defect was invisible exactly here: unioning rules into a
// chart-owned Role SUCCEEDS, Helm reverts it on that release's next apply, and neither side logs
// anything. The composition then fails on a permission it was granted seconds earlier, which reads
// as "the generated RBAC is incomplete" instead of "it was overwritten".
func TestHelmOwnedObjectIsRefused(t *testing.T) {
	u := objWith("krateo-system", "nightly-review", map[string]string{
		"meta.helm.sh/release-name":      "nightly-review",
		"meta.helm.sh/release-namespace": "krateo-system",
	})

	err := errIfHelmOwned(u, "Role")
	if err == nil {
		t.Fatal("expected a Helm-owned Role to be refused, got nil")
	}

	// The message has to name the release — that is the whole diagnostic value. Without it the
	// operator sees a permission failure and no reason to suspect a second writer.
	if !strings.Contains(err.Error(), `"nightly-review"`) {
		t.Errorf("error must name the owning release, got: %v", err)
	}
	if !strings.Contains(err.Error(), "krateo-system/nightly-review") {
		t.Errorf("error must identify the object as namespace/name, got: %v", err)
	}
	if !strings.Contains(err.Error(), "Role") {
		t.Errorf("error must name the kind, got: %v", err)
	}
}

// Our own objects carry no Helm annotation and must pass untouched.
func TestUnownedObjectIsAllowed(t *testing.T) {
	cases := []struct {
		name string
		u    *unstructured.Unstructured
	}{
		{"no annotations at all", objWith("krateo-system", "nightly-review-krateo-rbac", nil)},
		{"unrelated annotations", objWith("krateo-system", "nightly-review-krateo-rbac", map[string]string{
			"krateo.io/composition-name": "nightly-review",
		})},
		// An empty value is not ownership. Helm never writes this, but an empty string must not be
		// mistaken for a release name and block a legitimate write.
		{"empty release name", objWith("krateo-system", "x", map[string]string{"meta.helm.sh/release-name": ""})},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := errIfHelmOwned(tc.u, "Role"); err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
		})
	}
}

// Cluster-scoped objects have no namespace to prefix; the message must still be readable.
func TestClusterScopedHelmOwnedMessage(t *testing.T) {
	u := objWith("", "nightly-review", map[string]string{"meta.helm.sh/release-name": "nightly-review"})

	err := errIfHelmOwned(u, "ClusterRole")
	if err == nil {
		t.Fatal("expected a Helm-owned ClusterRole to be refused, got nil")
	}
	if strings.Contains(err.Error(), "/nightly-review") {
		t.Errorf("cluster-scoped object must not be reported with an empty namespace prefix, got: %v", err)
	}
}

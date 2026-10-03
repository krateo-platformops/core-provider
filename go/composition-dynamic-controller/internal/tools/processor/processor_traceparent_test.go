package processor

import (
	"fmt"
	"testing"

	"github.com/krateo-platformops/plumbing/helm"
)

func TestComputeReleaseDigest_ExcludesTraceparent(t *testing.T) {
	tmpl := "apiVersion: v1\nkind: ConfigMap\n" +
		"metadata:\n  name: x\n  annotations:\n" +
		"    krateo.io/traceparent: %s\n    krateo.io/tracestate: vendor=%s\n" +
		"  labels:\n    krateo.io/composition-id: abc\ndata:\n  k: v\n"

	d1, err := ComputeReleaseDigest(&helm.Release{Manifest: fmt.Sprintf(tmpl, "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01", "a")})
	if err != nil {
		t.Fatalf("digest 1: %v", err)
	}
	d2, err := ComputeReleaseDigest(&helm.Release{Manifest: fmt.Sprintf(tmpl, "00-11111111111111111111111111111111-2222222222222222-01", "b")})
	if err != nil {
		t.Fatalf("digest 2: %v", err)
	}
	if d1 != d2 {
		t.Fatalf("digest must be invariant to the traceparent/tracestate value (else every reconcile churns the helm release): %s vs %s", d1, d2)
	}
}

// A multi-doc release whose ONLY per-reconcile change is the krateo.io/traceparent (+ tracestate)
// on a NESTED *.krateo.io CR child (the second doc, after a plain resource) must produce the SAME
// digest across reconciles — from BOTH digest paths: ComputeReleaseDigest (Observe's drift check)
// and DecodeMinRelease (what Create/Update store in status.digest). Before the fix, DecodeMinRelease
// hashed the raw manifest bytes (traceparent included), so its stored digest churned every reconcile
// while Observe's stripped digest did not — an out-of-date loop that re-upgraded the release each
// cycle (the umbrella + snowplow churn exposed by #184). This case reproduces that: it fails pre-fix
// on the DecodeMinRelease assertions and on the cross-path equality.
func TestReleaseDigest_ExcludesTraceparent_NestedCR(t *testing.T) {
	// Doc 1: a plain k8s resource (no trace annotations). Doc 2: a nested *.krateo.io CR child
	// carrying the per-reconcile traceparent/tracestate (indented under metadata.annotations).
	tmpl := "apiVersion: v1\nkind: ServiceAccount\nmetadata:\n  name: sa-plain\n  namespace: demo\n" +
		"---\n" +
		"apiVersion: serviceaccount.authn.krateo.io/v1alpha1\nkind: ServiceAccount\n" +
		"metadata:\n  name: sa-nested\n  namespace: demo\n  annotations:\n" +
		"    krateo.io/traceparent: %s\n    krateo.io/tracestate: vendor=%s\n" +
		"  labels:\n    krateo.io/composition-id: abc\nspec:\n  displayName: nested\n"

	manA := fmt.Sprintf(tmpl, "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01", "a")
	manB := fmt.Sprintf(tmpl, "00-11111111111111111111111111111111-2222222222222222-01", "b")

	// Observe path (ComputeReleaseDigest) — must be traceparent-invariant across the nested CR.
	cA, err := ComputeReleaseDigest(&helm.Release{Manifest: manA})
	if err != nil {
		t.Fatalf("ComputeReleaseDigest A: %v", err)
	}
	cB, err := ComputeReleaseDigest(&helm.Release{Manifest: manB})
	if err != nil {
		t.Fatalf("ComputeReleaseDigest B: %v", err)
	}
	if cA != cB {
		t.Fatalf("ComputeReleaseDigest must ignore nested-CR traceparent/tracestate: %s vs %s", cA, cB)
	}

	// Create/Update path (DecodeMinRelease) — the digest stored in status.digest. Pre-fix this
	// hashed the raw manifest and CHANGED with the traceparent, causing the Observe<->Update churn.
	_, dA, err := DecodeMinRelease(&helm.Release{Manifest: manA})
	if err != nil {
		t.Fatalf("DecodeMinRelease A: %v", err)
	}
	_, dB, err := DecodeMinRelease(&helm.Release{Manifest: manB})
	if err != nil {
		t.Fatalf("DecodeMinRelease B: %v", err)
	}
	if dA != dB {
		t.Fatalf("DecodeMinRelease (stored status.digest) must ignore nested-CR traceparent/tracestate "+
			"(else every reconcile churns the helm release): %s vs %s", dA, dB)
	}

	// The two paths must agree: Observe compares its ComputeReleaseDigest against the DecodeMinRelease
	// value stored by Create/Update. If they differ for identical content, Observe reports drift forever.
	if cA != dA {
		t.Fatalf("ComputeReleaseDigest and DecodeMinRelease must agree for identical content "+
			"(Observe drift-checks the stored digest): %s vs %s", cA, dA)
	}
}

func TestComputeReleaseDigest_NoTraceparentStable(t *testing.T) {
	plain := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\ndata:\n  k: v\n"
	d1, err := ComputeReleaseDigest(&helm.Release{Manifest: plain})
	if err != nil || d1 == "" {
		t.Fatalf("digest: %v / %q", err, d1)
	}
	d2, _ := ComputeReleaseDigest(&helm.Release{Manifest: plain})
	if d1 != d2 {
		t.Fatalf("plain (no-traceparent) digest must be stable: %s vs %s", d1, d2)
	}
}

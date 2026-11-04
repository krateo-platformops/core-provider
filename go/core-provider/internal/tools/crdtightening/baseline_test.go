package crdtightening

// The parts of baseline resolution that can be tested without a registry: reading the artifact's
// identity out of helm's own output, and refusing to proceed without it.
//
// WHY THESE CASES AND NOT A LIVE PULL. A live pull belongs behind a build tag, for the reason the
// comparison is in its own file: a check that only runs when a registry answers is a check nobody
// controls. What is testable offline is the part most likely to be WRONG — the parsing — and the
// part whose failure is worst: a missing digest silently accepted, which turns a measurement
// against known bytes into a measurement against a mutable tag.

import (
	"strings"
	"testing"
)

func TestParsePullOutput(t *testing.T) {
	const realOutput = "Pulled: ghcr.io/krateo-platformops/charts/core-provider-crds:2.13.61\n" +
		"Digest: sha256:daf8a75303e24fafae8e2c37983c16755db09a73699e525e27fb89bae7eb3588\n"

	ref, digest, err := parsePullOutput(realOutput)
	if err != nil {
		t.Fatalf("real helm output must parse: %v", err)
	}
	if want := "ghcr.io/krateo-platformops/charts/core-provider-crds:2.13.61"; ref != want {
		t.Errorf("ref = %q, want %q", ref, want)
	}
	if !strings.HasPrefix(digest, "sha256:") || len(digest) != 71 {
		t.Errorf("digest = %q, want a sha256 of 64 hex chars", digest)
	}

	// Order must not matter, and neither must surrounding noise: helm prints these on stderr
	// alongside whatever else the command emits, and CombinedOutput interleaves them.
	t.Run("order and surrounding noise", func(t *testing.T) {
		noisy := "WARNING: something unrelated\n" +
			"Digest: sha256:daf8a75303e24fafae8e2c37983c16755db09a73699e525e27fb89bae7eb3588\n" +
			"Pulled: ghcr.io/x/charts/c:1.0.0\n" +
			"trailing chatter\n"
		ref, digest, err := parsePullOutput(noisy)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ref != "ghcr.io/x/charts/c:1.0.0" || digest == "" {
			t.Errorf("got ref=%q digest=%q", ref, digest)
		}
	})

	// THE FAILURES THAT MATTER. Each must be an error, because the alternative is a comparison
	// whose baseline cannot be named — and a finding against an uncitable baseline is an assertion
	// rather than a measurement.
	for _, c := range []struct {
		name string
		out  string
	}{
		{"nothing at all", ""},
		{"a digest but no Pulled line",
			"Digest: sha256:daf8a75303e24fafae8e2c37983c16755db09a73699e525e27fb89bae7eb3588\n"},
		{"a Pulled line but NO DIGEST — the one that must not slide through",
			"Pulled: ghcr.io/x/charts/c:1.0.0\n"},
		{"a digest that is not a sha256",
			"Pulled: ghcr.io/x/charts/c:1.0.0\nDigest: md5:abc\n"},
		{"a truncated sha256",
			"Pulled: ghcr.io/x/charts/c:1.0.0\nDigest: sha256:daf8a753\n"},
		{"the words appearing mid-line rather than as helm's own output",
			"some tool said Pulled: x and Digest: sha256:" + strings.Repeat("a", 64) + " inline\n"},
	} {
		if _, _, err := parsePullOutput(c.out); err == nil {
			t.Errorf("%s: expected an error, got none", c.name)
		}
	}
}

func TestResolveBaselineRefusesWithoutAChart(t *testing.T) {
	// Constraint 3 at its simplest: no chart named is an error, not an empty baseline that a
	// caller would compare against and report clean.
	if _, err := ResolveBaseline("", "", "", t.TempDir()); err == nil {
		t.Error("resolving with no chart name must be an error, not an empty baseline")
	}
}

func TestArtifactStringNamesTheBytes(t *testing.T) {
	// The one question anybody asks about a surprising finding is "compared against what?", so the
	// answer has to include the digest rather than only the version.
	a := &Artifact{
		Ref:    "ghcr.io/krateo-platformops/charts/core-provider-crds:2.13.61",
		Digest: "sha256:" + strings.Repeat("d", 64),
		Source: "latest published (core-provider has no installer pin entry)",
	}
	s := a.String()
	for _, want := range []string{"core-provider-crds:2.13.61", "sha256:", "latest published"} {
		if !strings.Contains(s, want) {
			t.Errorf("Artifact.String() = %q, missing %q", s, want)
		}
	}
}

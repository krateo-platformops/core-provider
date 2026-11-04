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

// The RELEASED umbrella's declared dependency is the PREFERRED baseline, so its parsing is the part
// whose silent failure costs most: it does not error the run, it falls back to latest-published — a
// baseline with no relationship to deployment at all, under which a tightening introduced and
// published in one release compares against itself.
func TestParseUmbrella(t *testing.T) {
	// The real shape, as `helm show chart oci://.../installer` prints it: annotations first, then
	// appVersion, then dependencies, with the umbrella's OWN version further down.
	const real = `annotations:
  krateo.io/category: application
  org.opencontainers.image.version: 0.3.506
apiVersion: v2
appVersion: 0.3.506
dependencies:
- condition: bootstrap.coreProvider.enabled
  name: core-provider
  repository: oci://ghcr.io/krateo-platformops/charts
  version: 2.13.57
- condition: bootstrap.coreProvider.enabled
  name: core-provider-crds
  repository: oci://ghcr.io/krateo-platformops/charts
  version: 2.13.57
description: Umbrella orchestrator blueprint.
name: installer
version: 0.3.506
`
	v, umbrella, err := parseUmbrella([]byte(real), "core-provider-crds")
	if err != nil {
		t.Fatalf("real `helm show chart` output must parse: %v", err)
	}
	if v != "2.13.57" {
		t.Errorf("dependency version = %q, want 2.13.57", v)
	}
	// THE TRAP THIS PINS. `version:` appears inside every dependencies[] entry as well as at the
	// top level, and both are plausible-looking semvers. A line-scan for the first `version:`
	// returns 2.13.57 as the umbrella's version — a wrong reading that never announces itself,
	// because it attributes the baseline to an installer release that does not exist.
	if umbrella != "0.3.506" {
		t.Errorf("umbrella version = %q, want 0.3.506 — a dependency's version was read as the "+
			"umbrella's", umbrella)
	}
	// The two subcharts are pinned independently even where they currently agree, so the lookup
	// must be by NAME and not by position.
	if v, _, err := parseUmbrella([]byte(real), "core-provider"); err != nil || v != "2.13.57" {
		t.Errorf("core-provider: got %q, %v", v, err)
	}

	for _, c := range []struct{ name, yaml, chart string }{
		{"a chart the umbrella does not declare", real, "chart-inspector"},
		{"no dependencies at all", "name: installer\nversion: 1.0.0\n", "core-provider-crds"},
		{"a dependency with no version",
			"dependencies:\n- name: core-provider-crds\n", "core-provider-crds"},
		// Each of these would otherwise be handed to `helm pull` and fail at the registry, where
		// the error names the registry instead of the placeholder.
		{"the CHART_VERSION placeholder",
			"dependencies:\n- name: core-provider-crds\n  version: CHART_VERSION\n", "core-provider-crds"},
		{"a helm template expression",
			"dependencies:\n- name: core-provider-crds\n  version: \"{{ .Chart.AppVersion }}\"\n", "core-provider-crds"},
		{"a shell-style variable",
			"dependencies:\n- name: core-provider-crds\n  version: $VERSION\n", "core-provider-crds"},
		{"not YAML at all", "\tthis: : is: not\n  - yaml", "core-provider-crds"},
	} {
		if v, _, err := parseUmbrella([]byte(c.yaml), c.chart); err == nil {
			t.Errorf("%s: expected an error, got version %q", c.name, v)
		}
	}

	// A missing umbrella version is NOT fatal: the baseline is still correctly resolved, it is only
	// less precisely attributed in the run log.
	if v, umbrella, err := parseUmbrella(
		[]byte("dependencies:\n- name: c\n  version: 1.2.3\n"), "c"); err != nil || v != "1.2.3" {
		t.Errorf("a chart with no top-level version must still resolve: %q, %q, %v", v, umbrella, err)
	} else if umbrella == "" {
		t.Error("the run log must get something rather than an empty attribution")
	}

	// And the shapes that MUST be accepted, so the guard cannot reject a real release.
	for _, good := range []string{"2.13.57", "v2.13.57", "0.0.1", "2.13.57-rc.1", "2.13.57+build.4"} {
		y := "dependencies:\n- name: c\n  version: " + good + "\n"
		if v, _, err := parseUmbrella([]byte(y), "c"); err != nil || v != good {
			t.Errorf("version %q must be accepted, got %q, %v", good, v, err)
		}
	}
}

package crdtightening

// Baseline resolution: fetch the chart the comparison measures against, and SAY WHICH ARTIFACT IT
// GOT.
//
// THREE CONSTRAINTS, all from the core-provider session and all load-bearing:
//
//  1. The baseline is the PULLABLE ARTIFACT, never a local checkout and never "a tag exists".
//     A tag is the input; the artifact is the effect. They come apart: frontend 1.8.17, 1.8.18 and
//     1.8.19 are all tags whose chart was never published, so a baseline resolved from tag history
//     would have compared against a version nobody could install — and silently, because the tag
//     resolves fine.
//
//  2. The resolution SURFACES the artifact it fetched, digest included. A gate that cites
//     "the baseline" without naming it cannot be audited after the fact, and the one question
//     anybody asks about a surprising finding is "compared against what?".
//
//  3. "Could not determine the baseline" is an ERROR, never "nothing to compare, so pass". An
//     unresolvable baseline that reads as green is the single failure mode that makes the whole
//     gate decorative. Every path out of here either returns an Artifact or an error.
//
// WHERE THE VERSION COMES FROM, AND WHY NOT THE INSTALLER PIN. The other repos running this
// comparison baseline on installer's `component-pins.yaml`, because the pin is the version live
// clusters serve and therefore the one with objects stored against it. That is not available here:
// the installer pins carry no entry for core-provider. Measured rather than assumed — of 68
// entries, the only match on "core" is `core-provider-agent`, and no entry names chart
// `core-provider` or `core-provider-crds`. core-provider is deployed by the installer chart
// directly as a bootstrap-tier component rather than pinned as a composition, so there is no pin
// to read.
//
// So the default is LATEST PUBLISHED, which is the honest fallback for a component whose delivery
// is not pinned: if nothing pins it, latest-published is what an install gets. The cost, stated
// rather than discovered: the baseline MOVES when this repo publishes, so a tightening introduced
// and published in one release compares against itself. Pass an explicit version to escape that
// when it matters — and the Source field says which rule was used on every run, so it is never
// silent about which baseline it measured against.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// DefaultRegistry is where this org's charts publish. Charts land at <registry>/<chart>, which is
// why a bare registry root would collide with same-named container images.
const DefaultRegistry = "oci://ghcr.io/krateo-platformops/charts"

// Artifact is the baseline, named precisely enough to cite in a finding.
type Artifact struct {
	Chart   string // core-provider-crds
	Version string // 2.13.61
	Ref     string // ghcr.io/krateo-platformops/charts/core-provider-crds:2.13.61, as helm reported it
	Digest  string // sha256:…, as helm reported it
	Dir     string // where it was unpacked
	Source  string // how Version was chosen, for the run log
}

// String is what a finding or a run log should print: the artifact, not "the baseline".
func (a *Artifact) String() string {
	return fmt.Sprintf("%s@%s (%s)", a.Ref, a.Digest, a.Source)
}

// ResolveBaseline fetches `chart` at `version`, or at the latest published version when `version`
// is empty. `dir` must exist; the chart is unpacked inside it.
func ResolveBaseline(registry, chart, version, dir string) (*Artifact, error) {
	if registry == "" {
		registry = DefaultRegistry
	}
	if chart == "" {
		return nil, fmt.Errorf("no chart named: there is nothing to resolve a baseline from")
	}
	ref := strings.TrimSuffix(registry, "/") + "/" + chart

	source := "explicit version"
	if version == "" {
		var err error
		version, err = latestPublished(ref)
		if err != nil {
			// Constraint 3. A caller that cannot tell this apart from "clean" will report clean.
			return nil, fmt.Errorf("could not determine the latest published version of %s, so "+
				"there is no baseline to compare against: %w", chart, err)
		}
		source = "latest published (core-provider has no installer pin entry)"
	}

	out, err := exec.Command("helm", "pull", ref, "--version", version,
		"--untar", "-d", dir).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("helm pull %s --version %s: %w\n%s", ref, version, err, out)
	}
	pulled, digest, err := parsePullOutput(string(out))
	if err != nil {
		// Deliberately fatal rather than degraded to "we got the files, close enough". Constraint
		// 2 is the whole reason this function exists in addition to a plain pull: an artifact we
		// cannot name is not a baseline we can cite, and a finding against an uncitable baseline
		// is an assertion rather than a measurement.
		return nil, fmt.Errorf("pulled %s %s but could not identify the artifact: %w\n%s",
			chart, version, err, out)
	}

	unpacked := filepath.Join(dir, chart)
	if st, statErr := os.Stat(unpacked); statErr != nil || !st.IsDir() {
		return nil, fmt.Errorf("helm reported pulling %s but %s is not a directory: the chart "+
			"name and the archive's root differ, so discovery would walk nothing", pulled, unpacked)
	}

	return &Artifact{
		Chart: chart, Version: version, Ref: pulled, Digest: digest,
		Dir: unpacked, Source: source,
	}, nil
}

// latestPublished asks the registry rather than git. `helm show chart` with no --version resolves
// the registry's own notion of latest, which is what an unpinned install would get.
func latestPublished(ref string) (string, error) {
	out, err := exec.Command("helm", "show", "chart", ref).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("helm show chart %s: %w\n%s", ref, err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "version:"); ok {
			if v = strings.TrimSpace(v); v != "" {
				return v, nil
			}
		}
	}
	return "", fmt.Errorf("no `version:` line in the chart metadata:\n%s", out)
}

var (
	pulledRe = regexp.MustCompile(`(?m)^Pulled:\s*(\S+)\s*$`)
	digestRe = regexp.MustCompile(`(?m)^Digest:\s*(sha256:[0-9a-f]{64})\s*$`)
)

// parsePullOutput reads the artifact's identity out of what `helm pull` reports:
//
//	Pulled: ghcr.io/krateo-platformops/charts/core-provider-crds:2.13.61
//	Digest: sha256:daf8a75303e24fafae8e2c37983c16755db09a73699e525e27fb89bae7eb3588
//
// Both are required. A missing digest is not a cosmetic loss: the digest is the only part that
// identifies the BYTES, and a mutable tag is exactly the thing a baseline must not be taken on
// trust.
func parsePullOutput(out string) (ref, digest string, err error) {
	m := pulledRe.FindStringSubmatch(out)
	if m == nil {
		return "", "", fmt.Errorf("no `Pulled:` line in helm's output")
	}
	d := digestRe.FindStringSubmatch(out)
	if d == nil {
		return "", "", fmt.Errorf("no `Digest: sha256:…` line in helm's output for %s", m[1])
	}
	return m[1], d[1], nil
}

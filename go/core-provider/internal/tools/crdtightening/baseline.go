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
// WHERE THE VERSION COMES FROM, AND WHY IT IS NOT THE PIN FILE. The other repos running this
// comparison baseline on installer's `component-pins.yaml`, because a pin is the version live
// clusters serve and therefore the one with objects stored against it. That file has no entry for
// core-provider. Measured, with the denominator: of 68 entries the only match on "core" is
// `core-provider-agent`, and nothing names chart `core-provider` or `core-provider-crds`.
//
// The obvious fix — "ask installer to add a pin entry" — is the WRONG one here, and the installer
// session said so before anyone acted on it. core-provider is not pinned as a composition at all:
// `core-provider` and `core-provider-crds` are the two subchart DEPENDENCIES the umbrella declares
// in its own Chart.yaml. There is no entry to add, so this needs a different resolver rather than a
// change on their side.
//
// SO THE SOURCE IS THE RELEASED UMBRELLA CHART, AND NOT ITS `main`. That distinction is the whole
// design, and it is installer's correction to a first version of this file that read `main`:
//
//	1. the cluster                  exact, and unavailable to a CI gate
//	2. the RELEASED installer chart  lags deployment by at most one roll     <- what this uses
//	3. installer main                AHEAD of deployment by every unreleased merge
//	4. latest published              no relationship to deployment at all
//
// AHEAD IS THE DANGEROUS DIRECTION for this gate. Its job is to fail on a tightening against the
// version in the field. A baseline that advances the instant a pin MERGES absorbs the tightening
// before any cluster has the version — so a narrowing shipped in 2.13.58..2.13.61, which no cluster
// has ever run, becomes invisible. The released chart still catches it. A merged pin describes no
// cluster; installer's main currently carries eleven merged-but-unreleased changes, several of them
// pins, and PR #658 would move those two dependency versions to 2.13.61 while every cluster stays
// on 2.13.57.
//
// Today all the deployed-ish sources agree and only latest-published is wrong, by four versions:
//
//	released installer 0.3.506 Chart.yaml   core-provider-crds  2.13.57
//	installer main                          core-provider-crds  2.13.57
//	latest published core-provider-crds                         2.13.61
//	cdc actually running   057 {2.13.57: 70, 2.13.45: 1}   demo {2.13.57: 42, 2.13.45: 2}
//
// The divergence is one merge away, which is why the source matters before it bites.
//
// Order: an explicit version, else the released umbrella's declared dependency, else
// latest-published. The last is a FALLBACK, not a default, and says so in the run log along with
// the reason the preferred resolver failed — because knowing only that a fallback was used hides
// which of the two things went wrong, and they have different fixes ("the installer moved" versus
// "the registry did not answer"). Under that fallback alone the baseline moves when this repo
// publishes, so a tightening introduced and published in one release would compare against itself.
//
// NOT USED HERE, but recorded so nobody rediscovers it: installer publishes a
// `krateo-installed-versions` ConfigMap written by the release actually installed, which is source
// (1) rather than (2). It covers pin entries only today, so core-provider-crds is absent — and it
// needs a cluster, which a CI gate does not have. If anyone later wants "what is deployed" rather
// than "what the newest release declares", that is where it should come from.

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"sigs.k8s.io/yaml"
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
		var declaredErr error
		var umbrella string
		version, umbrella, declaredErr = releasedUmbrellaDependency(chart)
		if declaredErr == nil {
			source = fmt.Sprintf("released installer chart %s, dependencies[] (lags deployment "+
				"by at most one roll)", umbrella)
		} else {
			var err error
			version, err = latestPublished(ref)
			if err != nil {
				// Constraint 3, and both reasons are reported: a caller that cannot tell this
				// apart from "clean" will report clean, and knowing only that the fallback failed
				// hides that the preferred resolver failed first.
				return nil, fmt.Errorf("could not determine a baseline version for %s. The "+
					"released umbrella's declared dependency could not be read (%v) and neither "+
					"could the latest published version (%w)", chart, declaredErr, err)
			}
			// Named as a FALLBACK in the run log, because this is the one resolution under which
			// the baseline moves when this repo publishes.
			source = fmt.Sprintf("latest published — FALLBACK, the released umbrella's declared "+
				"dependency could not be read (%v)", declaredErr)
		}
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

// UmbrellaChart is installer's own chart, where the two core-provider subcharts are pinned.
// Resolved at its latest PUBLISHED version — see the note above on why not its `main`.
const UmbrellaChart = "installer"

// releasedUmbrellaDependency reads the version the RELEASED umbrella declares for `chart`, and
// returns the umbrella version it read it from so the run log can name the source exactly.
//
// `helm show chart` on an OCI ref prints the chart's whole metadata, dependencies included, so this
// is one registry call and no pull. It also means the answer comes from the published artifact
// rather than from a branch — the distinction the note above is entirely about.
func releasedUmbrellaDependency(chart string) (version, umbrella string, err error) {
	ref := DefaultRegistry + "/" + UmbrellaChart
	out, err := exec.Command("helm", "show", "chart", ref).CombinedOutput()
	if err != nil {
		return "", "", fmt.Errorf("helm show chart %s: %w\n%s", ref, err, out)
	}
	return parseUmbrella(out, chart)
}

// parseUmbrella is the parsing, split out so it is testable without a registry.
//
// Parsed as YAML and never line-scanned. `version:` appears inside every dependencies[] entry as
// well as at the top level, so a grep for it returns a subchart's version as often as the
// umbrella's — and both are plausible-looking semvers, which is the shape of a wrong reading that
// never announces itself.
func parseUmbrella(chartYAML []byte, chart string) (version, umbrella string, err error) {
	var doc struct {
		Version      string `json:"version"`
		Dependencies []struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"dependencies"`
	}
	if err := yaml.Unmarshal(chartYAML, &doc); err != nil {
		return "", "", fmt.Errorf("parsing the umbrella chart metadata: %w", err)
	}
	for _, d := range doc.Dependencies {
		if d.Name != chart {
			continue
		}
		if d.Version == "" {
			return "", "", fmt.Errorf("the umbrella declares %s with no version", chart)
		}
		// A templated placeholder is not a version. The umbrella's own `version`/`appVersion` read
		// CHART_VERSION until release time; installer says the dependency pins are deliberately
		// literal so they stay reviewable in a diff, and this holds them to it. Asserted as a
		// POSITIVE shape rather than by listing placeholder spellings: a denylist of "contains { or
		// $ or an underscore" passes the next placeholder nobody predicted, while "looks like a
		// version" fails every one of them. Without it the string reaches `helm pull` and fails at
		// the registry, where the error sends the reader to the wrong system entirely.
		if !versionRe.MatchString(d.Version) {
			return "", "", fmt.Errorf("the umbrella declares %s at %q, which is not a version — "+
				"most likely a templated placeholder resolved at release time", chart, d.Version)
		}
		// The umbrella's own version is for the run log only, so a missing one is not fatal: the
		// baseline is still correctly resolved, it is just less precisely attributed.
		if doc.Version == "" {
			return d.Version, "<version unknown>", nil
		}
		return d.Version, doc.Version, nil
	}
	return "", "", fmt.Errorf("the released umbrella declares no dependency named %s (it declares "+
		"%d)", chart, len(doc.Dependencies))
}

// latestPublished asks the registry rather than git. `helm show chart` with no --version resolves
// the registry's own notion of latest, which is what an unpinned install would get.
func latestPublished(ref string) (string, error) {
	// stdout alone is the metadata; stderr (warnings, deprecations) is kept for the error message
	// but never parsed as YAML.
	var stderr bytes.Buffer
	cmd := exec.Command("helm", "show", "chart", ref)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("helm show chart %s: %w\n%s", ref, err, stderr.String())
	}
	return chartVersion(out)
}

// chartVersion reads the TOP-LEVEL version out of chart metadata, parsed as YAML.
//
// NOT LINE BY LINE. The first version of this matched `version:` after TrimSpace, which also
// matches an indented `dependencies[].version` — and `helm show chart` prints keys in alphabetical
// order, so `dependencies` comes BEFORE the top-level `version`. On any chart with dependencies
// the first match was a subchart's version. parseUmbrella had already been written to avoid exactly
// this; the fallback had not (reported by the frontend session, who wrote both). Inert for
// core-provider-crds today, which declares no dependencies — which is why it went unnoticed.
func chartVersion(metadata []byte) (string, error) {
	var doc struct {
		Version string `json:"version"`
	}
	if err := yaml.Unmarshal(metadata, &doc); err != nil {
		return "", fmt.Errorf("parsing the chart metadata: %w", err)
	}
	if !versionRe.MatchString(doc.Version) {
		return "", fmt.Errorf("the chart metadata's top-level version %q is not a version:\n%s", doc.Version, metadata)
	}
	return doc.Version, nil
}

var (
	// Deliberately loose on the suffix (prereleases, build metadata) and strict on the start: a
	// version begins with a digit, or a `v` and a digit. Every placeholder this estate uses
	// (CHART_VERSION, APP_VERSION, SOURCE_REF, {{ ... }}) fails that.
	versionRe = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+`)

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

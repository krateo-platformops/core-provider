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
// The obvious suggestion — "ask installer to add a pin entry" — is the WRONG one here, and the
// installer session said so before anyone acted on it. core-provider is not pinned as a composition
// at all: `core-provider` and `core-provider-crds` are the two subchart DEPENDENCIES the umbrella
// declares in its own `chart/Chart.yaml`. There is no pin entry to add, so this needs a different
// resolver rather than a change on their side.
//
// It is still a pin in every sense that matters: a reviewable number that moves only through a PR in
// installer's repo, and the version actually deployed. At the time of writing it reads 2.13.57 —
// which is the CDC version running on krateo-057, while latest-published is 2.13.61. That gap is the
// whole argument: latest-published would have measured against a chart no cluster is running.
//
// So the order is: an explicit version, else the umbrella's declared dependency, else
// latest-published. The last is a fallback and not a default, and Source names which rule was used
// on every run, because a baseline that moves when this repo publishes means a tightening introduced
// and published in one release compares against itself — and the run log is the only place that
// becomes visible.

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

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
		version, declaredErr = umbrellaDependencyVersion(chart)
		if declaredErr == nil {
			source = "installer chart/Chart.yaml dependencies[] (the deployed, reviewable pin)"
		} else {
			var err error
			version, err = latestPublished(ref)
			if err != nil {
				// Constraint 3, and both reasons are reported: a caller that cannot tell this
				// apart from "clean" will report clean, and knowing only that the fallback failed
				// hides that the preferred resolver failed first.
				return nil, fmt.Errorf("could not determine a baseline version for %s. The "+
					"umbrella's declared dependency could not be read (%v) and neither could the "+
					"latest published version (%w)", chart, declaredErr, err)
			}
			// Named as a FALLBACK in the run log, because this is the one resolution under which
			// the baseline moves when this repo publishes.
			source = fmt.Sprintf("latest published — FALLBACK, the umbrella's declared "+
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

// UmbrellaChartURL is installer's own Chart.yaml, where the two core-provider subcharts are pinned.
const UmbrellaChartURL = "https://raw.githubusercontent.com/krateo-platformops/installer/main/" +
	"chart/Chart.yaml"

// umbrellaDependencyVersion reads the version installer's umbrella DECLARES for `chart`.
//
// Preferred over latest-published because it is the version actually deployed and it only moves
// through a PR in that repo — the same two properties a `component-pins.yaml` entry has, which is
// what makes this a real pin rather than a convenient guess.
func umbrellaDependencyVersion(chart string) (string, error) {
	// Three attempts: a transient blip should not red an unrelated PR. It must not SILENTLY fall
	// through to latest-published either, which is why the caller records the reason in Source.
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(2*attempt) * time.Second)
		}
		resp, err := http.Get(UmbrellaChartURL)
		if err != nil {
			lastErr = err
			continue
		}
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("GET %s returned %d", UmbrellaChartURL, resp.StatusCode)
			continue
		}
		return dependencyVersion(body, chart)
	}
	return "", fmt.Errorf("after 3 attempts: %w", lastErr)
}

// dependencyVersion is the parsing, split out so it is testable without the network.
func dependencyVersion(chartYAML []byte, chart string) (string, error) {
	var doc struct {
		Dependencies []struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"dependencies"`
	}
	if err := yaml.Unmarshal(chartYAML, &doc); err != nil {
		return "", fmt.Errorf("parsing the umbrella Chart.yaml: %w", err)
	}
	for _, d := range doc.Dependencies {
		if d.Name == chart {
			if d.Version == "" {
				return "", fmt.Errorf("the umbrella declares %s with no version", chart)
			}
			// A templated placeholder is not a version. The umbrella's own `version:` field reads
			// CHART_VERSION until release time and a dependency could acquire the same treatment;
			// resolving that string would produce a pull failure several steps later, attributed
			// to the registry rather than to the placeholder.
			//
			// Asserted as a POSITIVE shape rather than by listing the placeholder spellings: a
			// denylist of "contains { or $ or an underscore" passes the next placeholder nobody
			// predicted, while "looks like a version" fails every one of them.
			if !versionRe.MatchString(d.Version) {
				return "", fmt.Errorf("the umbrella declares %s at %q, which is not a version — "+
					"most likely a templated placeholder resolved at release time", chart, d.Version)
			}
			return d.Version, nil
		}
	}
	return "", fmt.Errorf("the umbrella declares no dependency named %s (it declares %d)",
		chart, len(doc.Dependencies))
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

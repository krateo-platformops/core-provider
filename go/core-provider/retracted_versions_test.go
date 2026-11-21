package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/mod/semver"
	"sigs.k8s.io/yaml"
)

// The retraction list at the repo root is read by installer's CI, which refuses a pin on a listed
// version. An entry is this repo vouching for its OWN releases, so it may only name charts this repo
// publishes: the set is read from the release verification script, the one place that lists every
// chart a tag publishes, so the two cannot disagree. (installer also enforces a set on its side.)
func TestRetractedVersions(t *testing.T) {
	root := filepath.Join("..", "..")
	raw, err := os.ReadFile(filepath.Join(root, "retracted-versions.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile(filepath.Join(root, ".github", "scripts", "verify-release-artifacts.sh"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^charts=\(([^)]*)\)`).FindSubmatch(script)
	if m == nil {
		t.Fatal("no charts=(...) line in verify-release-artifacts.sh; cannot know what this repo publishes")
	}
	published := map[string]bool{}
	for _, c := range strings.Fields(string(m[1])) {
		published[c] = true
	}
	if len(published) == 0 {
		t.Fatal("verify-release-artifacts.sh lists no charts")
	}

	var doc struct {
		Retracted []struct {
			Version  string   `json:"version"`
			Versions []string `json:"versions"`
			Charts   []string `json:"charts"`
			Use      string   `json:"use"`
			Reason   string   `json:"reason"`
		} `json:"retracted"`
	}
	if err := yaml.UnmarshalStrict(raw, &doc); err != nil {
		t.Fatalf("retracted-versions.yaml: %v", err)
	}
	if len(doc.Retracted) == 0 {
		t.Fatal("premise: the list has no entries, so nothing below is checked")
	}
	sv := func(v string) string { return "v" + v }
	inEntry := func(v string) bool {
		for _, e := range doc.Retracted {
			if e.Version != "" && semver.Compare(sv(v), sv(e.Version)) == 0 {
				return true
			}
			for _, x := range e.Versions {
				if semver.Compare(sv(v), sv(x)) == 0 {
					return true
				}
			}
		}
		return false
	}
	for i, e := range doc.Retracted {
		// The consumer's shape (installer's check-retracted-versions.py, entry_versions): exactly one of
		// `version:` or a NON-EMPTY `versions:` list. Anything else is an error there, which blocks
		// every installer PR, so it is refused here first.
		if (e.Version == "") == (len(e.Versions) == 0) {
			t.Errorf("entry %d: exactly one of version or a non-empty versions list is required", i)
		}
		for _, v := range append([]string{e.Version, e.Use}, e.Versions...) {
			if v != "" && !semver.IsValid(sv(v)) {
				t.Errorf("entry %d: %q is not a version", i, v)
			}
		}
		if len(e.Charts) == 0 {
			t.Errorf("entry %d: names no chart", i)
		}
		for _, c := range e.Charts {
			if !published[c] {
				t.Errorf("entry %d: chart %q is not published by this repo (published: %s)", i, c, m[1])
			}
		}
		if e.Use == "" || inEntry(e.Use) {
			t.Errorf("entry %d: use %q must be a version that is not itself retracted", i, e.Use)
		}
		if strings.TrimSpace(e.Reason) == "" {
			t.Errorf("entry %d: no reason", i)
		}
	}
}

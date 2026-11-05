package crdtightening

import "testing"

// helm show chart prints the metadata with keys in ALPHABETICAL order, so `dependencies` precedes
// the top-level `version`. Rows span the shapes a line scan confuses: a dependency version printed
// first, a nested key spelled `version:` with no dependencies, and the plain chart (the control).
func TestChartVersionReadsTheTopLevelVersion(t *testing.T) {
	cases := []struct {
		name, metadata, want string
		wantErr              bool
	}{
		{"dependencies printed before version — the top-level one, not the subchart's", `apiVersion: v2
dependencies:
- name: sub
  repository: oci://example.invalid/charts
  version: 0.9.1
description: umbrella
name: core-provider-crds
type: application
version: 2.13.63
`, "2.13.63", false},
		{"no dependencies — the control", "apiVersion: v2\nname: core-provider-crds\nversion: 2.13.63\n", "2.13.63", false},
		{"only a nested version — refused, not mistaken for the chart's", `apiVersion: v2
annotations:
  version: 1.0.0
name: x
`, "", true},
		{"a placeholder — refused", "apiVersion: v2\nname: x\nversion: CHART_VERSION\n", "", true},
		{"not YAML — refused", "::::", "", true},
	}
	for _, c := range cases {
		got, err := chartVersion([]byte(c.metadata))
		if c.wantErr {
			if err == nil {
				t.Errorf("%s: got %q with no error", c.name, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%s: got %q, %v; want %q", c.name, got, err, c.want)
		}
	}
	t.Logf("%d rows", len(cases))
}

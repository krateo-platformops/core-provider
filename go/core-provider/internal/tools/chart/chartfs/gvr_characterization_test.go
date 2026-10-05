// Characterization test for the GroupVersionKind derived from a chart's name and version.
//
// This is not testing that the derivation is CORRECT -- there is no independent definition of
// correct here. It pins what the derivation currently PRODUCES, because the output is a published
// contract that things outside this repository depend on, and a change to it is silent at the point
// of change.
//
// The downstream consumers that prompted this: the krateo-aws-blueprint, krateo-gcp-blueprint and
// krateo-azure-blueprint catalogues generate Composition CRDs by reproducing this derivation --
// roughly 737 charts at first release (AWS 242, GCP 206 Config Connector stable resources, Azure
// 289 GA), and growing as each provider's surface grows.
//
// They share one generator core with per-provider profiles, so the derivation exists in exactly ONE
// place across all three. That makes this guard more valuable rather than less: a single upstream
// flect change would rename every Composition type in all three catalogues at once, through one
// code path, and nothing in this repository's tests would have said so.
//
// A reviewer changing strutil.ToGolangName or the flect version now sees this file fail and has to
// decide deliberately -- which is the whole point.
//
// Note what this does NOT cover, so the next reader does not over-trust it: the RESOURCE (plural)
// is not derived here. Pluralizer.GVKtoGVR calls plumbing/kubeutil/plurals.Get, whose default
// resolver reads the plural back from apiserver DISCOVERY -- so the plural is whatever
// crdgen/controller-gen stamped into spec.names.plural, and it can move via a flect bump reaching
// controller-gen through plumbing without any source change here. That guard belongs next to
// crdgen, not in this file.
package chartfs

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"testing"
)

// chartTGZ builds the smallest archive FromReader accepts: one root dir holding a Chart.yaml.
// Driving the real GroupVersionKind through the real ChartFS matters -- a test that re-stated
// `flect.Pascalize(strutil.ToGolangName(name))` would keep passing if gvr.go stopped calling it.
func chartTGZ(t *testing.T, name, version string) *ChartFS {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := fmt.Sprintf("apiVersion: v2\nname: %s\nversion: %s\n", name, version)
	if err := tw.WriteHeader(&tar.Header{
		Name: name + "/Chart.yaml", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	fs, err := FromReader(&buf, "oci://example.invalid/"+name)
	if err != nil {
		t.Fatalf("building ChartFS for %q: %v", name, err)
	}
	return fs
}

func TestGroupVersionKind_Characterization(t *testing.T) {
	// Names are real published charts, plus edge shapes. Values are OBSERVED output, recorded on
	// 2026-10-05, not predicted -- a predicted table would encode the author's model of the
	// derivation rather than the derivation.
	cases := []struct{ chart, wantKind string }{
		{"builder-publish", "BuilderPublish"},
		{"builder-gate", "BuilderGate"},
		{"nightly-review", "NightlyReview"},
		{"alert-troubleshooter", "AlertTroubleshooter"},
		{"access-review", "AccessReview"},
		{"blueprint-render-service", "BlueprintRenderService"},
		{"incident-controller", "IncidentController"},
		{"krateo-observability", "KrateoObservability"},
		{"krateo-aws-blueprint", "KrateoAwsBlueprint"},

		// Acronym: SSE does not survive as SSE. Downstream catalogues must expect KrateoSseProxy.
		{"krateo-sse-proxy", "KrateoSseProxy"},
		{"krateo-oas-gitops", "KrateoOasGitops"},

		// Single segment, no hyphens.
		{"postgresql", "Postgresql"},
		{"redis-ha", "RedisHa"},

		// Degenerate shapes: one letter, and single-letter segments collapsing with no separator.
		{"a", "A"},
		{"x-y-z", "Xyz"},
	}

	for _, tc := range cases {
		t.Run(tc.chart, func(t *testing.T) {
			gvk, err := GroupVersionKind(chartTGZ(t, tc.chart, "1.2.3"))
			if err != nil {
				t.Fatalf("GroupVersionKind(%q): %v", tc.chart, err)
			}
			if gvk.Kind != tc.wantKind {
				t.Errorf("Kind for chart %q = %q, want %q\n"+
					"This derivation is reproduced by the AWS/GCP/Azure blueprint catalogues (~737 charts at\n"+
					"first release) through one shared generator core.\n"+
					"Changing it renames every Composition type they generate. If the change is intended, "+
					"update this table AND tell those repos.", tc.chart, gvk.Kind, tc.wantKind)
			}
			if gvk.Group != "composition.krateo.io" {
				t.Errorf("Group = %q, want composition.krateo.io", gvk.Group)
			}
		})
	}
}

// The version segment is part of the same published contract: chart 1.2.3 becomes v1-2-3, so a
// chart version bump changes the CRD's served version. Pinned for the same reason as Kind.
func TestGroupVersionKind_VersionMangling(t *testing.T) {
	for _, tc := range []struct{ chartVersion, wantAPIVersion string }{
		{"1.2.3", "v1-2-3"},
		{"0.1.0", "v0-1-0"},
		{"10.0.1", "v10-0-1"},
	} {
		gvk, err := GroupVersionKind(chartTGZ(t, "demo", tc.chartVersion))
		if err != nil {
			t.Fatalf("chart version %q: %v", tc.chartVersion, err)
		}
		if gvk.Version != tc.wantAPIVersion {
			t.Errorf("chart version %q -> API version %q, want %q", tc.chartVersion, gvk.Version, tc.wantAPIVersion)
		}
	}
}

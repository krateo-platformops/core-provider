package archive

import (
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// definition is a CompositionDefinition at current chart 0.3.48 (status.apiVersion v0-3-48) that
// records the chart of v0-3-47 from when it was current, as core-provider's UpdateVersionInfo does.
func definition(withStatus bool, records ...map[string]any) *unstructured.Unstructured {
	cd := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "core.krateo.io/v1alpha1", "kind": "CompositionDefinition",
		"metadata": map[string]any{"name": "glance", "namespace": "openstack"},
		"spec": map[string]any{"chart": map[string]any{
			"url": "oci://ghcr.io/x/glance", "version": "0.3.48", "insecureSkipVerifyTLS": true,
			"credentials": map[string]any{"username": "u", "passwordRef": map[string]any{"name": "s", "namespace": "openstack", "key": "token"}},
		}},
	}}
	if withStatus {
		vi := make([]any, 0, len(records))
		for _, r := range records {
			vi = append(vi, r)
		}
		cd.Object["status"] = map[string]any{
			"apiVersion": "composition.krateo.io/v0-3-48", "kind": "Glance",
			"managed": map[string]any{"versionInfo": vi},
		}
	}
	return cd
}

var recorded047 = map[string]any{"version": "v0-3-47", "served": true, "chart": map[string]any{
	"url": "oci://ghcr.io/x/glance", "version": "0.3.47",
}}

// #461 (Diego, 2026-11-13): each controller renders the chart recorded for its own version.
func TestChartForVersion(t *testing.T) {
	t.Run("the current version's controller renders spec.chart, unchanged", func(t *testing.T) {
		c, err := chartForVersion(definition(true, recorded047), "v0-3-48")
		if err != nil || c.version != "0.3.48" || c.source != "spec.chart" || c.username != "u" || c.passwordRef["key"] != "token" {
			t.Errorf("got %+v, %v; want spec.chart at 0.3.48 with its credentials", c, err)
		}
	})
	t.Run("an older version's controller renders ITS recorded chart, not the current one", func(t *testing.T) {
		c, err := chartForVersion(definition(true, recorded047), "v0-3-47")
		if err != nil || c.version != "0.3.47" || c.source != "status.managed.versionInfo[v0-3-47].chart" {
			t.Errorf("got %+v, %v; want the recorded 0.3.47 chart", c, err)
		}
	})
	t.Run("an older version with no recorded chart refuses, never borrows another", func(t *testing.T) {
		noChart := map[string]any{"version": "v0-3-46", "served": true}
		for name, cd := range map[string]*unstructured.Unstructured{
			"no record at all":     definition(true, recorded047),
			"record without chart": definition(true, recorded047, noChart),
		} {
			c, err := chartForVersion(cd, "v0-3-46")
			if !errors.Is(err, errNoChartForVersion) {
				t.Errorf("%s: got %+v, %v; want errNoChartForVersion", name, c, err)
			}
		}
	})
	t.Run("no status yet: the desired version renders spec.chart, an older one refuses", func(t *testing.T) {
		if c, err := chartForVersion(definition(false), "v0-3-48"); err != nil || c.version != "0.3.48" || c.source != "spec.chart" {
			t.Errorf("desired version: got %+v, %v; want spec.chart", c, err)
		}
		if _, err := chartForVersion(definition(false), "v0-3-47"); !errors.Is(err, errNoChartForVersion) {
			t.Errorf("an older version with no status (so no record): err %v, want errNoChartForVersion", err)
		}
	})
}

// #461, recurrence on 2.13.91 (osh-052, keystone 0.3.52 -> 0.3.53): on a bump spec.chart.version moves
// first and core-provider writes status.apiVersion only at the end of its Update (~40s there, a
// conflict retry included). In that window the RETIRING controller's version still equals the stale
// status.apiVersion. 2.13.91 decided "current" from status, so it rendered spec.chart (the NEW chart)
// and was killed mid-upgrade. Deciding from spec, it renders its own recorded chart.
func TestChartForVersionDecidesCurrentFromSpecNotStatus(t *testing.T) {
	cd := definition(true, recorded047)
	// The window: status still at the old version, spec already at the new one.
	_ = unstructured.SetNestedField(cd.Object, "composition.krateo.io/v0-3-47", "status", "apiVersion")
	c, err := chartForVersion(cd, "v0-3-47")
	if err != nil || c.version != "0.3.47" || c.source != "status.managed.versionInfo[v0-3-47].chart" {
		t.Fatalf("the retiring controller in the spec/status window rendered %+v (%v); want its own recorded 0.3.47, never spec.chart 0.3.48", c, err)
	}
	// Control: the new version's controller in the same window renders spec.chart.
	if c, err := chartForVersion(cd, "v0-3-48"); err != nil || c.version != "0.3.48" || c.source != "spec.chart" {
		t.Errorf("the desired version's controller in the window rendered %+v (%v); want spec.chart 0.3.48", c, err)
	}
}

// #476: the TLS flag is the API's insecureSkipVerifyTLS. The getter used to read
// "insecureSkipTLSverify", which the CRD does not have, so the flag was always false.
func TestChartForVersionHonoursInsecureSkipVerifyTLS(t *testing.T) {
	c, err := chartForVersion(definition(true), "v0-3-48")
	if err != nil || !c.insecureSkipVerifyTLS {
		t.Errorf("spec.chart.insecureSkipVerifyTLS=true read as %v (err %v)", c.insecureSkipVerifyTLS, err)
	}
	// Control: the misspelled key is not the API's and must not be read.
	cd := definition(true)
	chart := cd.Object["spec"].(map[string]any)["chart"].(map[string]any)
	delete(chart, "insecureSkipVerifyTLS")
	chart["insecureSkipTLSverify"] = true
	if c, _ := chartForVersion(cd, "v0-3-48"); c.insecureSkipVerifyTLS {
		t.Error("the misspelled insecureSkipTLSverify key must not be honoured")
	}
}

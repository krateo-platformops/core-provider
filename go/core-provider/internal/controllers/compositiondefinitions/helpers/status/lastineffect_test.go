package status

import (
	"testing"

	rtv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	v1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
)

// #609 (Diego, 2026-11-22: "the last in effect"): the CURRENT version's chart record is refreshed
// from spec.chart on every call; other versions' records are frozen.
func TestUpdateVersionInfo_609_LastInEffect(t *testing.T) {
	crd := &apiextensionsv1.CustomResourceDefinition{Spec: apiextensionsv1.CustomResourceDefinitionSpec{
		Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
			{Name: "v1-0-0", Served: true}, {Name: "v2-0-0", Served: true, Storage: true}}}}
	gvr := func(v string) schema.GroupVersionResource {
		return schema.GroupVersionResource{Group: "composition.krateo.io", Version: v, Resource: "widgets"}
	}
	creds := &v1alpha1.Credentials{Username: "u",
		PasswordRef: rtv1.SecretKeySelector{Key: "k", Reference: rtv1.Reference{Name: "s", Namespace: "n"}}}
	cd := func(url string, c *v1alpha1.Credentials, entries ...v1alpha1.VersionDetail) *v1alpha1.CompositionDefinition {
		return &v1alpha1.CompositionDefinition{
			Spec: v1alpha1.CompositionDefinitionSpec{Chart: &v1alpha1.ChartInfo{
				Url: url, Version: "2.0.0", Repo: "r-now", InsecureSkipVerifyTLS: true, Credentials: c}},
			Status: v1alpha1.CompositionDefinitionStatus{Managed: v1alpha1.Managed{VersionInfo: entries}},
		}
	}
	entry := func(v, url string) v1alpha1.VersionDetail {
		return v1alpha1.VersionDetail{Version: v, Chart: &v1alpha1.ChartInfoProps{Url: url, Version: "x", Repo: "r-then"}}
	}
	chartOf := func(t *testing.T, cr *v1alpha1.CompositionDefinition, v string) *v1alpha1.ChartInfoProps {
		t.Helper()
		for _, vi := range cr.Status.Managed.VersionInfo {
			if vi.Version == v {
				return vi.Chart
			}
		}
		t.Fatalf("no versionInfo entry for %s", v)
		return nil
	}

	t.Run("an existing current entry with a stale chart is refreshed from spec", func(t *testing.T) {
		cr := cd("oci://NEW/w", nil, entry("v1-0-0", "oci://V1/w"), entry("v2-0-0", "oci://OLD/w"))
		UpdateVersionInfo(cr, crd, gvr("v2-0-0"))
		c := chartOf(t, cr, "v2-0-0")
		if c == nil || c.Url != "oci://NEW/w" || c.Version != "2.0.0" || c.Repo != "r-now" || !c.InsecureSkipVerifyTLS || c.Credentials != nil {
			t.Fatalf("current entry must equal spec.chart, got %+v", c)
		}
	})
	t.Run("a non-current entry is left untouched", func(t *testing.T) {
		cr := cd("oci://NEW/w", nil, entry("v1-0-0", "oci://V1/w"), entry("v2-0-0", "oci://OLD/w"))
		UpdateVersionInfo(cr, crd, gvr("v2-0-0"))
		c := chartOf(t, cr, "v1-0-0")
		if c == nil || c.Url != "oci://V1/w" || c.Version != "x" || c.Repo != "r-then" || c.InsecureSkipVerifyTLS {
			t.Fatalf("a non-current version's chart is frozen, got %+v", c)
		}
	})
	t.Run("a new current entry is populated (unchanged behaviour)", func(t *testing.T) {
		cr := cd("oci://NEW/w", nil, entry("v1-0-0", "oci://V1/w"))
		UpdateVersionInfo(cr, crd, gvr("v2-0-0"))
		if c := chartOf(t, cr, "v2-0-0"); c == nil || c.Url != "oci://NEW/w" {
			t.Fatalf("new current entry must be populated from spec, got %+v", c)
		}
		if c := chartOf(t, cr, "v1-0-0"); c == nil || c.Url != "oci://V1/w" {
			t.Fatalf("control: the existing non-current entry is untouched, got %+v", c)
		}
	})
	t.Run("an absent spec.chart keeps the current record and does not panic", func(t *testing.T) {
		cr := cd("ignored", nil, entry("v2-0-0", "oci://OLD/w"))
		cr.Spec.Chart = nil
		UpdateVersionInfo(cr, crd, gvr("v2-0-0"))
		if c := chartOf(t, cr, "v2-0-0"); c == nil || c.Url != "oci://OLD/w" {
			t.Fatalf("with no spec.chart the existing record must be kept, got %+v", c)
		}
		cr = cd("ignored", nil)
		cr.Spec.Chart = nil
		UpdateVersionInfo(cr, crd, gvr("v2-0-0")) // a new current entry with no spec.chart
		if c := chartOf(t, cr, "v2-0-0"); c != nil {
			t.Fatalf("a new entry with no spec.chart has no chart to record, got %+v", c)
		}
	})
	t.Run("credentials set are copied, not aliased; credentials nil clears them", func(t *testing.T) {
		cr := cd("oci://NEW/w", creds, entry("v2-0-0", "oci://OLD/w"))
		UpdateVersionInfo(cr, crd, gvr("v2-0-0"))
		c := chartOf(t, cr, "v2-0-0")
		if c.Credentials == nil || *c.Credentials != *creds {
			t.Fatalf("credentials must be copied, got %+v", c.Credentials)
		}
		if c.Credentials == cr.Spec.Chart.Credentials {
			t.Fatal("the status must not alias spec.chart.credentials")
		}
		cr.Spec.Chart.Credentials = nil // the author removes them at the same version
		UpdateVersionInfo(cr, crd, gvr("v2-0-0"))
		if c := chartOf(t, cr, "v2-0-0"); c.Credentials != nil {
			t.Fatalf("removed credentials must be removed from the current record, got %+v", c.Credentials)
		}
	})
}

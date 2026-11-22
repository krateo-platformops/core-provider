package status

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
)

// updateVersionInfo updates the version information of a CompositionDefinition custom resource
// based on the provided CustomResourceDefinition and GroupVersionResource.
//
// The function iterates through the versions specified in the CustomResourceDefinition and updates
// the corresponding version information in the CompositionDefinition's status. If a version is not
// found in the existing status, it is added.
//
// gvr is the CURRENT version: every caller derives it from the chart fetched for the current
// spec.chart (Observe, Update, observeFollower), never from an older version. Its entry's chart is
// refreshed from spec.chart on EVERY call, not only when the entry is first added, so it always holds
// the chart in effect for that version (#609, Diego 2026-11-22: "the last in effect"). Entries for
// other versions are left untouched: once a version stops being current its chart is frozen at the
// last one in effect, which is what that version's controller (instances held there under Manual or
// Paused) renders (cdc archive/getter.go chartForVersion reads versionInfo[own].chart). Before #609
// the chart was copied only on first append, so a same-version change to spec.chart.url, repo,
// credentials or insecureSkipVerifyTLS never reached the record, and an older version rendered the
// first chart it was ever registered with.
//
// spec.chart is optional in the schema (omitempty, not required). When it is absent there is no chart
// to record, so the existing record is KEPT rather than cleared: clearing would erase the only chart
// that version's controller can render, on an input that says nothing about it.
//
// The current version is identified by gvr.Version (from the fetched chart's Chart.yaml), not by
// versionname.ForChart(spec.chart.version): spec.chart.version is optional (a tgz URL), and for such a
// chart the latter is "v", which no entry carries.
func UpdateVersionInfo(cr *compositiondefinitionsv1alpha1.CompositionDefinition, crd *apiextensionsv1.CustomResourceDefinition, gvr schema.GroupVersionResource) {
	for _, v := range crd.Spec.Versions {
		i := -1
		for j, cv := range cr.Status.Managed.VersionInfo {
			if cv.Version == v.Name {
				i = j
				break
			}
		}

		if i == -1 {
			var versionDetail compositiondefinitionsv1alpha1.VersionDetail
			versionDetail.Version = v.Name
			versionDetail.Served = v.Served
			versionDetail.Stored = v.Storage

			if gvr.Version == versionDetail.Version && cr.Spec.Chart != nil {
				versionDetail.Chart = chartFromSpec(cr)
			}

			cr.Status.Managed.VersionInfo = append(cr.Status.Managed.VersionInfo, versionDetail)
			continue
		}
		cr.Status.Managed.VersionInfo[i].Served = v.Served
		cr.Status.Managed.VersionInfo[i].Stored = v.Storage
		if gvr.Version == v.Name && cr.Spec.Chart != nil {
			cr.Status.Managed.VersionInfo[i].Chart = chartFromSpec(cr) // #609: the last in effect
		}
	}

	// Make VersionInfo a true projection of the live CRD: drop entries whose version is no longer in
	// the CRD (pruned by served-version GC). Without this VersionInfo is append-only and accumulates
	// dead versions forever, drifting from reality (#234A) and driving redundant migrate/undeploy
	// passes over versions that no longer exist. Safe: served-version pruning only removes a version
	// once no instance carries its label and no other definition references it, so a version dropped
	// here can have no stragglers left to migrate. The per-version Chart of surviving entries is kept.
	live := make(map[string]struct{}, len(crd.Spec.Versions))
	for _, v := range crd.Spec.Versions {
		live[v.Name] = struct{}{}
	}
	kept := cr.Status.Managed.VersionInfo[:0]
	for _, vi := range cr.Status.Managed.VersionInfo {
		if _, ok := live[vi.Version]; ok {
			kept = append(kept, vi)
		}
	}
	cr.Status.Managed.VersionInfo = kept
}

// chartFromSpec is the chart record for the current version: a copy of spec.chart's fields. The
// Credentials pointer is deep-copied so the status never aliases the spec.
func chartFromSpec(cr *compositiondefinitionsv1alpha1.CompositionDefinition) *compositiondefinitionsv1alpha1.ChartInfoProps {
	c := &compositiondefinitionsv1alpha1.ChartInfoProps{
		InsecureSkipVerifyTLS: cr.Spec.Chart.InsecureSkipVerifyTLS,
		Repo:                  cr.Spec.Chart.Repo,
		Url:                   cr.Spec.Chart.Url,
		Version:               cr.Spec.Chart.Version,
	}
	if cr.Spec.Chart.Credentials != nil {
		c.Credentials = cr.Spec.Chart.Credentials.DeepCopy()
	}
	return c
}

func RefreshCompositionDefinitionStatus(
	cr *compositiondefinitionsv1alpha1.CompositionDefinition,
	crd *apiextensionsv1.CustomResourceDefinition,
	gvr schema.GroupVersionResource,
	gvk schema.GroupVersionKind,
	packageURL string,
) error {
	UpdateVersionInfo(cr, crd, gvr)
	cr.Status.Managed.Group = crd.Spec.Group
	cr.Status.Managed.Kind = crd.Spec.Names.Kind
	cr.Status.ApiVersion, cr.Status.Kind = gvk.ToAPIVersionAndKind()
	cr.Status.Resource = gvr.Resource
	cr.Status.PackageURL = packageURL

	return nil
}

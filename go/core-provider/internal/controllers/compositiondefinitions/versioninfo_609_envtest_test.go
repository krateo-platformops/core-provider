//go:build envtest

package compositiondefinitions

import (
	"bytes"
	"context"
	"testing"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/controllers/compositiondefinitions/helpers/status"
	"github.com/krateo-platformops/core-provider/internal/tools/chart"
	"github.com/krateo-platformops/core-provider/internal/tools/chart/chartfs"
	crdutils "github.com/krateo-platformops/core-provider/internal/tools/crd/generation"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// #609 on a real apiserver (Diego, 2026-11-22: "the last in effect"). A definition registered at
// 1.0.0 from oci://OLD changes spec.chart.url to oci://NEW without a version bump, reconciles, then
// moves to 2.0.0. The 1.0.0 controller (instances held there under Manual or Paused) renders
// status.managed.versionInfo[v1-0-0].chart (cdc archive/getter.go chartForVersion), so that record
// must be oci://NEW, the chart in effect when the definition left 1.0.0. Before #609 the record was
// written only when the entry was first appended, so it stayed oci://OLD.
//
// Each step runs the calls Create/Observe/Update make (applyCompositionCRD, then
// RefreshCompositionDefinitionStatus with the gvr it returned), and the status is written to and read
// back from the apiserver between steps, so the record that survives is the one admission keeps.
// CONTROL: without the same-version change, versionInfo[v1-0-0] keeps oci://OLD (the arm that
// distinguishes "refreshed from spec" from "always the latest URL").
func TestEnvtest_609_VersionInfoChartIsTheLastInEffect(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name          string
		sameVersionTo string // "" = no same-version change (control)
		wantV1        string
	}{
		{name: "same-version URL change then bump", sameVersionTo: "oci://NEW/widget", wantV1: "oci://NEW/widget"},
		{name: "control: bump without a same-version change", sameVersionTo: "", wantV1: "oci://OLD/widget"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cl, dyn := startAddEnv(t)
			if err := cl.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-609"}}); err != nil {
				t.Fatal(err)
			}
			cr := &compositiondefinitionsv1alpha1.CompositionDefinition{ObjectMeta: metav1.ObjectMeta{Name: "widget", Namespace: "team-609"}}
			cr.Spec.Chart = &compositiondefinitionsv1alpha1.ChartInfo{Url: "oci://OLD/widget", Version: "1.0.0"}
			if err := cl.Create(ctx, cr); err != nil {
				t.Fatalf("create CD: %v", err)
			}

			// reconcile applies the chart at spec.chart.version and refreshes status, then persists
			// the status through the apiserver and re-reads the CD.
			reconcile := func(t *testing.T) {
				t.Helper()
				pkg, err := chartfs.FromReader(bytes.NewReader(versionedChartTGZ(t, cr.Spec.Chart.Version)), "test://widget.tgz")
				if err != nil {
					t.Fatal(err)
				}
				gvk, err := chart.ChartGroupVersionKind(pkg.FS(), pkg.RootDir())
				if err != nil {
					t.Fatal(err)
				}
				sch, err := chart.ChartJsonSchema(pkg.FS(), pkg.RootDir())
				if err != nil {
					t.Fatal(err)
				}
				gen, err := crdutils.GenerateCRD(sch, gvk)
				if err != nil {
					t.Fatal(err)
				}
				gvr, err := newExternal(cl, dyn).applyCompositionCRD(ctx, cr, gvk, gen.DeepCopy())
				if err != nil {
					t.Fatalf("applyCompositionCRD: %v", err)
				}
				live := gen.DeepCopy()
				if err := cl.Get(ctx, types.NamespacedName{Name: gen.Name}, live); err != nil {
					t.Fatal(err)
				}
				if err := status.RefreshCompositionDefinitionStatus(cr, live, gvr, gvk, "test://widget.tgz"); err != nil {
					t.Fatal(err)
				}
				if err := cl.Status().Update(ctx, cr); err != nil {
					t.Fatalf("persist status: %v", err)
				}
				if err := cl.Get(ctx, client.ObjectKeyFromObject(cr), cr); err != nil {
					t.Fatal(err)
				}
			}
			updateSpec := func(t *testing.T, mutate func(*compositiondefinitionsv1alpha1.ChartInfo)) {
				t.Helper()
				mutate(cr.Spec.Chart)
				if err := cl.Update(ctx, cr); err != nil {
					t.Fatalf("update spec: %v", err)
				}
				if err := cl.Get(ctx, client.ObjectKeyFromObject(cr), cr); err != nil {
					t.Fatal(err)
				}
			}
			chartURL := func(t *testing.T, v string) string {
				t.Helper()
				for _, vi := range cr.Status.Managed.VersionInfo {
					if vi.Version == v {
						if vi.Chart == nil {
							t.Fatalf("versionInfo[%s] has no chart; versionInfo=%+v", v, cr.Status.Managed.VersionInfo)
						}
						return vi.Chart.Url
					}
				}
				t.Fatalf("no versionInfo entry for %s; versionInfo=%+v", v, cr.Status.Managed.VersionInfo)
				return ""
			}

			reconcile(t)
			if got := chartURL(t, "v1-0-0"); got != "oci://OLD/widget" {
				t.Fatalf("premise: after registration versionInfo[v1-0-0] = %q, want oci://OLD/widget", got)
			}
			if tc.sameVersionTo != "" {
				updateSpec(t, func(c *compositiondefinitionsv1alpha1.ChartInfo) { c.Url = tc.sameVersionTo })
				reconcile(t)
				if got := chartURL(t, "v1-0-0"); got != tc.sameVersionTo {
					t.Fatalf("while 1.0.0 is current, versionInfo[v1-0-0] = %q, want spec.chart.url %q", got, tc.sameVersionTo)
				}
			}
			updateSpec(t, func(c *compositiondefinitionsv1alpha1.ChartInfo) { c.Url = "oci://V2/widget"; c.Version = "2.0.0" })
			reconcile(t)

			if got := chartURL(t, "v1-0-0"); got != tc.wantV1 {
				t.Fatalf("after the bump versionInfo[v1-0-0].chart.url = %q, want %q (the chart in effect when the definition left 1.0.0)", got, tc.wantV1)
			}
			if got := chartURL(t, "v2-0-0"); got != "oci://V2/widget" {
				t.Fatalf("versionInfo[v2-0-0].chart.url = %q, want spec.chart.url oci://V2/widget", got)
			}
		})
	}
}

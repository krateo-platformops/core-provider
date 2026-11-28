//go:build envtest

package compositiondefinitions

import (
	"reflect"
	"sync/atomic"
	"testing"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/tools/writtenby"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// core-provider#671 on a real apiserver, through provider-runtime's own reconciler and the real
// connector. The CRD is the generated one (crds/), so a field it did not declare would be pruned
// and read back absent: the expectations are read from what the apiserver SERVED.

const twoMaintainers671 = "maintainers:\n  - name: Ada\n    email: ada@example.com\n    url: https://example.com/ada\n  - name: Bob\n    email: bob@example.com\n"

func TestEnvtest_671_ChartMaintainers(t *testing.T) {
	e := start644(t)
	e.charts.addChart(t, "owned", "1.0.0", `{"type":"object"}`, twoMaintainers671)
	d := e.define(t, "owned", "owned", nil)
	d.rest(6)
	got := d.cd().Status.Chart
	want := compositiondefinitionsv1alpha1.ChartStatus{Version: "1.0.0", Maintainers: []compositiondefinitionsv1alpha1.ChartMaintainer{
		{Name: "Ada", Email: "ada@example.com", URL: "https://example.com/ada"},
		{Name: "Bob", Email: "bob@example.com"},
	}}
	if got == nil || !reflect.DeepEqual(*got, want) {
		t.Fatalf("status.chart = %+v, want %+v", got, want)
	}
}

func TestEnvtest_671_ChartWithoutMaintainers(t *testing.T) {
	e := start644(t)
	e.charts.add(t, "unowned", "1.0.0", `{"type":"object"}`)
	d := e.define(t, "unowned", "unowned", nil)
	d.rest(6)
	got := d.cd().Status.Chart
	if got == nil || got.Version != "1.0.0" {
		t.Fatalf("status.chart = %+v, want the version read (1.0.0) even with no maintainers", got)
	}
	if got.Maintainers != nil {
		t.Errorf("status.chart.maintainers = %+v, want it absent", got.Maintainers)
	}
	// What the apiserver stored carries no maintainers key at all, not an empty list.
	u, err := e.c.dyn.Resource(cdGVR644).Namespace(self642CDNS).Get(t.Context(), "unowned", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	chartMap, _ := u.Object["status"].(map[string]any)["chart"].(map[string]any)
	if _, has := chartMap["maintainers"]; has {
		t.Errorf("the stored status.chart has a maintainers key: %v", chartMap)
	}
	if chartMap["version"] != "1.0.0" {
		t.Errorf("the stored status.chart = %v, want version 1.0.0", chartMap)
	}
}

// THE NO-OP RULE: status.chart is set on the object the pass writes anyway; passes that change
// nothing else send the status and the apiserver records nothing.
func TestEnvtest_671_ChartInfoCausesNoExtraWrites(t *testing.T) {
	e := start644(t)
	e.charts.addChart(t, "quiet", "1.0.0", `{"type":"object"}`, twoMaintainers671)
	d := e.define(t, "quiet", "quiet", nil)
	var updates atomic.Int32
	d.r = e.reconciler669(writtenby.New("2.13.136", "c0ffee136"), &updates)
	d.rest(6)
	if d.cd().Status.Chart == nil {
		t.Fatal("control: status.chart was not recorded")
	}
	rv := d.cd().ResourceVersion
	updates.Store(0)
	const passes = 3
	for i := 0; i < passes; i++ {
		if _, err := d.pass(); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("%d passes sent %d status updates; resourceVersion %s -> %s", passes, updates.Load(), rv, d.cd().ResourceVersion)
	if updates.Load() < passes {
		t.Fatalf("control: %d status updates in %d passes; the passes did not reach the status write, so an unchanged resourceVersion would prove nothing", updates.Load(), passes)
	}
	if got := d.cd().ResourceVersion; got != rv {
		t.Errorf("at rest the definition is still being written: resourceVersion %s -> %s", rv, got)
	}
}

package compositiondefinitions

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	dynfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// chartYAMLTGZ is a chart "contents" whose Chart.yaml is exactly chartYAML.
func chartYAMLTGZ(t *testing.T, chartYAML string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range map[string]string{
		"contents/Chart.yaml":         chartYAML,
		"contents/values.yaml":        "{}\n",
		"contents/values.schema.json": `{"type":"object"}`,
		"contents/templates/t.yaml":   twoConfigMaps,
	} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

const maintainedChartYAML671 = "apiVersion: v2\nname: contents\nversion: 2.3.4\nmaintainers:\n  - name: Ada\n    email: ada@example.com\n    url: https://example.com/ada\n  - name: Bob\n"

// #671: status.chart is the chart's own Chart.yaml: the version READ from the chart (2.3.4), not
// spec.chart.version (1.0.0), and the maintainers as declared.
func TestObserve_RecordsChartMaintainersAndTheVersionRead(t *testing.T) {
	cd := observeContents(t, chartYAMLTGZ(t, maintainedChartYAML671), "1.0.0", nil, false)
	c := cd.Status.Chart
	if c == nil {
		t.Fatal("status.chart not recorded")
	}
	want := compositiondefinitionsv1alpha1.ChartStatus{Version: "2.3.4", Maintainers: []compositiondefinitionsv1alpha1.ChartMaintainer{
		{Name: "Ada", URL: "https://example.com/ada"}, {Name: "Bob"}}}
	if !reflect.DeepEqual(*c, want) {
		t.Errorf("status.chart = %+v, want %+v", *c, want)
	}
	// The chart declared ada@example.com; the marshalled status must not carry it or an email key.
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "ada@example.com") || strings.Contains(string(b), "email") {
		t.Errorf("marshalled status.chart carries the maintainer's email: %s", b)
	}
}

// A chart with no maintainers: the version is written, the list is nil (omitted), never empty.
func TestObserve_ChartWithoutMaintainersRecordsVersionOnly(t *testing.T) {
	cd := observeContents(t, chartYAMLTGZ(t, "apiVersion: v2\nname: contents\nversion: 2.3.4\n"), "1.0.0", nil, false)
	c := cd.Status.Chart
	if c == nil || c.Version != "2.3.4" || c.Maintainers != nil {
		t.Fatalf("status.chart = %+v, want version 2.3.4 and nil maintainers", c)
	}
}

// A new chart version without maintainers replaces the previous owner; an unreadable chart and a
// definition being deleted leave status.chart as it was.
func TestObserve_ChartInfoReplacedKeptAndNotWrittenWhileDeleting(t *testing.T) {
	prev := func() *compositiondefinitionsv1alpha1.ChartStatus {
		return &compositiondefinitionsv1alpha1.ChartStatus{Version: "1.0.0", Maintainers: []compositiondefinitionsv1alpha1.ChartMaintainer{{Name: "Old"}}}
	}
	run := func(tgz []byte, deleting bool) *compositiondefinitionsv1alpha1.ChartStatus {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if tgz == nil {
				http.NotFound(w, nil)
				return
			}
			_, _ = w.Write(tgz)
		}))
		t.Cleanup(srv.Close)
		cd := &compositiondefinitionsv1alpha1.CompositionDefinition{}
		cd.SetName("contents")
		cd.SetNamespace("krateo-system")
		cd.Spec.Chart = &compositiondefinitionsv1alpha1.ChartInfo{Url: srv.URL + "/contents-2.0.0.tgz", Version: "2.0.0"}
		cd.Status.Chart = prev()
		if deleting {
			now := metav1.Now()
			cd.SetDeletionTimestamp(&now)
			cd.SetFinalizers([]string{"finalizer.managedresource.krateo.io"})
		}
		kc := clientfake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(cd).WithStatusSubresource(cd).Build()
		e := &external{kube: kc, mgmt: kc, apiReader: kc, mgmtDynamic: dynfake.NewSimpleDynamicClient(scheme(t)),
			client: k8sfake.NewSimpleClientset(), log: logging.NewNopLogger(), pluralizer: notFoundPluralizer{}}
		_, _ = e.Observe(context.Background(), cd)
		return cd.Status.Chart
	}
	next := chartYAMLTGZ(t, "apiVersion: v2\nname: contents\nversion: 2.0.0\n")
	if got := run(next, false); got == nil || got.Version != "2.0.0" || got.Maintainers != nil {
		t.Errorf("new chart without maintainers: status.chart = %+v, want version 2.0.0, no maintainers", got)
	}
	if got := run(nil, false); !reflect.DeepEqual(got, prev()) {
		t.Errorf("unreadable chart: status.chart = %+v, want it kept as %+v", got, prev())
	}
	if got := run(next, true); !reflect.DeepEqual(got, prev()) {
		t.Errorf("definition being deleted: status.chart = %+v, want it kept as %+v", got, prev())
	}
}

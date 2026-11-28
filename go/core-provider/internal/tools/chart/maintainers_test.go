package chart

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
)

func chartYAML(body string) fstest.MapFS {
	return fstest.MapFS{"demo/Chart.yaml": &fstest.MapFile{Data: []byte("apiVersion: v2\nname: demo\n" + body)}}
}

// Expectations are literals, never built with the code under test.
func TestInfo(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
		want *v1alpha1.ChartStatus
	}{
		{"name only", "version: 1.2.3\nmaintainers:\n  - name: Ada\n",
			&v1alpha1.ChartStatus{Version: "1.2.3", Maintainers: []v1alpha1.ChartMaintainer{{Name: "Ada"}}}},
		{"email in the chart is not copied", "version: 1.2.3\nmaintainers:\n  - name: Ada\n    email: ada@example.com\n",
			&v1alpha1.ChartStatus{Version: "1.2.3", Maintainers: []v1alpha1.ChartMaintainer{{Name: "Ada"}}}},
		{"name, email and url", "version: 1.2.3\nmaintainers:\n  - name: Ada\n    email: ada@example.com\n    url: https://example.com/ada\n  - name: Bob\n    url: https://example.com/bob\n",
			&v1alpha1.ChartStatus{Version: "1.2.3", Maintainers: []v1alpha1.ChartMaintainer{
				{Name: "Ada", URL: "https://example.com/ada"},
				{Name: "Bob", URL: "https://example.com/bob"}}}},
		{"no maintainers key: version still written, list absent", "version: 4.5.6\n",
			&v1alpha1.ChartStatus{Version: "4.5.6"}},
		{"empty maintainers list: list absent, not empty", "version: 4.5.6\nmaintainers: []\n",
			&v1alpha1.ChartStatus{Version: "4.5.6"}},
		{"a maintainer with no name is skipped", "version: 1.2.3\nmaintainers:\n  - email: nobody@example.com\n  - name: Ada\n",
			&v1alpha1.ChartStatus{Version: "1.2.3", Maintainers: []v1alpha1.ChartMaintainer{{Name: "Ada"}}}},
		{"only nameless maintainers: list absent", "version: 1.2.3\nmaintainers:\n  - email: nobody@example.com\n",
			&v1alpha1.ChartStatus{Version: "1.2.3"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Info(chartYAML(tc.yaml), "demo")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
			if got.Maintainers != nil && len(got.Maintainers) == 0 {
				t.Error("maintainers is an empty non-nil list; it must be nil so the field is omitted")
			}
		})
	}
}

// The fixture's email address must be absent from the marshalled status, not merely from a field.
func TestInfoNeverCarriesEmail(t *testing.T) {
	const addr = "ada@example.com"
	got, err := Info(chartYAML("version: 1.2.3\nmaintainers:\n  - name: Ada\n    email: "+addr+"\n    url: https://example.com/ada\n"), "demo")
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), addr) || strings.Contains(strings.ToLower(string(b)), "email") {
		t.Errorf("marshalled status carries the maintainer's email: %s", b)
	}
	if want := `{"version":"1.2.3","maintainers":[{"name":"Ada","url":"https://example.com/ada"}]}`; string(b) != want {
		t.Errorf("marshalled status = %s, want %s", b, want)
	}
}

func TestInfoErrors(t *testing.T) {
	if _, err := Info(chartYAML("maintainers:\n  - name: Ada\n"), "demo"); err == nil {
		t.Error("a Chart.yaml with no version was accepted")
	}
	if _, err := Info(chartYAML("version: [\n"), "demo"); err == nil {
		t.Error("an unparseable Chart.yaml was accepted")
	}
	if _, err := Info(fstest.MapFS{}, "demo"); err == nil {
		t.Error("a missing Chart.yaml was accepted")
	}
}

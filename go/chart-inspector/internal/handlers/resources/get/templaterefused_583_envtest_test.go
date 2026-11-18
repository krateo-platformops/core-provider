//go:build envtest

// core-provider#583 through the REAL chain against a REAL apiserver. A chart whose template calls
// `fail` must be answered 400 with Status reason TemplateRefused. Before #583 it was a 500, and the
// cdc reported "chart-inspector itself failed; not the blueprint's fault".
//
// WHY ENVTEST. helm's install checks reachability BEFORE it renders (action/install.go:235-238), so
// the closed-port fixture the #325 tests use answers connection refused and never reaches a template.
//
//	Run: KUBEBUILDER_ASSETS=$(setup-envtest use 1.36.0 -p path) \
//	go test -tags envtest ./internal/handlers/resources/get/ -run Envtest_TemplateRefused -v
package resources

import (
	"net/http"
	"os"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestEnvtest_TemplateRefused(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Fatal("KUBEBUILDER_ASSETS is not set: this test exists to run against a real apiserver, and a skip would read as a pass")
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	refusing := chartArchive(t,
		chartFile{"refusing/Chart.yaml", chartYAML("refusing")},
		chartFile{"refusing/values.yaml", "replicas: 1\n"},
		chartFile{"refusing/templates/cm.yaml", `{{ fail "kinds omits Repository, which this component requires" }}` + "\n"},
	)
	rec := doRequest(t, realChainFixtureAt(t, cfg, serveChart(t, refusing), map[string]any{}))
	b := decodeStatus(t, rec)
	t.Logf("status=%d reason=%q message=%s", rec.Code, b.Reason, b.Message)
	if !strings.Contains(b.Message, "kinds omits Repository") {
		t.Fatalf("VACUOUS: the response did not come from the chart's own fail. status=%d message=%s", rec.Code, b.Message)
	}
	if rec.Code != http.StatusBadRequest || b.Reason != string(ReasonTemplateRefused) {
		t.Errorf("status=%d reason=%q, want 400 %q", rec.Code, b.Reason, ReasonTemplateRefused)
	}

	// CONTROL: the same chain against the same apiserver with a chart that renders. It must NOT be
	// TemplateRefused, so the assertion above is about the template, not about this setup.
	ok := chartArchive(t,
		chartFile{"ok/Chart.yaml", chartYAML("ok")},
		chartFile{"ok/values.yaml", "replicas: 1\n"},
		chartFile{"ok/templates/cm.yaml", configMapTemplate},
	)
	rec = doRequest(t, realChainFixtureAt(t, cfg, serveChart(t, ok), map[string]any{}))
	t.Logf("control: status=%d body=%.120s", rec.Code, rec.Body.String())
	if rec.Code != http.StatusOK || !strings.HasPrefix(strings.TrimSpace(rec.Body.String()), "[") {
		t.Errorf("a chart that renders was not answered 200 with its resources: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

package compositiondefinitions

import (
	"regexp"
	"strings"
	"testing"

	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
)

// otelLogsLines renders helm/core-provider with extra values and returns, for core-provider's own
// ConfigMap and for the cdc ConfigMap asset, every OTEL_LOGS_ENABLED key line. Lines, not a parsed
// map: the cdc asset is itself a template (it still holds {{ .resource }} placeholders, so it is not
// YAML yet), and a parse that keeps the last of two equal keys would hide a duplicate.
func otelLogsLines(t *testing.T, extra map[string]any) (core, cdc []string) {
	t.Helper()
	chrt, err := loadChartForTest(chartDir)
	if err != nil {
		t.Fatal(err)
	}
	rv, err := chartutil.ToRenderValues(chrt, extra, chartutil.ReleaseOptions{Name: "core-provider", Namespace: "krateo-system", IsInstall: true}, chartutil.DefaultCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := engine.Render(chrt, rv)
	if err != nil {
		t.Fatal(err)
	}
	key := regexp.MustCompile(`(?m)^\s*OTEL_LOGS_ENABLED:.*$`)
	found := 0
	for name, body := range rendered {
		switch {
		case strings.HasSuffix(name, "templates/configmap.yaml"):
			core = trimAll(key.FindAllString(body, -1))
			found++
		case strings.HasSuffix(name, "templates/cdc-configmap.yaml"):
			cdc = trimAll(key.FindAllString(body, -1))
			found++
		}
	}
	if found != 2 {
		t.Fatalf("found %d of the 2 ConfigMap templates in the render", found)
	}
	return core, cdc
}

func trimAll(in []string) []string {
	for i := range in {
		in[i] = strings.TrimSpace(in[i])
	}
	return in
}

// #469: OTEL_LOGS_ENABLED had no chart value, so it reached each process only through its own free
// env map, and on krateo-057 core-provider's was absent while every cdc's was true: core-provider's
// audit records would have been dropped. otel.logs now governs both, and an explicit env key wins
// for its process with exactly ONE key rendered.
func TestOtelLogsGovernsBothProcesses(t *testing.T) {
	want := func(t *testing.T, what string, got []string, line string) {
		t.Helper()
		if line == "" {
			if len(got) != 0 {
				t.Errorf("%s: OTEL_LOGS_ENABLED rendered %v, want absent", what, got)
			}
			return
		}
		if len(got) != 1 || got[0] != line {
			t.Errorf("%s: OTEL_LOGS_ENABLED lines %v, want exactly [%s]", what, got, line)
		}
	}
	t.Run("otel.logs true reaches both", func(t *testing.T) {
		core, cdc := otelLogsLines(t, map[string]any{"otel": map[string]any{"enabled": true, "logs": true}})
		want(t, "core-provider", core, `OTEL_LOGS_ENABLED: "true"`)
		want(t, "cdc", cdc, `OTEL_LOGS_ENABLED: "true"`)
	})
	t.Run("default: rendered false in both", func(t *testing.T) {
		core, cdc := otelLogsLines(t, map[string]any{"otel": map[string]any{"enabled": true}})
		want(t, "core-provider", core, `OTEL_LOGS_ENABLED: "false"`)
		want(t, "cdc", cdc, `OTEL_LOGS_ENABLED: "false"`)
	})
	t.Run("otel off: neither (the byte-identical promise)", func(t *testing.T) {
		core, cdc := otelLogsLines(t, map[string]any{"otel": map[string]any{"enabled": false, "logs": true}})
		want(t, "core-provider", core, "")
		want(t, "cdc", cdc, "")
	})
	t.Run("an explicit env key wins, rendered once", func(t *testing.T) {
		// The krateo-057 shape: cdc.env sets it, env does not; otel.logs false.
		core, cdc := otelLogsLines(t, map[string]any{
			"otel": map[string]any{"enabled": true, "logs": false},
			"env":  map[string]any{"CORE_PROVIDER_DEBUG": "false", "OTEL_LOGS_ENABLED": "true"},
			"cdc":  map[string]any{"env": map[string]any{"HOME": "/tmp", "OTEL_LOGS_ENABLED": "true"}},
		})
		want(t, "core-provider", core, `OTEL_LOGS_ENABLED: "true"`)
		want(t, "cdc", cdc, `OTEL_LOGS_ENABLED: "true"`)
	})
}

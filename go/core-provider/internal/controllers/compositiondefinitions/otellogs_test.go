package compositiondefinitions

import (
	"regexp"
	"strings"
	"testing"

	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	"sigs.k8s.io/yaml"
)

// otelLogsLines renders helm/core-provider with extra values and returns, for core-provider's own
// ConfigMap and for the cdc ConfigMap asset, every OTEL_LOGS_ENABLED key line. Lines, not a parsed
// map: the cdc asset is itself a template (it still holds {{ .resource }} placeholders, so it is not
// YAML yet), and a parse that keeps the last of two equal keys would hide a duplicate.
func otelLogsLines(t *testing.T, extra map[string]any) (core, cdc []string) {
	t.Helper()
	return otelKeyLines(t, extra, "OTEL_LOGS_ENABLED")
}

// otelKeyLines is otelLogsLines for any ConfigMap key.
func otelKeyLines(t *testing.T, extra map[string]any, envKey string) (core, cdc []string) {
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
	key := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(envKey) + `:.*$`)
	found := 0
	for name, body := range rendered {
		switch {
		case strings.HasSuffix(name, "templates/configmap.yaml"):
			core = trimAll(key.FindAllString(body, -1))
			found++
		case strings.HasSuffix(name, "templates/cdc-configmap.yaml"):
			// This ConfigMap packages one env per PROCESS: configmap.yaml (a dedicated controller's)
			// and, since pool step 5, pool-configmap.yaml (a pool's). Each is checked on its own: one
			// key per process env is the rule (#469), and the two processes each have their own.
			var cm struct {
				Data map[string]string `json:"data"`
			}
			if err := yaml.Unmarshal([]byte(body), &cm); err != nil {
				t.Fatalf("decoding %s: %v", name, err)
			}
			dedicated, ok := cm.Data["configmap.yaml"]
			if !ok {
				t.Fatalf("%s carries no configmap.yaml (keys: %v)", name, keysOf(cm.Data))
			}
			cdc = trimAll(key.FindAllString(dedicated, -1))
			if pool, ok := cm.Data["pool-configmap.yaml"]; ok {
				// The pool's process env comes from the same helm templates
				// (core-provider.cdcProcessEnvHead/Tail), so it must carry the same single line.
				if got := trimAll(key.FindAllString(pool, -1)); strings.Join(got, "|") != strings.Join(cdc, "|") {
					t.Errorf("%s: the pool process env has %v, the dedicated one %v; they come from one template and must agree", envKey, got, cdc)
				}
			}
			found++
		}
	}
	if found != 2 {
		t.Fatalf("found %d of the 2 ConfigMap templates in the render", found)
	}
	return core, cdc
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
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

// The audit-only switch (Diego, 2026-11-14): otel.auditLogs reaches core-provider AND every cdc as
// OTEL_AUDIT_LOGS_ENABLED, independently of otel.logs, and an explicit env / cdc.env key wins for its
// process with exactly ONE key rendered, the same override path as OTEL_LOGS_ENABLED (#469).
func TestOtelAuditLogsGovernsBothProcesses(t *testing.T) {
	const k = "OTEL_AUDIT_LOGS_ENABLED"
	want := func(t *testing.T, what string, got []string, line string) {
		t.Helper()
		if line == "" {
			if len(got) != 0 {
				t.Errorf("%s: %s rendered %v, want absent", what, k, got)
			}
			return
		}
		if len(got) != 1 || got[0] != line {
			t.Errorf("%s: %s lines %v, want exactly [%s]", what, k, got, line)
		}
	}
	t.Run("otel.auditLogs true reaches both, with otel.logs off", func(t *testing.T) {
		core, cdc := otelKeyLines(t, map[string]any{"otel": map[string]any{"enabled": true, "logs": false, "auditLogs": true}}, k)
		want(t, "core-provider", core, k+`: "true"`)
		want(t, "cdc", cdc, k+`: "true"`)
		lcore, lcdc := otelKeyLines(t, map[string]any{"otel": map[string]any{"enabled": true, "logs": false, "auditLogs": true}}, "OTEL_LOGS_ENABLED")
		want(t, "core-provider (OTEL_LOGS_ENABLED unaffected)", trimKey(lcore, "OTEL_LOGS_ENABLED"), "")
		want(t, "cdc (OTEL_LOGS_ENABLED unaffected)", trimKey(lcdc, "OTEL_LOGS_ENABLED"), "")
	})
	t.Run("default: rendered false in both", func(t *testing.T) {
		core, cdc := otelKeyLines(t, map[string]any{"otel": map[string]any{"enabled": true}}, k)
		want(t, "core-provider", core, k+`: "false"`)
		want(t, "cdc", cdc, k+`: "false"`)
	})
	t.Run("otel off: neither", func(t *testing.T) {
		core, cdc := otelKeyLines(t, map[string]any{"otel": map[string]any{"enabled": false, "auditLogs": true}}, k)
		want(t, "core-provider", core, "")
		want(t, "cdc", cdc, "")
	})
	t.Run("an explicit env key wins, rendered once", func(t *testing.T) {
		core, cdc := otelKeyLines(t, map[string]any{
			"otel": map[string]any{"enabled": true, "auditLogs": false},
			"env":  map[string]any{k: "true"},
			"cdc":  map[string]any{"env": map[string]any{k: "true"}},
		}, k)
		want(t, "core-provider", core, k+`: "true"`)
		want(t, "cdc", cdc, k+`: "true"`)
	})
}

// trimKey keeps the lines that are NOT a "false" rendering of key, so an arm can assert "this value
// did not turn the other switch on" without depending on whether otel.logs renders false or nothing.
func trimKey(lines []string, key string) []string {
	var out []string
	for _, l := range lines {
		if l != key+`: "false"` {
			out = append(out, l)
		}
	}
	return out
}

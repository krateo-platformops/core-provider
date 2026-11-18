package deploy

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// chartInspectorDeployment renders the shipped chart with overrides and returns chart-inspector's
// Deployment, or the chart's refusal.
func chartInspectorDeployment(t *testing.T, overrides map[string]any) (appsv1.Deployment, error) {
	t.Helper()
	m, err := renderShippedChartE(t, overrides)
	if err != nil {
		return appsv1.Deployment{}, err
	}
	for path, manifest := range m {
		if strings.HasSuffix(path, "chart-inspector/deployment.yaml") {
			var dep appsv1.Deployment
			if err := yaml.Unmarshal([]byte(manifest), &dep); err != nil {
				t.Fatal(err)
			}
			return dep, nil
		}
	}
	t.Fatal("no chart-inspector Deployment rendered")
	return appsv1.Deployment{}, nil
}

func envValue(c corev1.Container, name string) (string, bool) {
	for _, e := range c.Env {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}

// #582. chart-inspector's termination budget covers the preStop sleep THEN the drain of in-flight
// renders, and chart-inspector is told the drain part (SHUTDOWN_TIMEOUT, a contract with its
// main.go, which pins the same literal), ending 5 s before the kubelet's SIGKILL. Before, the pod
// had the 30 s default: the 10 s sleep left ~20 s, and a ~1 min umbrella render was killed
// mid-response on every roll.
func TestShippedChartInspector_TerminationBudget(t *testing.T) {
	for _, tc := range []struct {
		name         string
		overrides    map[string]any
		wantGrace    int64
		wantShutdown string
	}{
		{"defaults: 90 s, of which 75 s to drain", nil, 90, "75s"},
		{"a longer budget moves the drain with it", map[string]any{"chartInspector": map[string]any{"terminationGracePeriodSeconds": 120}}, 120, "105s"},
		{"no preStop sleep: the drain gets all but 5 s", map[string]any{"chartInspector": map[string]any{"preStopSleepSeconds": 0}}, 90, "85s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dep, err := chartInspectorDeployment(t, tc.overrides)
			if err != nil {
				t.Fatal(err)
			}
			g := dep.Spec.Template.Spec.TerminationGracePeriodSeconds
			if g == nil || *g != tc.wantGrace {
				t.Fatalf("terminationGracePeriodSeconds = %v, want %d", g, tc.wantGrace)
			}
			c := dep.Spec.Template.Spec.Containers[0]
			got, ok := envValue(c, "SHUTDOWN_TIMEOUT")
			if !ok || got != tc.wantShutdown {
				t.Errorf("SHUTDOWN_TIMEOUT = %q (set %v), want %q", got, ok, tc.wantShutdown)
			}
			// The property itself, not just the numbers: sleep + drain fits inside the budget.
			var sleep int64
			if l := c.Lifecycle; l != nil && l.PreStop != nil && l.PreStop.Sleep != nil {
				sleep = l.PreStop.Sleep.Seconds
			}
			drain := strings.TrimSuffix(got, "s")
			var d int64
			for _, r := range drain {
				d = d*10 + int64(r-'0')
			}
			if sleep+d >= *g {
				t.Errorf("sleep %ds + drain %ds does not fit the %ds budget", sleep, d, *g)
			}
		})
	}

	t.Run("a budget that leaves no drain is refused by the chart", func(t *testing.T) {
		_, err := chartInspectorDeployment(t, map[string]any{"chartInspector": map[string]any{"terminationGracePeriodSeconds": 15}})
		if err == nil || !strings.Contains(err.Error(), "must exceed preStopSleepSeconds") {
			t.Errorf("grace 15 with sleep 10: err = %v, want the chart's refusal", err)
		}
	})
}

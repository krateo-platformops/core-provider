package deploy

import (
	"sort"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// #570: every object the shipped chart renders carries the release's selector label, so
// `kubectl get <kind> -l app.kubernetes.io/instance=<release>` finds all of them. helm tracks its
// objects through the release manifest, so a missing label fails no install and no test; it only
// shows when something SELECTS by it. Measured before the fix: 6 of 10 cluster-scoped objects (the
// main ClusterRole/Binding, both admission policies and their bindings) and 3 ConfigMaps.
//
// Rendered twice: with the defaults, and with every optional template switched on, so a template
// behind a toggle is checked too. Each object's identity is printed with the count of labelled
// ones, which is the control: a render that produced nothing cannot pass.
func TestShippedChart_EveryObjectCarriesTheReleaseLabel(t *testing.T) {
	everythingOn := map[string]any{
		"leaderElection":      map[string]any{"enabled": true},
		"autoscaling":         map[string]any{"enabled": true, "minReplicas": 2, "targetCPUUtilizationPercentage": 80, "targetMemoryUtilizationPercentage": 80},
		"podDisruptionBudget": map[string]any{"enabled": true},
		"replicaCount":        2,
		"apiRefRBAC":          map[string]any{"enabled": true},
		"otel":                map[string]any{"enabled": true},
		"serviceAccount":      map[string]any{"create": true},
		"cdc":                 map[string]any{"metrics": map[string]any{"enabled": true}},
		"chartInspector": map[string]any{
			"autoscaling":         map[string]any{"enabled": true, "minReplicas": 2},
			"podDisruptionBudget": map[string]any{"enabled": true},
			"replicaCount":        2,
		},
	}
	for _, tc := range []struct {
		name   string
		values map[string]any
		// mustRender: kinds the toggles exist to produce, with their count; the control that the
		// "on" render really switched them on rather than passing on the default set.
		mustRender map[string]int
	}{
		{"defaults", nil, nil},
		{"every optional template on", everythingOn, map[string]int{"HorizontalPodAutoscaler": 2, "PodDisruptionBudget": 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var labelled, missing []string
			kinds := map[string]int{}
			for path, manifest := range renderShippedChart(t, tc.values) {
				for _, doc := range strings.Split(manifest, "\n---") {
					var obj map[string]any
					if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
						t.Fatalf("%s: %v", path, err)
					}
					if len(obj) == 0 {
						continue
					}
					md, _ := obj["metadata"].(map[string]any)
					kinds[obj["kind"].(string)]++
					id := path + ": " + obj["kind"].(string) + "/" + md["name"].(string)
					labels, _ := md["labels"].(map[string]any)
					if labels["app.kubernetes.io/instance"] == "core-provider" {
						labelled = append(labelled, id)
					} else {
						missing = append(missing, id)
					}
				}
			}
			sort.Strings(missing)
			t.Logf("%d objects labelled, %d not; kinds %v", len(labelled), len(missing), kinds)
			for k, n := range tc.mustRender {
				if kinds[k] != n {
					t.Errorf("control: the render has %d %s, want %d, so a toggle did not take", kinds[k], k, n)
				}
			}
			if len(labelled) == 0 {
				t.Fatal("control: no labelled object rendered, so this check measured nothing")
			}
			for _, m := range missing {
				t.Errorf("no app.kubernetes.io/instance=core-provider: %s", m)
			}
		})
	}
}

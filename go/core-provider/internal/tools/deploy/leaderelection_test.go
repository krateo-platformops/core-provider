package deploy

import (
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

// #382. A rolling update runs the old and the new core-provider pod side by side, even at
// replicaCount 1. Without a Lease both reconcile, and during an upgrade they disagree, so each
// restarts every cdc after the other. The DEFAULT install must therefore elect a leader, and have
// the RBAC to do it: the env alone without the Lease grant would fail to start.
func TestShippedChart_ElectsALeaderByDefault(t *testing.T) {
	cases := []struct {
		name      string
		overrides map[string]any
		want      bool
	}{
		{"default values", nil, true},
		// The control: proves the assertions read the toggle rather than something always rendered.
		{"explicitly off", map[string]any{"leaderElection": map[string]any{"enabled": false}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := renderShippedChart(t, tc.overrides)

			var cm corev1.ConfigMap
			if err := yaml.Unmarshal([]byte(m["core-provider/templates/configmap.yaml"]), &cm); err != nil || cm.Name == "" {
				t.Fatalf("decoding the env ConfigMap: %v (name %q)", err, cm.Name)
			}
			if got := cm.Data["CORE_PROVIDER_LEADER_ELECTION"] == "true"; got != tc.want {
				t.Errorf("CORE_PROVIDER_LEADER_ELECTION=true rendered: %v, want %v (data: %v)", got, tc.want, cm.Data)
			}

			var cr rbacv1.ClusterRole
			if err := yaml.Unmarshal([]byte(m["core-provider/templates/clusterrole.yaml"]), &cr); err != nil || len(cr.Rules) == 0 {
				t.Fatalf("decoding the ClusterRole: %v (%d rules)", err, len(cr.Rules))
			}
			canLease := false
			for _, r := range cr.Rules {
				if slices.Contains(r.APIGroups, "coordination.k8s.io") && slices.Contains(r.Resources, "leases") &&
					slices.Contains(r.Verbs, "get") && slices.Contains(r.Verbs, "create") && slices.Contains(r.Verbs, "update") {
					canLease = true
				}
			}
			if canLease != tc.want {
				t.Errorf("Lease get/create/update granted: %v, want %v", canLease, tc.want)
			}
		})
	}
}

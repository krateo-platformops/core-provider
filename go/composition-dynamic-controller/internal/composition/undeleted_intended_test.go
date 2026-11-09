package composition

import (
	"testing"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
)

// The case #346 added intendedDeletions for (#304): the render dropped objects and the merge did not
// delete them, detected independently of Changed. Since plumbing v1.35.0 a KEPT deletion
// (helm.sh/resource-policy: keep) is reported apart and is correct, because it re-records the
// release, so it is not counted as a failure. What is left is a deletion that FAILED.
func TestFailedIntendedDeletions(t *testing.T) {
	for name, tc := range map[string]struct {
		res  *helmconfig.ReconcileResult
		want int
	}{
		"nil result":                              {nil, 0},
		"nothing intended":                        {&helmconfig.ReconcileResult{}, 0},
		"intended and deleted":                    {&helmconfig.ReconcileResult{IntendedDeletions: []string{"a", "b"}, Deleted: 2, Changed: true}, 0},
		"intended, NOT deleted, Changed == false": {&helmconfig.ReconcileResult{IntendedDeletions: []string{"Agent/k8s-agent", "Agent/helm-agent", "RemoteMCPServer/clickhouse-mcp-server"}}, 3},
		"partly deleted":                          {&helmconfig.ReconcileResult{IntendedDeletions: []string{"a", "b"}, Deleted: 1}, 1},
		"the omitted tail counts":                 {&helmconfig.ReconcileResult{IntendedDeletions: []string{"a"}, IntendedDeletionsOmitted: 4, Deleted: 1}, 4},
		"all kept (krateo-autopilot): no failure": {&helmconfig.ReconcileResult{IntendedDeletions: []string{"Agent/k8s-agent", "Agent/helm-agent", "RemoteMCPServer/clickhouse-mcp-server"}, KeptDeletions: []string{"Agent/k8s-agent", "Agent/helm-agent", "RemoteMCPServer/clickhouse-mcp-server"}, Changed: true}, 0},
		"one kept, one failed":                    {&helmconfig.ReconcileResult{IntendedDeletions: []string{"a", "b"}, KeptDeletions: []string{"a"}}, 1},
		"the kept omitted tail counts as kept":    {&helmconfig.ReconcileResult{IntendedDeletions: []string{"a"}, IntendedDeletionsOmitted: 2, KeptDeletions: []string{"a"}, KeptDeletionsOmitted: 2}, 0},
	} {
		if got := failedIntendedDeletions(tc.res); got != tc.want {
			t.Errorf("%s: failedIntendedDeletions = %d, want %d", name, got, tc.want)
		}
	}
}

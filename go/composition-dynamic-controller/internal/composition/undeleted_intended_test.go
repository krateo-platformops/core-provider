package composition

import (
	"testing"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
)

// The case #346 added intendedDeletions for (#304): the render dropped objects and the merge did not
// delete them. It reads Changed == false, so it must be detected independently of Changed.
func TestUndeletedIntendedDeletions(t *testing.T) {
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
	} {
		if got := undeletedIntendedDeletions(tc.res); got != tc.want {
			t.Errorf("%s: undeletedIntendedDeletions = %d, want %d", name, got, tc.want)
		}
	}
}

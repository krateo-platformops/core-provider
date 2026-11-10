package composition

import (
	"testing"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
)

func fieldsMap(t *testing.T, kv []any) map[string]any {
	t.Helper()
	if len(kv)%2 != 0 {
		t.Fatalf("odd number of key/values (%d): the logger would pair them wrongly", len(kv))
	}
	m := map[string]any{}
	for i := 0; i < len(kv); i += 2 {
		k, ok := kv[i].(string)
		if !ok {
			t.Fatalf("key %d is %T, not a string", i, kv[i])
		}
		if _, dup := m[k]; dup {
			t.Fatalf("duplicate key %q", k)
		}
		m[k] = kv[i+1]
	}
	return m
}

// plumbing#112 / #439. Both drift lines carry which FIELD of a patched child changed, not just which
// child. The Update line used to carry only the three counts.
func TestReconcileDiffFields(t *testing.T) {
	child := "apps/v1, Kind=Deployment demo-system/frontend-agent"
	res := &helmconfig.ReconcileResult{
		Changed: true, PatchedUpdated: 1,
		ChangedObjects:      []string{child},
		ChangedPaths:        map[string][]string{child: {"metadata.annotations[checksum/config]", "spec.replicas"}},
		ChangedPathsOmitted: map[string]int{child: 2},
		IntendedDeletions:   []string{"v1, Kind=ConfigMap demo-system/gone"},
	}
	m := fieldsMap(t, reconcileDiffFields(res))

	paths, ok := m["changedPaths"].(map[string][]string)
	if !ok || len(paths[child]) != 2 || paths[child][1] != "spec.replicas" {
		t.Errorf("changedPaths = %#v, want the child's two paths", m["changedPaths"])
	}
	if om, ok := m["changedPathsOmitted"].(map[string]int); !ok || om[child] != 2 {
		t.Errorf("changedPathsOmitted = %#v, want 2 for the child", m["changedPathsOmitted"])
	}
	// The pre-existing fields keep their names: dashboards and runbooks match on them.
	for _, k := range []string{"created", "deleted", "patched", "intendedDeletions", "intendedDeletionsCount",
		"intendedDeletionsOmitted", "deletionsSkipped", "changedObjects", "changedObjectsOmitted"} {
		if _, ok := m[k]; !ok {
			t.Errorf("field %q missing", k)
		}
	}
	if m["deletionsSkipped"] != true {
		t.Errorf("deletionsSkipped = %v, want true (intended 1, deleted 0)", m["deletionsSkipped"])
	}
	if got := reconcileDiffFields(nil); got != nil {
		t.Errorf("nil result: %v, want no fields", got)
	}
}

package composition

import (
	"errors"
	"strings"
	"testing"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
	xcontext "github.com/krateo-platformops/unstructured-runtime/pkg/context"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// core-provider#640: what reportFailedReleaseHeal says for each shape of plumbing's FailedReleaseHeal.
// The envtest arms (failedreleaseheal_*_envtest_test.go) drive the real heal; these pin every branch.
func TestReportFailedReleaseHeal(t *testing.T) {
	const (
		healed   = "Composition's failed release healed against what its failed revision applied"
		noRecord = "Composition's failed release cannot be healed: its failed revision carries no record of what was applied " +
			"(written before plumbing v1.110.0, or by helm alone), so drift on its objects stays until an attempt gets past its objects step"
		failed = "Composition's failed release could not be healed"
	)
	ctx, lines := logCapture(t)
	log := xcontext.Logger(ctx)
	mg := func(uid string) *unstructured.Unstructured {
		u := &unstructured.Unstructured{}
		u.SetUID(types.UID(uid))
		return u
	}
	report := func(uid string, res *helmconfig.ReconcileResult) []string {
		*lines = (*lines)[:0]
		reportFailedReleaseHeal(mg(uid), res, "rel", log)
		return append([]string(nil), *lines...)
	}
	retry := func(h helmconfig.FailedReleaseHeal) *helmconfig.ReconcileResult {
		return &helmconfig.ReconcileResult{RetriedFailedRelease: true, FailedReleaseHeal: &h}
	}
	one := func(t *testing.T, got []string, msg string, fields ...string) {
		t.Helper()
		if len(got) != 1 || !strings.HasPrefix(got[0], msg) {
			t.Fatalf("lines %q, want one %q", got, msg)
		}
		for _, f := range fields {
			if !strings.Contains(got[0], f) {
				t.Errorf("line %q lacks %q", got[0], f)
			}
		}
	}
	none := func(t *testing.T, got []string) {
		t.Helper()
		if len(got) != 0 {
			t.Errorf("lines %q, want none", got)
		}
	}
	t.Cleanup(func() {
		failedReleaseNoRecordSaid.Range(func(k, _ any) bool { failedReleaseNoRecordSaid.Delete(k); return true })
	})

	t.Run("no result, or no heal: silent", func(t *testing.T) {
		none(t, report("a", nil))
		none(t, report("a", &helmconfig.ReconcileResult{Changed: true, Created: 1}))
	})
	t.Run("recorded, with writes: the heal's own counts and names, not the top-level ones", func(t *testing.T) {
		res := retry(helmconfig.FailedReleaseHeal{Revision: 2, Recorded: true, Created: 1, PatchedUpdated: 1,
			ChangedObjects: []string{"v1/ConfigMap/ns/app", "v1/ConfigMap/ns/b"}, ChangedObjectsOmitted: 3,
			ChangedPaths: map[string][]string{"v1/ConfigMap/ns/b": {"data.k"}}})
		res.Created, res.PatchedUpdated = 5, 7
		one(t, report("a", res), healed, "release=rel", "revision=2", "recorded=true", "created=1", "patched=1",
			"changedObjects=[v1/ConfigMap/ns/app v1/ConfigMap/ns/b]", "changedObjectsOmitted=3", "data.k")
	})
	t.Run("recorded, only a create, or only a patch: said", func(t *testing.T) {
		one(t, report("a", retry(helmconfig.FailedReleaseHeal{Revision: 2, Recorded: true, Created: 1})), healed, "created=1")
		one(t, report("a", retry(helmconfig.FailedReleaseHeal{Revision: 2, Recorded: true, PatchedUpdated: 1})), healed, "patched=1")
	})
	t.Run("recorded, nothing to write: silent", func(t *testing.T) {
		none(t, report("a", retry(helmconfig.FailedReleaseHeal{Revision: 2, Recorded: true})))
	})
	t.Run("recorded, some children failed: said with the error", func(t *testing.T) {
		one(t, report("a", retry(helmconfig.FailedReleaseHeal{Revision: 2, Recorded: true, Err: errors.New("patch b: denied")})), healed, "error=patch b: denied")
	})
	t.Run("the record could not be read: every pass, with the error", func(t *testing.T) {
		res := retry(helmconfig.FailedReleaseHeal{Revision: 2, Err: errors.New("reading revision 2: boom")})
		one(t, report("e", res), failed, "revision=2", "error=reading revision 2: boom")
		one(t, report("e", res), failed)
	})
	t.Run("no record: once per composition while it stays unrecorded, however many revisions", func(t *testing.T) {
		one(t, report("n", retry(helmconfig.FailedReleaseHeal{Revision: 2})), noRecord, "release=rel", "revision=2", "recorded=false")
		none(t, report("n", retry(helmconfig.FailedReleaseHeal{Revision: 2})))
		none(t, report("n", retry(helmconfig.FailedReleaseHeal{Revision: 3})))
		// Another composition is its own.
		one(t, report("m", retry(helmconfig.FailedReleaseHeal{Revision: 3})), noRecord, "revision=3")
		// A Reconcile that failed before giving a result says nothing about the state: still said.
		none(t, report("n", nil))
		none(t, report("n", retry(helmconfig.FailedReleaseHeal{Revision: 4})))
	})
	t.Run("no record again after a recorded heal, or after a reconcile that is no retry", func(t *testing.T) {
		one(t, report("r", retry(helmconfig.FailedReleaseHeal{Revision: 2})), noRecord)
		none(t, report("r", retry(helmconfig.FailedReleaseHeal{Revision: 3, Recorded: true})))
		one(t, report("r", retry(helmconfig.FailedReleaseHeal{Revision: 4})), noRecord, "revision=4")
		none(t, report("r", &helmconfig.ReconcileResult{}))
		one(t, report("r", retry(helmconfig.FailedReleaseHeal{Revision: 5})), noRecord, "revision=5")
	})
}

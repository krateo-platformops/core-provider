package composition

import helmconfig "github.com/krateo-platformops/plumbing/helm"

// reconcileDiffFields is what a reconcile that changed something logs about the change, shared by
// both lines that report one: Observe's "Composition drift detected and self-healed" and Update's
// "Update wrote a helm revision". They used to list their own fields, and the Update line carried
// only the three counts, so the same drift was nameable in one phase and anonymous in the other.
//
// changedPaths (plumbing#112, v1.38.0) answers the question the rest of the line could not: WHICH
// field of a patched child differed. alert-provider's FrontendAgent self-healed after a
// metadata-only patch and nothing could say what the heal had changed. A managedFields snapshot
// taken afterwards cannot either, because the heal takes ownership. Paths only, never values: a
// value may be Secret data. Capped at five paths per child, with changedPathsOmitted counting the
// rest per child. A patched child WITHOUT an entry means its paths could not be computed (the merge
// still counted it as changed, failing safe), not that nothing differed. Created and deleted
// children never have an entry, because the whole object is the change.
//
// intendedDeletions sits BESIDE deleted, and the pair is the point (#304). A bare `deleted:0`
// cannot tell "nothing was removed from the render" from "the merge was asked to remove something
// and did not":
//
//	intended 0, deleted 0   nothing to remove, OR the removal was already lost upstream
//	                        (the stored manifest no longer lists the child, so helm's
//	                        original.Difference(target) is legitimately empty)
//	intended >0, deleted 0  helm was asked and did not do it (deletionsSkipped). Its deletion
//	                        loop sits AFTER both error returns in update() (pkg/kube/client.go),
//	                        so ONE failed patch skips every deletion, and updateResource appends
//	                        to res.Updated "regardless" of failure, so a non-zero patched count
//	                        does not rule this out.
//
// Every list here is capped in plumbing, so the line stays bounded however large the drift.
func reconcileDiffFields(res *helmconfig.ReconcileResult) []any {
	if res == nil {
		return nil
	}
	return []any{
		"created", res.Created, "deleted", res.Deleted, "patched", res.PatchedUpdated,
		"intendedDeletions", res.IntendedDeletions,
		"intendedDeletionsCount", len(res.IntendedDeletions),
		"intendedDeletionsOmitted", res.IntendedDeletionsOmitted,
		"deletionsSkipped", len(res.IntendedDeletions) > 0 && res.Deleted == 0,
		"changedObjects", res.ChangedObjects, "changedObjectsOmitted", res.ChangedObjectsOmitted,
		"changedPaths", res.ChangedPaths, "changedPathsOmitted", res.ChangedPathsOmitted,
	}
}

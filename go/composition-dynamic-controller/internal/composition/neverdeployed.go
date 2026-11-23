package composition

import (
	"context"
	"encoding/json"
	"fmt"

	compositionMeta "github.com/krateo-platformops/composition-dynamic-controller/pkg/meta"
	helmconfig "github.com/krateo-platformops/plumbing/helm"
	"github.com/krateo-platformops/unstructured-runtime/pkg/logging"
	"github.com/krateo-platformops/unstructured-runtime/pkg/tools"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// core-provider#636: the one proof that no revision of a release was ever deployed, used by both
// recoveries that act on it: #629 (an interrupted install is UNINSTALLED and installed again,
// freshreinstall.go) and #636 (a failed install is installed again with Replace, failedinstall.go).
//
// POSITIVE evidence only. "No deployed revision in the history" is not evidence: helm keeps
// helmMaxHistory revisions and never prunes the last DEPLOYED one, but a failed rollback marks the
// once-live revision superseded (helm v3.20.2 rollback.go:197), and failed retries after it prune that
// one like any other. Such a release was live, and its stored history holds only failed revisions.
//
// So the history must account for EVERY revision, from the first one, and each must have failed (the
// latest may be the interrupted pending-install). Revision 1 present and failed, with no gap above it,
// is the whole proof. Once a retried install has been pruned out of the window, the mark
// krateo.io/release-never-deployed stands in for the revisions it names: it is written while the proof
// held, before the helm call that can prune them, so what it says was true. The history must still
// account for every revision AFTER the ones it names. A mark that outlives the release it describes
// (its removal failed) therefore proves nothing: the revision that deployed is above it, and is either
// in the history as deployed or superseded, or missing from it.

// neverDeployedMark is the value of krateo.io/release-never-deployed.
type neverDeployedMark struct {
	// UID is the composition the mark was written on: a copied annotation (a restore, a GitOps export)
	// on another object is not evidence about its release.
	UID string `json:"uid"`
	// Release is the helm release it describes.
	Release string `json:"release"`
	// ThroughRevision: every revision up to and including this one was never deployed.
	ThroughRevision int `json:"throughRevision"`
}

// readNeverDeployedMark returns the highest revision the composition's mark vouches for in releaseName,
// or 0 and why when there is no mark that does. A mark that cannot be read vouches for nothing.
func readNeverDeployedMark(mg *unstructured.Unstructured, releaseName string) (int, string) {
	raw, ok := mg.GetAnnotations()[compositionMeta.AnnotationKeyReleaseNeverDeployed]
	if !ok {
		return 0, "no " + compositionMeta.AnnotationKeyReleaseNeverDeployed + " mark"
	}
	var m neverDeployedMark
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return 0, fmt.Sprintf("annotation %s=%q cannot be read", compositionMeta.AnnotationKeyReleaseNeverDeployed, raw)
	}
	if m.UID != string(mg.GetUID()) || m.Release != releaseName {
		return 0, fmt.Sprintf("annotation %s describes another composition or release", compositionMeta.AnnotationKeyReleaseNeverDeployed)
	}
	if m.ThroughRevision < 1 {
		return 0, fmt.Sprintf("annotation %s=%q names no revision", compositionMeta.AnnotationKeyReleaseNeverDeployed, raw)
	}
	return m.ThroughRevision, ""
}

// neverDeployed says whether no revision of the release was ever deployed, from latest (the revision
// this pass read) and the stored history, and says why not. latest must be the failed or the
// interrupted (pending-install) attempt; every other stored revision must have failed; and the history,
// with the mark, must account for every revision. Anything else, including an entry it cannot read, is
// "not proven", the side that keeps the release.
func neverDeployed(mg *unstructured.Unstructured, releaseName string, latest *helmconfig.Release, history []*helmconfig.Release) (bool, string) {
	if latest == nil {
		return false, "no release"
	}
	if latest.Status != helmconfig.StatusPendingInstall && latest.Status != helmconfig.StatusFailed {
		return false, fmt.Sprintf("revision %d is %s, neither a failed nor an interrupted install", latest.Revision, latest.Status)
	}
	if len(history) == 0 {
		return false, "the release has no stored history"
	}
	stored := map[int]bool{}
	for _, r := range history {
		if r == nil {
			return false, "the history holds an entry that cannot be read"
		}
		if stored[r.Revision] {
			return false, fmt.Sprintf("the history holds revision %d twice", r.Revision)
		}
		stored[r.Revision] = true
		switch {
		case r.Revision > latest.Revision:
			return false, fmt.Sprintf("the history holds revision %d (%s), so the release moved on since it was read", r.Revision, r.Status)
		case r.Revision == latest.Revision:
			if r.Status != latest.Status {
				return false, fmt.Sprintf("the history records revision %d as %s", r.Revision, r.Status)
			}
		case r.Status != helmconfig.StatusFailed:
			return false, fmt.Sprintf("revision %d is %s, so the release may have been live once", r.Revision, r.Status)
		}
	}
	if !stored[latest.Revision] {
		return false, fmt.Sprintf("the history does not hold revision %d", latest.Revision)
	}
	missing := func(from int) int {
		for rev := from; rev < latest.Revision; rev++ {
			if !stored[rev] {
				return rev
			}
		}
		return 0
	}
	gap := missing(1)
	if gap == 0 {
		return true, ""
	}
	through, why := readNeverDeployedMark(mg, releaseName)
	if through == 0 {
		return false, fmt.Sprintf("the history does not hold revision %d, which may have been deployed and pruned since (%s)", gap, why)
	}
	if through > latest.Revision {
		return false, fmt.Sprintf("annotation %s names revision %d, above the latest (%d), so it describes another history",
			compositionMeta.AnnotationKeyReleaseNeverDeployed, through, latest.Revision)
	}
	if gap = missing(through + 1); gap != 0 {
		return false, fmt.Sprintf("neither the history nor the %s mark (through revision %d) accounts for revision %d, which may have been deployed and pruned since",
			compositionMeta.AnnotationKeyReleaseNeverDeployed, through, gap)
	}
	return true, ""
}

// markNeverDeployed writes the mark for every revision through latest's, on the composition. The caller
// has just proven it, and has not called helm yet. An error stops the pass: the helm call that follows
// can prune the evidence, and a later pass would then find neither.
func markNeverDeployed(ctx context.Context, mg *unstructured.Unstructured, releaseName string, latest *helmconfig.Release, opts tools.UpdateOptions) (*unstructured.Unstructured, error) {
	b, err := json.Marshal(neverDeployedMark{UID: string(mg.GetUID()), Release: releaseName, ThroughRevision: latest.Revision})
	if err != nil {
		return mg, err
	}
	v := string(b)
	if mg.GetAnnotations()[compositionMeta.AnnotationKeyReleaseNeverDeployed] == v {
		return mg, nil
	}
	return patchAnnotation(ctx, mg, compositionMeta.AnnotationKeyReleaseNeverDeployed, &v, opts)
}

// clearNeverDeployedMark removes the mark, if there is one.
func clearNeverDeployedMark(ctx context.Context, mg *unstructured.Unstructured, opts tools.UpdateOptions) (*unstructured.Unstructured, error) {
	if _, ok := mg.GetAnnotations()[compositionMeta.AnnotationKeyReleaseNeverDeployed]; !ok {
		return mg, nil
	}
	return patchAnnotation(ctx, mg, compositionMeta.AnnotationKeyReleaseNeverDeployed, nil, opts)
}

// clearNeverDeployedAfterDeploy removes the mark once the release has a deployed revision. A failed
// removal is logged, not failed: neverDeployed does not trust a mark past the revisions it names, so a
// mark left behind proves nothing about this release any more, and the next pass removes it. The one
// place a left-behind mark could mislead is a NEW release of the same name, whose revisions start at 1
// again, and Create removes it before installing one, or does not install.
func clearNeverDeployedAfterDeploy(ctx context.Context, mg *unstructured.Unstructured, rel *helmconfig.Release, opts tools.UpdateOptions, log logging.Logger) *unstructured.Unstructured {
	if rel == nil || rel.Status != helmconfig.StatusDeployed {
		return mg
	}
	cleared, err := clearNeverDeployedMark(ctx, mg, opts)
	if err != nil {
		log.Info("Could not remove the never-deployed mark from a deployed release; the next pass retries",
			"error", err.Error())
		return mg
	}
	return cleared
}

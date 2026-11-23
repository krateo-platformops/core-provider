package composition

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	compositionMeta "github.com/krateo-platformops/composition-dynamic-controller/pkg/meta"
	helmconfig "github.com/krateo-platformops/plumbing/helm"
	"github.com/krateo-platformops/unstructured-runtime/pkg/logging"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clienttesting "k8s.io/client-go/testing"
)

const ndUID = "uid-636"

// ndPortal is a composition with a UID and, when mark is not empty, the never-deployed mark.
func ndPortal(mark string) *unstructured.Unstructured {
	u := srPortal("1", "x")
	u.SetUID(types.UID(ndUID))
	if mark != "" {
		u.SetAnnotations(map[string]string{compositionMeta.AnnotationKeyReleaseNeverDeployed: mark})
	}
	return u
}

func ndMark(uid, release string, through int) string {
	return fmt.Sprintf(`{"uid":%q,"release":%q,"throughRevision":%d}`, uid, release, through)
}

// withInstallRecord gives mg this controller's operation record of rel's install: what proves the
// controller started it. rel.Updated is its clock.
func withInstallRecord(mg *unstructured.Unstructured, rel *helmconfig.Release) *unstructured.Unstructured {
	_ = unstructured.SetNestedStringMap(mg.Object, map[string]string{
		"phase": "install", "startedAt": rel.Updated.UTC().Format(time.RFC3339Nano), "startedBy": "ns/a-dead-pod"}, "status", "helmOperation")
	return mg
}

// failedRevs is revisions from..to, all failed.
func failedRevs(from, to int) []*helmconfig.Release {
	var out []*helmconfig.Release
	for n := from; n <= to; n++ {
		out = append(out, rev(n, helmconfig.StatusFailed))
	}
	return out
}

// core-provider#636: the proof that a release was never deployed, with and without the mark. The
// once-live release is the row that matters: a failed rollback superseded its deployed revision and
// failed retries pruned it, so its stored history is exactly a never-deployed release's window, only
// failed revisions. Every NOT-proven row has the same history as a proven one, differing in the one
// thing its name says.
func TestNeverDeployed_636(t *testing.T) {
	failed, pending := helmconfig.StatusFailed, helmconfig.StatusPendingInstall
	for _, tc := range []struct {
		name    string
		mark    string
		latest  *helmconfig.Release
		history []*helmconfig.Release
		want    bool
	}{
		{"one failed install", "", rev(1, failed), failedRevs(1, 1), true},
		{"every attempt since the first is in the history", "", rev(3, failed), failedRevs(1, 3), true},
		{"a retry interrupted: pending-install above failed attempts", "", rev(3, pending), append(failedRevs(1, 2), rev(3, pending)), true},

		{"ONCE LIVE, pruned: only failed revisions left, revision 1 missing", "", rev(6, failed), failedRevs(4, 6), false},
		{"once live, pruned, pending-install", "", rev(6, pending), append(failedRevs(4, 5), rev(6, pending)), false},
		{"control: the same history, and a mark that accounts for what is missing", ndMark(ndUID, "r", 5), rev(6, failed), failedRevs(4, 6), true},
		{"control: the same, the mark ending exactly below the window", ndMark(ndUID, "r", 3), rev(6, failed), failedRevs(4, 6), true},
		{"control: the same, pending-install", ndMark(ndUID, "r", 5), rev(6, pending), append(failedRevs(4, 5), rev(6, pending)), true},

		// A mark left behind by a failed removal: revision 3 deployed after it was written.
		{"a stale mark: revision 3, which it does not name, is missing", ndMark(ndUID, "r", 2), rev(6, failed), failedRevs(4, 6), false},
		{"a stale mark: the deployed revision is still stored", ndMark(ndUID, "r", 2), rev(4, failed), []*helmconfig.Release{rev(3, helmconfig.StatusSuperseded), rev(4, failed)}, false},
		{"a mark naming a revision above the latest describes another history", ndMark(ndUID, "r", 9), rev(6, failed), failedRevs(4, 6), false},
		{"a mark written on another composition", ndMark("other-uid", "r", 5), rev(6, failed), failedRevs(4, 6), false},
		{"a mark for another release", ndMark(ndUID, "other", 5), rev(6, failed), failedRevs(4, 6), false},
		{"a mark that does not parse", "true", rev(6, failed), failedRevs(4, 6), false},
		{"a mark naming no revision", ndMark(ndUID, "r", 0), rev(6, failed), failedRevs(4, 6), false},
		{"a mark never overrides a revision that was live", ndMark(ndUID, "r", 5), rev(6, failed), []*helmconfig.Release{rev(4, failed), rev(5, helmconfig.StatusSuperseded), rev(6, failed)}, false},

		{"a gap inside the window", "", rev(4, failed), []*helmconfig.Release{rev(1, failed), rev(2, failed), rev(4, failed)}, false},
		{"a pending-install BELOW the latest", "", rev(2, failed), []*helmconfig.Release{rev(1, pending), rev(2, failed)}, false},
		{"an interrupted upgrade below the latest", "", rev(2, failed), []*helmconfig.Release{rev(1, helmconfig.StatusPendingUpgrade), rev(2, failed)}, false},
		{"the history moved on", "", rev(1, failed), failedRevs(1, 2), false},
		{"a revision twice", "", rev(2, failed), []*helmconfig.Release{rev(1, failed), rev(1, failed), rev(2, failed)}, false},
		{"the latest is deployed", "", rev(1, helmconfig.StatusDeployed), []*helmconfig.Release{rev(1, helmconfig.StatusDeployed)}, false},
		{"the latest is uninstalled", "", rev(1, helmconfig.StatusUninstalled), []*helmconfig.Release{rev(1, helmconfig.StatusUninstalled)}, false},
		{"no history", ndMark(ndUID, "r", 5), rev(6, failed), nil, false},
		{"an unreadable entry", "", rev(2, failed), []*helmconfig.Release{rev(1, failed), nil, rev(2, failed)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, why := neverDeployed(ndPortal(tc.mark), "r", tc.latest, tc.history); got != tc.want || (!got && why == "") {
				t.Errorf("neverDeployed = %v (%q), want %v (and a reason when false)", got, why, tc.want)
			}
		})
	}
}

// The mark that is written is the mark that is read, on the stored object; rewriting the same one costs
// no write; and it goes once the release is deployed, and only then.
func TestNeverDeployedMark(t *testing.T) {
	ctx := context.Background()
	dyn := frFakeDyn(ndPortal(""))
	patches := func() int {
		n := 0
		for _, a := range dyn.Actions() {
			if a.Matches("patch", "portals") {
				n++
			}
		}
		return n
	}
	mg, err := markNeverDeployed(ctx, ndPortal(""), "r", rev(4, helmconfig.StatusFailed), srOpts(dyn))
	if err != nil {
		t.Fatal(err)
	}
	stored := func() *unstructured.Unstructured {
		u, err := dyn.Resource(srGVR).Namespace("krateo-system").Get(ctx, "portal", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	if n, why := readNeverDeployedMark(stored(), "r"); n != 4 {
		t.Fatalf("the stored mark reads %d (%s), want 4", n, why)
	}
	if mg, err = markNeverDeployed(ctx, mg, "r", rev(4, helmconfig.StatusFailed), srOpts(dyn)); err != nil || patches() != 1 {
		t.Fatalf("the same mark again: err=%v, %d patches, want the first one only", err, patches())
	}
	if mg, err = markNeverDeployed(ctx, mg, "r", rev(5, helmconfig.StatusFailed), srOpts(dyn)); err != nil || patches() != 2 {
		t.Fatalf("control: a later revision: err=%v, %d patches, want 2", err, patches())
	}
	for _, rel := range []*helmconfig.Release{nil, rev(5, helmconfig.StatusFailed), rev(6, helmconfig.StatusPendingInstall)} {
		mg = clearNeverDeployedAfterDeploy(ctx, mg, rel, srOpts(dyn), logging.NewNopLogger())
		if n, _ := readNeverDeployedMark(stored(), "r"); n != 5 {
			t.Fatalf("release %s removed the mark of a release that is not deployed", releaseState(rel))
		}
	}
	mg = clearNeverDeployedAfterDeploy(ctx, mg, rev(6, helmconfig.StatusDeployed), srOpts(dyn), logging.NewNopLogger())
	if _, ok := stored().GetAnnotations()[compositionMeta.AnnotationKeyReleaseNeverDeployed]; ok {
		t.Fatal("a deployed release keeps its never-deployed mark")
	}
	before := patches()
	clearNeverDeployedAfterDeploy(ctx, mg, rev(6, helmconfig.StatusDeployed), srOpts(dyn), logging.NewNopLogger())
	if patches() != before {
		t.Error("a deployed release with no mark costs a patch every pass")
	}

	// A failed removal leaves the mark and the object as they were.
	bad := frFakeDyn(ndPortal(""))
	bad.PrependReactor("patch", "portals", func(clienttesting.Action) (bool, runtime.Object, error) { return true, nil, errors.New("denied") })
	marked := ndPortal(ndMark(ndUID, "r", 5))
	if out := clearNeverDeployedAfterDeploy(ctx, marked, rev(6, helmconfig.StatusDeployed), srOpts(bad), logging.NewNopLogger()); out != marked {
		t.Errorf("a failed removal returned another object: %v", out)
	}
	if _, err := clearNeverDeployedMark(ctx, marked, srOpts(bad)); err == nil {
		t.Error("clearNeverDeployedMark hid a failed removal, so Create would install a new release under an old mark")
	}
}

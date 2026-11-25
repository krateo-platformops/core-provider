//go:build envtest

package composition

import (
	"fmt"
	"strings"
	"testing"

	helmv3 "github.com/krateo-platformops/plumbing/helm/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// core-provider#640: a failed revision with no record of what was applied (helm alone wrote it, or a
// plumbing before v1.110.0) is not healed, and the composition says so ONCE, although every pass writes
// another unrecorded failed revision. Simulated by removing the record's label from revision 2's Secret,
// which is where helm keeps a release's labels. CONTROL: TestEnvtest_640_PreUpgradeHookFailingHealsToWhatWasApplied,
// the same release with its record.
//
// A file of its own because helmv3.AppliedManifestLabel is new in plumbing v1.110.0: the other 640 arms
// must still compile against v1.109.0, where they are the control that fails.
func TestEnvtest_640_AFailedRevisionWithoutARecordIsNotHealed(t *testing.T) {
	e := start636(t)
	a := e.arm640(t, "norec640")
	a.failUpgrade(map[string]any{"k": "v2", "hookns": a.ns, "both": true, "prens": "no-such-namespace"})

	secret := fmt.Sprintf("sh.helm.release.v1.%s.v2", a.relName())
	s, err := a.e.kube.CoreV1().Secrets(a.ns).Get(a.e.ctx, secret, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Labels[helmv3.AppliedManifestLabel] == "" {
		t.Fatalf("control: revision 2 carries no %s to remove: %v", helmv3.AppliedManifestLabel, s.Labels)
	}
	delete(s.Labels, helmv3.AppliedManifestLabel)
	if _, err := a.e.kube.CoreV1().Secrets(a.ns).Update(a.e.ctx, s, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	a.deleteCM("app")
	a.log.reset()

	before := a.history()
	for pass := 1; pass <= 3; pass++ {
		if err := a.observe640(); err == nil {
			t.Fatalf("pass %d: control: the pre-upgrade hook still fails, so the retry must", pass)
		}
		if a.cm("app") {
			t.Fatalf("pass %d: app was re-created from a revision that records nothing applied", pass)
		}
	}
	if after := a.history(); after == before {
		t.Fatalf("control: no pass wrote a revision (%s), so once-per-pass and once-per-composition cannot be told apart", after)
	}
	lines := a.log.matching(noRecordLine640)
	t.Logf("SAMPLE %v; history %s -> %s", lines, before, a.history())
	if len(lines) != 1 || !strings.Contains(lines[0], "revision=2") || !strings.Contains(lines[0], "recorded=false") {
		t.Errorf("no-record lines %v, want exactly one, for revision 2", lines)
	}
	if l := a.log.matching(healedLine640); len(l) != 0 {
		t.Errorf("a heal was logged: %v", l)
	}
}

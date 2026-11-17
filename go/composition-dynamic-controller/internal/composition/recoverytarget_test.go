package composition

import (
	"strings"
	"testing"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
)

func rev(n int, s helmconfig.Status) *helmconfig.Release {
	return &helmconfig.Release{Name: "r", Revision: n, Status: s}
}

// #563: the recovery's target, for every input state the stuck-operation recovery can meet. The
// rows name the state, not the code path: a deployed predecessor next door, one beyond MaxHistory
// (helm keeps it), none at all (first install, after a failed rollback), a stuck rollback, a stuck
// uninstall, and the reads that cannot be trusted.
func TestRecoveryTarget(t *testing.T) {
	const (
		deployed   = helmconfig.StatusDeployed
		superseded = helmconfig.StatusSuperseded
		failed     = helmconfig.StatusFailed
		pInstall   = helmconfig.StatusPendingInstall
		pUpgrade   = helmconfig.StatusPendingUpgrade
		pRollback  = helmconfig.StatusPendingRollback
		uninst     = helmconfig.StatusUninstalling
	)
	for _, tc := range []struct {
		name       string
		pending    *helmconfig.Release
		history    []*helmconfig.Release
		want       int
		unverified bool
		wantErr    string
		whyHas     string
	}{
		{name: "interrupted upgrade: its deployed predecessor", pending: rev(3, pUpgrade),
			history: []*helmconfig.Release{rev(1, superseded), rev(2, deployed), rev(3, pUpgrade)}, want: 2, whyHas: "revision 2, the last deployed"},
		{name: "the deployed revision beyond MaxHistory, which helm keeps", pending: rev(14, pUpgrade),
			history: []*helmconfig.Release{rev(10, deployed), rev(13, failed), rev(14, pUpgrade)}, want: 10},
		{name: "history in any order", pending: rev(14, pUpgrade),
			history: []*helmconfig.Release{rev(14, pUpgrade), rev(10, deployed), rev(13, failed)}, want: 10},
		{name: "two deployed (helm's concurrent-write corruption): the newest", pending: rev(5, pUpgrade),
			history: []*helmconfig.Release{rev(2, deployed), rev(4, deployed), rev(5, pUpgrade)}, want: 4},
		{name: "interrupted rollback: the last deployed", pending: rev(5, pRollback),
			history: []*helmconfig.Release{rev(3, deployed), rev(4, failed), rev(5, pRollback)}, want: 3},
		{name: "interrupted first install: itself, hooks unverified", pending: rev(1, pInstall),
			history: []*helmconfig.Release{rev(1, pInstall)}, want: 1, unverified: true, whyHas: "its install hooks may not have run"},
		{name: "no deployed predecessor (after a failed rollback): itself, hooks unverified", pending: rev(7, pUpgrade),
			history: []*helmconfig.Release{rev(5, superseded), rev(6, failed), rev(7, pUpgrade)}, want: 7, unverified: true, whyHas: "upgrade hooks may not have run"},
		{name: "reinstall after uninstall --keep-history: itself, hooks unverified", pending: rev(4, pInstall),
			history: []*helmconfig.Release{rev(3, helmconfig.StatusUninstalled), rev(4, pInstall)}, want: 4, unverified: true},
		{name: "stuck uninstall: itself, as before (out of scope)", pending: rev(4, uninst),
			history: []*helmconfig.Release{rev(3, superseded), rev(4, uninst)}, want: 4},
		{name: "empty history: an error, never a guess", pending: rev(3, pUpgrade), wantErr: "no stored history"},
		{name: "history without the pending revision: an error", pending: rev(3, pUpgrade),
			history: []*helmconfig.Release{rev(1, superseded), rev(2, deployed)}, wantErr: "does not hold it"},
		{name: "history with a newer revision (the release moved on): an error", pending: rev(3, pUpgrade),
			history: []*helmconfig.Release{rev(2, deployed), rev(3, pUpgrade), rev(4, deployed)}, wantErr: "moved on"},
		{name: "no release: an error", wantErr: "no release"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := recoveryTarget(tc.pending, tc.history)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one containing %q (plan %+v)", err, tc.wantErr, got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.revision != tc.want || got.hooksUnverified != tc.unverified {
				t.Errorf("plan = revision %d, hooksUnverified %v; want %d, %v (%s)", got.revision, got.hooksUnverified, tc.want, tc.unverified, got.why)
			}
			if tc.whyHas != "" && !strings.Contains(got.why, tc.whyHas) {
				t.Errorf("why = %q, want it to contain %q", got.why, tc.whyHas)
			}
		})
	}
}

package composition

import (
	"testing"

	"github.com/krateo-platformops/composition-dynamic-controller/internal/tools/archive"
	helmconfig "github.com/krateo-platformops/plumbing/helm"
)

func TestRetiringController(t *testing.T) {
	for _, tc := range []struct {
		name string
		pkg  *archive.Info
		want bool
	}{
		{"the current version's controller", &archive.Info{Version: "0.3.64", DefinitionVersion: "0.3.64"}, false},
		{"an older version's controller after the definition moved", &archive.Info{Version: "0.3.62", DefinitionVersion: "0.3.64"}, true},
		{"no definition version resolved: not retiring", &archive.Info{Version: "0.3.62"}, false},
		{"no chart version for this controller: not retiring", &archive.Info{DefinitionVersion: "0.3.64"}, false},
		{"no package info", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, why := retiringController(tc.pkg)
			if got != tc.want || (got && why == "") {
				t.Fatalf("retiringController = (%v, %q), want %v with a reason when true", got, why, tc.want)
			}
		})
	}
}

// The hold reaches plumbing's Reconcile for a retiring controller, and only for it.
func TestReconcileConfigCarriesTheHold(t *testing.T) {
	if cfg := reconcileConfig(&helmconfig.ActionConfig{}, nil, true); !cfg.HoldDesiredStateChange {
		t.Error("retiring: HoldDesiredStateChange not set")
	}
	if cfg := reconcileConfig(&helmconfig.ActionConfig{}, nil, false); cfg.HoldDesiredStateChange {
		t.Error("current: HoldDesiredStateChange set")
	}
}

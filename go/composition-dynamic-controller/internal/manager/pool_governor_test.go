package manager

import (
	"context"
	"strings"
	"testing"

	"github.com/krateo-platformops/unstructured-runtime/pkg/ratelimit"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

// core-provider#561: a pool's members share the process's governor, so Pool.Run refuses one not built
// to be shared. It is checked here, in the pool, because main.go's wiring cannot be unit-tested (the
// image compiles main.go alone). Control: a Shared governor gets past the check (the run then ends at
// once on a cancelled context).
func TestPoolRunRefusesAnUnsharedGovernor(t *testing.T) {
	run := func(g ratelimit.Governor) error {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		p := &Pool{Process: Process{Governor: g}, Client: k8sfake.NewSimpleClientset(), Namespace: "krateo-system", Index: "cdc-pool-p-members"}
		return p.Run(ctx)
	}
	for name, g := range map[string]ratelimit.Governor{
		"none":     nil,
		"unshared": ratelimit.New(ratelimit.Options{Ceiling: 4}),
	} {
		if err := run(g); err == nil || !strings.Contains(err.Error(), "SHARED governor") {
			t.Errorf("%s governor: err = %v, want the shared-governor refusal", name, err)
		}
	}
	if err := run(ratelimit.New(ratelimit.Options{Ceiling: 4, Shared: true})); err != nil && strings.Contains(err.Error(), "SHARED governor") {
		t.Errorf("control: a Shared governor was refused: %v", err)
	}
}

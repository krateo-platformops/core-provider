package saturation

import (
	"context"
	"testing"
	"time"

	"github.com/krateo-platformops/unstructured-runtime/pkg/ratelimit"
)

func acquire(t *testing.T, g ratelimit.Governor, d time.Duration) (func(), bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return g.(ratelimit.LocalGate).AcquireLocal(ctx)
}

// core-provider#561: a pool's members share ONE governor that is Shared, and enforces the pool's total
// even with saturation off. Control: the dedicated governor with saturation off is nil, as today, so
// a dedicated controller is unchanged.
func TestNewPoolGovernor(t *testing.T) {
	g, err := NewPoolGovernor(false, 0, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sg, ok := g.(ratelimit.SharedLocalGate); !ok || !sg.SharedLocal() {
		t.Fatal("the pool's governor is not Shared: an idle member's worker would hold a slot")
	}
	r1, ok1 := acquire(t, g, time.Second)
	r2, ok2 := acquire(t, g, time.Second)
	if !ok1 || !ok2 {
		t.Fatal("the pool's total of 2 did not grant 2 slots")
	}
	if _, ok := acquire(t, g, 50*time.Millisecond); ok {
		t.Fatal("a third slot was granted over the pool's total of 2, with saturation off")
	}
	r1()
	r2()

	if _, err := NewPoolGovernor(false, 0, 0, nil); err == nil {
		t.Error("a pool with no total workers was accepted")
	}
	if d, err := NewGovernor(false, 0, 4, nil); err != nil || d != nil {
		t.Errorf("control: the dedicated governor with saturation off = %v, %v; want nil, nil", d, err)
	}
}

//go:build envtest

package compositiondefinitions

import (
	"context"
	"sync/atomic"
	"testing"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/tools/writtenby"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// patchCount669 counts the status patches the scale-to-zero reconciler sends.
type patchCount669 struct {
	client.Client
	n *atomic.Int32
}

func (c patchCount669) Status() client.SubResourceWriter {
	return patchWriter669{SubResourceWriter: c.Client.Status(), n: c.n}
}

type patchWriter669 struct {
	client.SubResourceWriter
	n *atomic.Int32
}

func (w patchWriter669) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	w.n.Add(1)
	return w.SubResourceWriter.Patch(ctx, obj, patch, opts...)
}

// core-provider#669: the scale-to-zero reconciler's status patch is a status write by this binary,
// so it names it, and the stamp is no reason for a patch of its own.
func TestScaleToZeroWrittenByEnvtest(t *testing.T) {
	yes := true
	e := startStzEnv(t)
	e.controllerDeployment(t, 1)
	cd := e.definition(t, "a", &yes, "10m", nil)
	var patches atomic.Int32
	e.r.kube = patchCount669{Client: e.r.kube, n: &patches}
	w := writtenby.New("2.13.135", "c0ffee135")
	e.r.writtenBy = w

	read := func() *compositiondefinitionsv1alpha1.CompositionDefinition {
		t.Helper()
		got := &compositiondefinitionsv1alpha1.CompositionDefinition{}
		if err := e.kube.Get(context.Background(), client.ObjectKeyFromObject(cd), got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	if read().Status.WrittenBy != nil {
		t.Fatal("premise: the definition starts with no status.writtenBy")
	}

	// The first pass starts the hold-down: status.scaleToZero changes, and the patch names this binary.
	if _, st := e.reconcile(t, cd); st == nil || st.Reason != "IdleHoldDown" {
		t.Fatalf("control: status.scaleToZero = %+v, want the IdleHoldDown the patch writes", st)
	}
	if patches.Load() != 1 {
		t.Fatalf("control: %d status patches on the pass that starts the hold-down, want 1", patches.Load())
	}
	got := read()
	if got.Status.WrittenBy == nil || *got.Status.WrittenBy != w {
		t.Fatalf("the scale-to-zero patch: status.writtenBy = %+v, want %+v", got.Status.WrittenBy, w)
	}

	// Within the hold-down the decision is the same. Whatever this pass sends, the apiserver records
	// nothing: the stamp it carries is the stored one.
	rv := got.ResourceVersion
	patches.Store(0)
	e.reconcile(t, cd)
	sent := patches.Load()
	t.Logf("an unchanged decision sent %d status patches", sent)
	if after := read(); after.ResourceVersion != rv {
		t.Errorf("an unchanged decision by the same binary wrote: resourceVersion %s -> %s, status.writtenBy %+v", rv, after.ResourceVersion, after.Status.WrittenBy)
	}

	// A new binary: a patch it sends names it; a pass that sends none leaves the stored stamp. Which
	// one happens is the reconciler's own rule (persist), not the stamp's.
	next := writtenby.New("2.13.136", "c0ffee136")
	e.r.writtenBy = next
	patches.Store(0)
	e.reconcile(t, cd)
	after := read()
	switch {
	case patches.Load() > 0 && *after.Status.WrittenBy != next:
		t.Errorf("the next binary sent a status patch and status.writtenBy = %+v, want %+v", after.Status.WrittenBy, next)
	case patches.Load() == 0 && (after.ResourceVersion != rv || *after.Status.WrittenBy != w):
		t.Errorf("no status patch, and the definition changed: resourceVersion %s -> %s, status.writtenBy %+v", rv, after.ResourceVersion, after.Status.WrittenBy)
	}
	if patches.Load() != sent {
		t.Errorf("the same decision sent %d patches under one stamp and %d under another: the stamp decided a write", sent, patches.Load())
	}
}

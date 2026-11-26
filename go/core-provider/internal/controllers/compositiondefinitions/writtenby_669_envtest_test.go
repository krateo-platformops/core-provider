//go:build envtest

package compositiondefinitions

import (
	"context"
	"sync/atomic"
	"testing"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/tools/writtenby"
	rtv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	"github.com/krateo-platformops/provider-runtime/pkg/reconciler"
	"github.com/krateo-platformops/provider-runtime/pkg/resource"
	"github.com/krateo-platformops/provider-runtime/pkg/resource/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// core-provider#669 on a real apiserver, through provider-runtime's own reconciler and the real
// connector: every CompositionDefinition status core-provider writes names this binary in
// status.writtenBy, and the stamp never causes a write of its own.
//
// Read from what the apiserver SERVED: the CRD is the generated one (crds/), so a field it did not
// declare would be pruned and read back absent.

// statusCount669 counts the status updates the reconciler sends: provider-runtime sends one at the end
// of every pass, so the count says the at-rest passes below DID send a status, and the unchanged
// resourceVersion says the apiserver recorded nothing.
type statusCount669 struct {
	client.Client
	n *atomic.Int32
}

func (c statusCount669) Status() client.SubResourceWriter {
	return statusWriter669{SubResourceWriter: c.Client.Status(), n: c.n}
}

type statusWriter669 struct {
	client.SubResourceWriter
	n *atomic.Int32
}

func (w statusWriter669) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	w.n.Add(1)
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

// reconciler669 is env644.reconciler with this binary's stamp on the connector, as Setup passes
// Options.WrittenBy, and the status updates counted.
func (e *env644) reconciler669(w compositiondefinitionsv1alpha1.WrittenBy, n *atomic.Int32) *reconciler.Reconciler {
	kube := statusCount669{Client: e.kube, n: n}
	return reconciler.NewReconciler(&fake.Manager{Client: kube, Scheme: e.c.cl.Scheme()},
		resource.ManagedKind(compositiondefinitionsv1alpha1.CompositionDefinitionGroupVersionKind),
		reconciler.WithExternalConnecter(&connector{
			dynamic: e.c.dyn, client: e.c.cs, kube: e.kube, apiReader: e.reader,
			log: logging.NewNopLogger(), pluralizer: pluralizer644{e.c.cs.Discovery()},
			writtenBy: w,
		}),
		reconciler.WithCreationGracePeriod(0),
	)
}

func TestEnvtest_669_WrittenBy(t *testing.T) {
	e := start644(t)
	e.charts.add(t, "stamped", "1.0.0", `{"type":"object"}`)
	d := e.define(t, "stamped", "stamped", nil)
	var updates atomic.Int32
	first := writtenby.New("2.13.135", "c0ffee135")
	d.r = e.reconciler669(first, &updates)
	d.rest(6)

	stored := func() compositiondefinitionsv1alpha1.WrittenBy {
		t.Helper()
		w := d.cd().Status.WrittenBy
		if w == nil {
			return compositiondefinitionsv1alpha1.WrittenBy{}
		}
		return *w
	}
	if got := stored(); got != first {
		t.Fatalf("at rest: status.writtenBy = %+v, want %+v", got, first)
	}
	if got := stored().Name; got != "core-provider" {
		t.Errorf("status.writtenBy.name = %q, want core-provider", got)
	}

	// THE NO-OP RULE. At rest, passes that change nothing else send the status (provider-runtime's
	// writeStatus) and the apiserver records nothing: the stamp is the stored one.
	rv := d.cd().ResourceVersion
	updates.Store(0)
	const passes = 3
	for i := 0; i < passes; i++ {
		if _, err := d.pass(); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("at rest: %d passes sent %d status updates; resourceVersion %s -> %s", passes, updates.Load(), rv, d.cd().ResourceVersion)
	if updates.Load() < passes {
		t.Fatalf("control: %d status updates in %d passes; the passes did not reach the status write, so an unchanged resourceVersion would prove nothing", updates.Load(), passes)
	}
	if got := d.cd().ResourceVersion; got != rv {
		t.Errorf("at rest the definition is still being written: resourceVersion %s -> %s (status.writtenBy %+v)", rv, got, stored())
	}

	// A NEW BINARY. The next status write carries its version, on a pass that changes nothing else.
	next := writtenby.New("2.13.136", "c0ffee136")
	d.r = e.reconciler669(next, &updates)
	if _, err := d.pass(); err != nil {
		t.Fatal(err)
	}
	if got := stored(); got != next {
		t.Errorf("after a pass by the next binary: status.writtenBy = %+v, want %+v", got, next)
	}
	if d.cond(string(rtv1.TypeReady)) != "True/Available" {
		t.Errorf("the new binary's pass changed Ready to %s", d.cond(string(rtv1.TypeReady)))
	}
}

// A pass whose Connect fails (a remote target that cannot be read) still ends in a status write,
// provider-runtime's ReconcileError, and that write names this binary too.
func TestEnvtest_669_WrittenByOnAConnectFailure(t *testing.T) {
	e := start644(t)
	e.charts.add(t, "unreachable", "1.0.0", `{"type":"object"}`)
	d := e.define(t, "unreachable", "unreachable", func(cd *compositiondefinitionsv1alpha1.CompositionDefinition) {
		cd.Spec.Deploy = &compositiondefinitionsv1alpha1.DeploymentTarget{TargetRef: &compositiondefinitionsv1alpha1.TargetReference{Name: "no-such-target"}}
	})
	var updates atomic.Int32
	w := writtenby.New("2.13.135", "c0ffee135")
	d.r = e.reconciler669(w, &updates)
	if _, err := d.pass(); err == nil {
		t.Fatal("control: the pass succeeded, so it is not a Connect failure")
	}
	if got := d.cond(string(rtv1.TypeSynced)); got != "False/ReconcileError" {
		t.Fatalf("control: Synced=%s, want the runtime's False/ReconcileError write", got)
	}
	got := d.cd().Status.WrittenBy
	if got == nil || *got != w {
		t.Errorf("the Connect failure's status write: status.writtenBy = %+v, want %+v", got, w)
	}
}

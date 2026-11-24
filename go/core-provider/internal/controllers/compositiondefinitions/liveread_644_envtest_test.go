//go:build envtest

package compositiondefinitions

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	rtv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	"github.com/krateo-platformops/provider-runtime/pkg/reconciler"
	"github.com/krateo-platformops/provider-runtime/pkg/resource"
	"github.com/krateo-platformops/provider-runtime/pkg/resource/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// lagging644 is the manager's client with an informer cache that has not caught up: while stale is
// set, a Get of that CompositionDefinition is answered from it, as the cache reader answers (the whole
// object replaced, never merged). Everything else, and every write, is the live client's.
type lagging644 struct {
	client.Client
	mu    sync.Mutex
	stale *compositiondefinitionsv1alpha1.CompositionDefinition
	// served counts the Gets answered from stale.
	served int
}

func (l *lagging644) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if cd, ok := obj.(*compositiondefinitionsv1alpha1.CompositionDefinition); ok && l.stale != nil && key == client.ObjectKeyFromObject(l.stale) {
		l.stale.DeepCopyInto(cd)
		l.served++
		return nil
	}
	return l.Client.Get(ctx, key, obj, opts...)
}

func (l *lagging644) lag(stale *compositiondefinitionsv1alpha1.CompositionDefinition) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.stale, l.served = stale, 0
}

// #644. status.observedGeneration is the generation of the object Observe re-reads live (D3), so that
// read must hold exactly what the apiserver serves. Observe reads into a COPY of the cached object, and
// a plain JSON decode into a populated object would keep a field the response does not carry: a field
// removed from the spec behind a lagging cache would be reconciled under the new generation.
//
// It does not happen: controller-runtime's typed client zeroes its target before it decodes
// (apiutil's targetZeroingDecoder). That is the client's property and not this repo's code, so it is
// held here: every removal is a real update of a real object, read back through the real client.
//
// The chart is unreadable on purpose: Observe answers from status and returns right after the read.
func TestEnvtest_644_LiveReadHoldsNoRemovedField(t *testing.T) {
	ctx := context.Background()
	c := start642(t)
	cl := c.cl
	noChart := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(noChart.Close)

	ptr := func(s string) *string { return &s }
	i32 := func(i int32) *int32 { return &i }
	removed := func(cd *compositiondefinitionsv1alpha1.CompositionDefinition) {
		cd.Spec.ApiRef = nil
		cd.Spec.Controller.Pool = nil
		cd.Spec.Controller.ResyncInterval = nil
		cd.Spec.StatusDataTemplate = nil
		delete(cd.Annotations, "gone")
		cd.Labels = nil
	}
	for _, tc := range []struct {
		name string
		edit func(*compositiondefinitionsv1alpha1.CompositionDefinition)
		// gone says the removed thing is absent from what Observe holds.
		gone func(*compositiondefinitionsv1alpha1.CompositionDefinition) bool
	}{
		{"spec.apiRef", func(cd *compositiondefinitionsv1alpha1.CompositionDefinition) { cd.Spec.ApiRef = nil },
			func(cd *compositiondefinitionsv1alpha1.CompositionDefinition) bool { return cd.Spec.ApiRef == nil }},
		{"spec.controller", func(cd *compositiondefinitionsv1alpha1.CompositionDefinition) { cd.Spec.Controller = nil },
			func(cd *compositiondefinitionsv1alpha1.CompositionDefinition) bool { return cd.Spec.Controller == nil }},
		{"spec.controller.pool", func(cd *compositiondefinitionsv1alpha1.CompositionDefinition) { cd.Spec.Controller.Pool = nil },
			func(cd *compositiondefinitionsv1alpha1.CompositionDefinition) bool {
				return cd.Spec.Controller != nil && cd.Spec.Controller.Pool == nil && cd.Spec.Controller.Workers != nil
			}},
		{"all of them", removed, func(cd *compositiondefinitionsv1alpha1.CompositionDefinition) bool {
			return cd.Spec.ApiRef == nil && cd.Spec.Controller != nil && cd.Spec.Controller.Pool == nil && cd.Spec.Controller.ResyncInterval == nil &&
				len(cd.Spec.StatusDataTemplate) == 0 && cd.Annotations["gone"] == "" && cd.Annotations["kept"] == "yes" && len(cd.Labels) == 0
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cd := &compositiondefinitionsv1alpha1.CompositionDefinition{ObjectMeta: metav1.ObjectMeta{
				GenerateName: "live-", Namespace: "default",
				Annotations: map[string]string{"gone": "yes", "kept": "yes"}, Labels: map[string]string{"gone": "yes"},
			}}
			cd.Spec.Chart = &compositiondefinitionsv1alpha1.ChartInfo{Url: noChart.URL + "/contents-1.0.0.tgz", Version: "1.0.0"}
			cd.Spec.ApiRef = &compositiondefinitionsv1alpha1.ApiReference{Name: "ra", Namespace: "default"}
			cd.Spec.Controller = &compositiondefinitionsv1alpha1.ControllerConfig{Workers: i32(2), Pool: ptr("pages"), ResyncInterval: ptr("30m")}
			cd.Spec.StatusDataTemplate = []compositiondefinitionsv1alpha1.StatusFieldMapping{{ForPath: "added", Expression: "${ .spec }"}}
			if err := cl.Create(ctx, cd); err != nil {
				t.Fatal(err)
			}
			cd.Status.Digest = "recorded"
			cd.Status.PackageURL = "oci://recorded"
			if err := cl.Status().Update(ctx, cd); err != nil {
				t.Fatal(err)
			}
			key := client.ObjectKeyFromObject(cd)
			cached := &compositiondefinitionsv1alpha1.CompositionDefinition{}
			if err := cl.Get(ctx, key, cached); err != nil {
				t.Fatal(err)
			}
			live := cached.DeepCopy()
			tc.edit(live)
			if err := cl.Update(ctx, live); err != nil {
				t.Fatal(err)
			}
			// A status field removed behind the cache too.
			live.Status.PackageURL = ""
			if err := cl.Status().Update(ctx, live); err != nil {
				t.Fatal(err)
			}
			served := &compositiondefinitionsv1alpha1.CompositionDefinition{}
			if err := cl.Get(ctx, key, served); err != nil {
				t.Fatal(err)
			}
			if tc.gone(cached) || !tc.gone(served) {
				t.Fatalf("premise: the cached copy must carry it and the served object must not (cached gone=%v, served gone=%v)", tc.gone(cached), tc.gone(served))
			}

			e := &external{mgmt: cl, kube: cl, apiReader: cl, dynamic: c.dyn, mgmtDynamic: c.dyn, client: c.cs, log: logging.NewNopLogger(), pluralizer: notFoundPluralizer{}}
			observed := cached.DeepCopy()
			if _, err := e.Observe(ctx, observed); err != nil {
				t.Fatalf("Observe: %v", err)
			}
			if observed.GetCondition(rtv1.TypeReady).Reason != "Unavailable" {
				t.Fatalf("premise: Observe must have run to the chart read; Ready=%+v", observed.GetCondition(rtv1.TypeReady))
			}
			if !tc.gone(observed) {
				t.Errorf("Observe ran on a spec that still carries the removed field, at generation %d", observed.Generation)
			}
			// The whole of it: spec and metadata are exactly the served ones, and the status is the
			// served one under what Observe itself writes before the chart read (conditions, target).
			if observed.Generation != served.Generation || observed.ResourceVersion != served.ResourceVersion {
				t.Errorf("generation/resourceVersion = %d/%s, want the served %d/%s", observed.Generation, observed.ResourceVersion, served.Generation, served.ResourceVersion)
			}
			if !reflect.DeepEqual(observed.Spec, served.Spec) {
				t.Errorf("spec = %+v\nwant the served %+v", observed.Spec, served.Spec)
			}
			if !reflect.DeepEqual(observed.ObjectMeta, served.ObjectMeta) {
				t.Errorf("metadata = %+v\nwant the served %+v", observed.ObjectMeta, served.ObjectMeta)
			}
			if observed.Status.PackageURL != "" || observed.Status.Digest != "recorded" {
				t.Errorf("status.packageUrl=%q digest=%q, want the served \"\" and \"recorded\"", observed.Status.PackageURL, observed.Status.Digest)
			}
			// What the read replaces that a caller could have set: the GVK. A live typed read has never
			// kept it (the decoder clears it), so nothing downstream can rely on it.
			if observed.GroupVersionKind() != served.GroupVersionKind() {
				t.Errorf("GVK = %v, want what a live read gives, %v", observed.GroupVersionKind(), served.GroupVersionKind())
			}
		})
	}
}

// The same through provider-runtime's reconciler, on its EFFECT: with a lagging cache, a removed
// spec.apiRef reaches the rendered controller. Read into the cached copy without zeroing, the pass would
// render the removed apiRef again, find the digest unchanged, and report the definition up to date at a
// generation whose spec it never applied.
func TestEnvtest_644_RemovedApiRefIsAppliedWithALaggingCache(t *testing.T) {
	c := start642(t)
	c.installAuthnCRD(t, true)
	b := newBench642(t, c, c)
	b.settle(5)
	lag := &lagging644{Client: c.cl}
	b.r = reconciler.NewReconciler(&fake.Manager{Client: lag, Scheme: c.cl.Scheme()},
		resource.ManagedKind(compositiondefinitionsv1alpha1.CompositionDefinitionGroupVersionKind),
		reconciler.WithExternalConnecter(reconciler.ExternalConnectorFn(func(context.Context, resource.Managed) (reconciler.ExternalClient, error) {
			return b.ext, nil
		})),
	)
	digest := b.cd().Status.Digest

	cached := b.cd()
	live := cached.DeepCopy()
	live.Spec.ApiRef = nil
	if err := c.cl.Update(context.Background(), live); err != nil {
		t.Fatal(err)
	}
	lag.lag(cached)
	if _, err := b.pass(); err != nil {
		t.Fatalf("the pass with the lagging cache: %v", err)
	}
	if lag.served == 0 {
		t.Fatal("control: the runtime never read the definition from the lagging cache")
	}
	if got := b.cd().Status.Digest; got == digest {
		t.Errorf("status.digest is unchanged (%s): the removed spec.apiRef was not applied, and the definition reads up to date (Synced=%s, Ready=%s) at generation %d",
			got, b.cond("Synced"), b.cond("Ready"), b.cd().Generation)
	}
}

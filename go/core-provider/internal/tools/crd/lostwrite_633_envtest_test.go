//go:build envtest

package crd

import (
	"context"
	"os"
	"sync/atomic"
	"testing"

	"github.com/krateo-platformops/core-provider/internal/tools/crd/generation"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

const thirdPartyKey = "example.krateo.io/third-party"

// staleReader is a client whose Get answers with a snapshot taken earlier: exactly what the
// controller-runtime cache (mgr.GetClient(), which the CompositionDefinition reconciler passes as cli)
// returns while it lags the apiserver. Every other call goes to the live client.
type staleReader struct {
	client.Client
	snapshot *apiextensionsv1.CustomResourceDefinition
}

func (s staleReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if crd, ok := obj.(*apiextensionsv1.CustomResourceDefinition); ok && key.Name == s.snapshot.Name {
		s.snapshot.DeepCopyInto(crd)
		return nil
	}
	return s.Client.Get(ctx, key, obj, opts...)
}

// racingDynamic makes a second writer annotate the CRD right before core-provider's FIRST Update of it
// reaches the apiserver, after the update path has read the object: the write core-provider then sends
// carries a resourceVersion that is already stale, the apiserver answers 409, and only a write that is
// recomputed from the object it replaces keeps both changes.
type racingDynamic struct {
	dynamic.Interface
	live    dynamic.Interface
	crdName string
	raced   *atomic.Bool
}

func (r racingDynamic) Resource(gvr schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	return racingResource{NamespaceableResourceInterface: r.Interface.Resource(gvr), r: r, gvr: gvr}
}

type racingResource struct {
	dynamic.NamespaceableResourceInterface
	r   racingDynamic
	gvr schema.GroupVersionResource
}

func (rr racingResource) Update(ctx context.Context, obj *unstructured.Unstructured, opts metav1.UpdateOptions, sub ...string) (*unstructured.Unstructured, error) {
	if len(sub) == 0 && obj.GetName() == rr.r.crdName && rr.r.raced.CompareAndSwap(false, true) {
		cur, err := rr.r.live.Resource(rr.gvr).Get(ctx, obj.GetName(), metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		ann := cur.GetAnnotations()
		if ann == nil {
			ann = map[string]string{}
		}
		ann[thirdPartyKey] = "raced"
		cur.SetAnnotations(ann)
		if _, err := rr.r.live.Resource(rr.gvr).Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
			return nil, err
		}
	}
	return rr.NamespaceableResourceInterface.Update(ctx, obj, opts, sub...)
}

// #633 on a real apiserver. core-provider's CRD update is a read-modify-write: it reads the CRD
// (through the cache), projects the new version onto that copy, and writes it. A change another
// writer made after core-provider's read must survive the write: here an annotation, the shape of
// provider-runtime's credential-use Disown switch (#402) and of any third party's metadata.
func TestEnvtest_633_CRDUpdateKeepsAnotherWritersChange(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Fatal("KUBEBUILDER_ASSETS is not set: this test exists to run against a real apiserver, and a skip would read as a pass")
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	sc := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(sc)
	_ = apiextensionsv1.AddToScheme(sc)
	cl, err := client.New(cfg, client.Options{Scheme: sc})
	if err != nil {
		t.Fatal(err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	gen := func(kind, version string) *apiextensionsv1.CustomResourceDefinition {
		t.Helper()
		c, err := generation.GenerateCRD([]byte(`{"type":"object","properties":{"replicas":{"type":"integer"}}}`),
			schema.GroupVersionKind{Group: "composition.krateo.io", Version: version, Kind: kind})
		if err != nil {
			t.Fatalf("GenerateCRD %s %s: %v", kind, version, err)
		}
		return c
	}
	read := func(name string) *apiextensionsv1.CustomResourceDefinition {
		t.Helper()
		got := &apiextensionsv1.CustomResourceDefinition{}
		if err := cl.Get(ctx, types.NamespacedName{Name: name}, got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	annotate := func(name, value string) {
		t.Helper()
		cur := read(name)
		ann := cur.GetAnnotations()
		if ann == nil {
			ann = map[string]string{}
		}
		ann[thirdPartyKey] = value
		cur.SetAnnotations(ann)
		if err := cl.Update(ctx, cur); err != nil {
			t.Fatalf("second writer: %v", err)
		}
	}
	served := func(crd *apiextensionsv1.CustomResourceDefinition, version string) bool {
		for _, v := range crd.Spec.Versions {
			if v.Name == version && v.Served {
				return true
			}
		}
		return false
	}

	t.Run("a change made after core-provider's (cached) read survives the version add", func(t *testing.T) {
		v1, v2 := gen("Widget", "v1-0-0"), gen("Widget", "v1-1-0")
		if _, err := ApplyOrUpdateCRD(ctx, cl, dyn, v1); err != nil {
			t.Fatalf("setup: %v", err)
		}
		snapshot := read(v1.Name) // what a lagging cache holds
		annotate(v1.Name, "x")    // the second writer, after that read
		if _, err := ApplyOrUpdateCRD(ctx, staleReader{Client: cl, snapshot: snapshot}, dyn, v2); err != nil {
			t.Fatalf("version add: %v", err)
		}
		got := read(v1.Name)
		if !served(got, "v1-1-0") {
			t.Fatalf("premise: the version add must have landed; served versions: %v", declaredVersions(got))
		}
		if got.GetAnnotations()[thirdPartyKey] != "x" {
			t.Fatalf("the second writer's annotation %s was lost by core-provider's CRD update: annotations now %v", thirdPartyKey, got.GetAnnotations())
		}
	})

	t.Run("a change racing core-provider's write (409) is kept and the version still lands", func(t *testing.T) {
		v1, v2 := gen("Gadget", "v1-0-0"), gen("Gadget", "v1-1-0")
		if _, err := ApplyOrUpdateCRD(ctx, cl, dyn, v1); err != nil {
			t.Fatalf("setup: %v", err)
		}
		raced := &atomic.Bool{}
		racer := racingDynamic{Interface: dyn, live: dyn, crdName: v1.Name, raced: raced}
		if _, err := ApplyOrUpdateCRD(ctx, cl, racer, v2); err != nil {
			t.Fatalf("version add under a race: %v", err)
		}
		if !raced.Load() {
			t.Fatal("premise: the second writer must have raced core-provider's Update")
		}
		got := read(v1.Name)
		if !served(got, "v1-1-0") {
			t.Fatalf("the version add must land after the conflict; served versions: %v", declaredVersions(got))
		}
		if got.GetAnnotations()[thirdPartyKey] != "raced" {
			t.Fatalf("the racing writer's annotation was lost: annotations now %v", got.GetAnnotations())
		}
	})

	t.Run("control: core-provider's own change still lands when nobody else writes", func(t *testing.T) {
		v1, v2 := gen("Doohickey", "v1-0-0"), gen("Doohickey", "v1-1-0")
		if _, err := ApplyOrUpdateCRD(ctx, cl, dyn, v1); err != nil {
			t.Fatalf("setup: %v", err)
		}
		if _, err := ApplyOrUpdateCRD(ctx, cl, dyn, v2); err != nil {
			t.Fatalf("version add: %v", err)
		}
		if got := read(v1.Name); !served(got, "v1-1-0") {
			t.Fatalf("served versions: %v", declaredVersions(got))
		}
	})
}

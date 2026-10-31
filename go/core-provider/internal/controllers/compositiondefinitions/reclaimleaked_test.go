package compositiondefinitions

import (
	"context"
	"strings"
	"testing"

	"github.com/krateo-platformops/core-provider/apis"
	appsv1 "k8s.io/api/apps/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// #270. A retirement that ran while 2.13.45's drain wedge was live aborted Undeploy partway and
// left the per-version controller running. Observed on 057: installers-v0-3-480-controller at
// image 2.13.45, two installer versions after it should have gone, 1/1 ready and inert — its CRD
// version no longer existed so its informer watched nothing.
//
// The whole risk in reclaiming these is deleting one that is still CLAIMED. Every arm below except
// the first is a keep-reason, deliberately: this predicate's job is to refuse.

const (
	leakNS       = "krateo-system"
	leakResource = "installers"
	leakGroup    = "composition.krateo.io"
)

func leakGVK(v string) schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: leakGroup, Kind: "Installer", Version: v}
}
func leakGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: leakGroup, Version: "v0-3-484", Resource: leakResource}
}

func leakCRD(versions ...string) *apiextensionsv1.CustomResourceDefinition {
	crd := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: leakResource + "." + leakGroup},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: leakGroup,
			Names: apiextensionsv1.CustomResourceDefinitionNames{Kind: "Installer", Plural: leakResource},
		},
	}
	for i, v := range versions {
		crd.Spec.Versions = append(crd.Spec.Versions, apiextensionsv1.CustomResourceDefinitionVersion{
			Name: v, Served: v != "vacuum", Storage: i == 0,
		})
	}
	return crd
}

func leakDeploy(name string) *appsv1.Deployment {
	return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: leakNS}}
}

func leakComposition(name, versionLabel string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": leakGroup + "/v0-3-484", "kind": "Installer",
		"metadata": map[string]any{"name": name, "namespace": leakNS},
	}}
	if versionLabel != "" {
		u.SetLabels(map[string]string{"krateo.io/composition-version": versionLabel})
	}
	return u
}

// leakScheme registers what THIS test puts in the fake client. The package's shared scheme(t) does
// not include apps/v1, so using it panics on the first Deployment — which is the only reason the
// subject arm failed on its first run rather than the predicate being wrong.
func leakScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := appsv1.AddToScheme(s); err != nil {
		t.Fatalf("appsv1: %v", err)
	}
	if err := apiextensionsv1.AddToScheme(s); err != nil {
		t.Fatalf("apiextensionsv1: %v", err)
	}
	// The keep-reason check lists CompositionDefinitions, so its own API group must be registered
	// too — otherwise the predicate errors out before it can find anything, which is how the
	// SUBJECT arm failed while all six refusals passed.
	if err := apis.AddToScheme(s); err != nil {
		t.Fatalf("core-provider apis: %v", err)
	}
	return s
}

func leakExternal(t *testing.T, crd *apiextensionsv1.CustomResourceDefinition, deps []*appsv1.Deployment, comps ...*unstructured.Unstructured) *external {
	t.Helper()
	b := clientfake.NewClientBuilder().WithScheme(leakScheme(t))
	if crd != nil {
		b = b.WithObjects(crd)
	}
	for _, d := range deps {
		b = b.WithObjects(d)
	}

	dynObjs := make([]runtime.Object, 0, len(comps))
	for _, c := range comps {
		dynObjs = append(dynObjs, c)
	}
	gvrToKind := map[schema.GroupVersionResource]string{leakGVR(): "InstallerList"}

	kc := b.Build()
	return &external{
		kube: kc,
		// versionReferencedByAnotherDefinition lists CompositionDefinitions through e.mgmt. Leaving
		// it nil panics inside the keep-reason check rather than failing the predicate, which is how
		// the subject arm failed on its first two runs.
		mgmt:    kc,
		dynamic: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), gvrToKind, dynObjs...),
	}
}

func TestFindLeakedControllers(t *testing.T) {
	// THE SUBJECT, and it is 057's exact state: the CRD carries only the current version plus a
	// non-served vacuum, one composition sits at the current version, and a controller for a
	// version the CRD no longer knows is still running.
	t.Run("a controller for a version absent from the CRD and unclaimed is leaked", func(t *testing.T) {
		e := leakExternal(t,
			leakCRD("v0-3-484", "vacuum"),
			[]*appsv1.Deployment{
				leakDeploy("installers-v0-3-484-controller"),
				leakDeploy("installers-v0-3-480-controller"),
			},
			leakComposition("installer", "v0-3-484"),
		)

		leaked, kept, err := e.findLeakedControllers(context.Background(), leakGVK("v0-3-484"), leakGVR(), "installer", leakNS)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(leaked) != 1 || leaked[0].Version != "v0-3-480" {
			t.Fatalf("leaked = %+v, want exactly installers-v0-3-480-controller (057's real leak); kept=%v", leaked, kept)
		}
		if leaked[0].Name != "installers-v0-3-480-controller" {
			t.Errorf("name = %q, want 057's real deployment name", leaked[0].Name)
		}
	})

	// KEEP-REASON 1. The current version's controller must never be a candidate.
	t.Run("the CURRENT version is never leaked", func(t *testing.T) {
		e := leakExternal(t, leakCRD("v0-3-484"),
			[]*appsv1.Deployment{leakDeploy("installers-v0-3-484-controller")},
			leakComposition("installer", "v0-3-484"))
		leaked, kept, err := e.findLeakedControllers(context.Background(), leakGVK("v0-3-484"), leakGVR(), "installer", leakNS)
		if err != nil || len(leaked) != 0 {
			t.Fatalf("leaked = %+v (err %v), want none; kept=%v", leaked, err, kept)
		}
		if !strings.Contains(strings.Join(kept, ","), "is-current") {
			t.Errorf("kept = %v, want an is-current reason", kept)
		}
	})

	// KEEP-REASON 2. A non-served version is still STORED data and its controller may be needed
	// for a conversion. Presence in spec.versions is the test, not servedness.
	t.Run("a version still on the CRD is kept even if NOT served", func(t *testing.T) {
		e := leakExternal(t, leakCRD("v0-3-484", "vacuum"),
			[]*appsv1.Deployment{leakDeploy("installers-vacuum-controller")},
			leakComposition("installer", "v0-3-484"))
		leaked, kept, err := e.findLeakedControllers(context.Background(), leakGVK("v0-3-484"), leakGVR(), "installer", leakNS)
		if err != nil || len(leaked) != 0 {
			t.Fatalf("leaked = %+v (err %v), want none — a non-served version is still stored data; kept=%v", leaked, err, kept)
		}
	})

	// THE #189 GAP, and the arm is deliberately renamed. It originally claimed to test a
	// per-version keep-reason; a surviving mutant showed the blanket refusal catches this case
	// first, because a label naming a version absent from the CRD IS unaccounted by definition.
	// The separate check was unreachable and has been removed, so this arm asserts what actually
	// protects the instance.
	t.Run("an instance labelled at a version gone from the CRD blocks all reclamation", func(t *testing.T) {
		e := leakExternal(t, leakCRD("v0-3-484"),
			[]*appsv1.Deployment{leakDeploy("installers-v0-3-480-controller")},
			leakComposition("installer", "v0-3-484"),
			leakComposition("legacy", "v0-3-480"))
		leaked, kept, err := e.findLeakedControllers(context.Background(), leakGVK("v0-3-484"), leakGVR(), "installer", leakNS)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(leaked) != 0 {
			t.Fatalf("leaked = %+v, want NONE: an instance carries v0-3-480's label, which makes it "+
				"unaccounted, which must stop all reclamation (#189); kept=%v", leaked, kept)
		}
		if !strings.Contains(strings.Join(kept, ","), "unaccounted-instances") {
			t.Errorf("kept = %v, want the blanket reason — if this ever reports a per-version "+
				"reason instead, the unreachable check has been reintroduced", kept)
		}
	})

	// KEEP-REASON 4, the blanket refusal. An instance with NO version label is invisible to every
	// label-keyed check here, so nothing may be reclaimed while one exists.
	t.Run("an UNLABELLED composition blocks all reclamation", func(t *testing.T) {
		e := leakExternal(t, leakCRD("v0-3-484"),
			[]*appsv1.Deployment{leakDeploy("installers-v0-3-480-controller")},
			leakComposition("installer", "v0-3-484"),
			leakComposition("mystery", ""))
		leaked, kept, err := e.findLeakedControllers(context.Background(), leakGVK("v0-3-484"), leakGVR(), "installer", leakNS)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(leaked) != 0 {
			t.Fatalf("leaked = %+v, want NONE: an unlabelled instance could depend on any of these "+
				"controllers and is invisible to every check; kept=%v", leaked, kept)
		}
		if !strings.Contains(strings.Join(kept, ","), "unaccounted-instances") {
			t.Errorf("kept = %v, want the blanket unaccounted-instances reason", kept)
		}
	})

	// KEEP-REASON 5. No CRD means no reliable notion of which versions exist. Treating every
	// controller as leaked would delete the whole fleet for this Kind.
	t.Run("a missing CRD reclaims NOTHING", func(t *testing.T) {
		e := leakExternal(t, nil,
			[]*appsv1.Deployment{leakDeploy("installers-v0-3-480-controller")})
		leaked, kept, err := e.findLeakedControllers(context.Background(), leakGVK("v0-3-484"), leakGVR(), "installer", leakNS)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(leaked) != 0 {
			t.Fatalf("leaked = %+v, want none with no CRD — that would delete the fleet; kept=%v", leaked, kept)
		}
	})

	// A Deployment that is not a per-version controller for THIS Kind must never match.
	t.Run("unrelated deployments are never candidates", func(t *testing.T) {
		e := leakExternal(t, leakCRD("v0-3-484"),
			[]*appsv1.Deployment{
				leakDeploy("krateo-core-provider"),
				leakDeploy("fireworks-v1-0-0-controller"),
				leakDeploy("installers-v0-3-484-controller"),
			},
			leakComposition("installer", "v0-3-484"))
		leaked, _, err := e.findLeakedControllers(context.Background(), leakGVK("v0-3-484"), leakGVR(), "installer", leakNS)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(leaked) != 0 {
			t.Errorf("leaked = %+v; a controller for another Kind, and core-provider itself, must "+
				"never be candidates", leaked)
		}
	})
}

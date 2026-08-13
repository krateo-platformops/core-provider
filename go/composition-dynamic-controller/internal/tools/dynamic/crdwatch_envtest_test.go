//go:build envtest

// Functional (real-apiserver) test of core-provider's ACTUAL cache-staleness mechanism:
// WatchCRDsAndInvalidate keeping the long-lived DeferredDiscoveryRESTMapper (NewRESTMapper) fresh.
//
// This is the honest reproduction of the "umbrella stops emitting Compositions until the cdc is
// restarted" symptom and its fix. It uses the REAL production code paths (NewRESTMapper +
// WatchCRDsAndInvalidate), registers a CRD MID-RUN against a live kube-apiserver, and proves:
//   - WATCHED mapper: after the CRD is created, the mapper resolves the new kind on its own within the
//     coalescing window — NO process restart (the fix works end-to-end).
//   - UNWATCHED mapper (negative control): the same mid-run CRD stays invisible (stale) until an
//     explicit Reset() — i.e. exactly what a controller restart used to do by hand.
//
// Run: KUBEBUILDER_ASSETS=$(setup-envtest use -p path 1.36.0) \
//        go test -tags envtest ./internal/tools/dynamic/ -run Envtest -v
package dynamic

import (
	"context"
	"os"
	"testing"
	"time"

	apixv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func crdwatchBoolPtr(b bool) *bool { return &b }

// mkCompositionCRD builds a composition.krateo.io/v1-0-0 CRD (the shape the umbrella's inst.crdExists
// gate and the cdc's GVKtoGVR/IsNamespaced both depend on resolving).
func mkCompositionCRD(plural, kind string) *apixv1.CustomResourceDefinition {
	return &apixv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: plural + ".composition.krateo.io"},
		Spec: apixv1.CustomResourceDefinitionSpec{
			Group: "composition.krateo.io",
			Names: apixv1.CustomResourceDefinitionNames{
				Plural:   plural,
				Singular: plural[:len(plural)-1],
				Kind:     kind,
				ListKind: kind + "List",
			},
			Scope: apixv1.NamespaceScoped,
			Versions: []apixv1.CustomResourceDefinitionVersion{
				{
					Name:    "v1-0-0",
					Served:  true,
					Storage: true,
					Schema: &apixv1.CustomResourceValidation{
						OpenAPIV3Schema: &apixv1.JSONSchemaProps{
							Type: "object",
							Properties: map[string]apixv1.JSONSchemaProps{
								"spec": {Type: "object", XPreserveUnknownFields: crdwatchBoolPtr(true)},
							},
						},
					},
				},
			},
		},
	}
}

func gkResolves(mapper meta.RESTMapper, gk schema.GroupKind) bool {
	_, err := mapper.RESTMapping(gk, "v1-0-0")
	return err == nil
}

func pollResolves(mapper meta.RESTMapper, gk schema.GroupKind, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if gkResolves(mapper, gk) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(150 * time.Millisecond)
	}
}

func waitCRDEstablished(t *testing.T, ac apiextclient.Interface, name string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		crd, err := ac.ApiextensionsV1().CustomResourceDefinitions().Get(context.Background(), name, metav1.GetOptions{})
		if err == nil {
			for _, c := range crd.Status.Conditions {
				if c.Type == apixv1.Established && c.Status == apixv1.ConditionTrue {
					return
				}
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("CRD %s never became Established", name)
}

// TestEnvtest_WatchCRDsAndInvalidate_Functional is the gold-standard proof. One apiserver, two REAL
// production mappers over it: one guarded by WatchCRDsAndInvalidate (the fix), one bare (the pre-fix
// behavior). Both are warmed before the CRD exists, then a single mid-run CRD create is observed
// against both — the watched mapper self-heals without a restart; the unwatched one stays stale until
// an explicit Reset (what a restart did by hand).
func TestEnvtest_WatchCRDsAndInvalidate_Functional(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via setup-envtest")
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest apiserver: %v", err)
	}
	defer func() { _ = env.Stop() }()

	ac, err := apiextclient.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("apiextensions client: %v", err)
	}

	// REAL production mappers (DeferredDiscoveryRESTMapper over a MemCache discovery client).
	watched, err := NewRESTMapper(cfg)
	if err != nil {
		t.Fatalf("NewRESTMapper (watched): %v", err)
	}
	unwatched, err := NewRESTMapper(cfg)
	if err != nil {
		t.Fatalf("NewRESTMapper (unwatched): %v", err)
	}

	// Guard only `watched` with the REAL watch.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := WatchCRDsAndInvalidate(ctx, cfg, watched, nil); err != nil {
		t.Fatalf("WatchCRDsAndInvalidate: %v", err)
	}
	// Let the CRD informer establish its initial list/watch before we mutate.
	time.Sleep(1 * time.Second)

	gk := schema.GroupKind{Group: "composition.krateo.io", Kind: "Widget"}

	// Warm BOTH mappers' discovery caches while the kind is absent (populates the stale delegate).
	if gkResolves(watched, gk) || gkResolves(unwatched, gk) {
		t.Fatalf("precondition: Widget must not resolve before its CRD exists")
	}

	// Register the CRD MID-RUN — no restart of anything.
	if _, err := ac.ApiextensionsV1().CustomResourceDefinitions().Create(context.Background(), mkCompositionCRD("widgets", "Widget"), metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create Widget CRD: %v", err)
	}
	waitCRDEstablished(t, ac, "widgets.composition.krateo.io")

	// FIX: the watched mapper must resolve the new kind on its own, within the 2s coalescing window
	// (plus generous margin for informer delivery) — WITHOUT a restart.
	if !pollResolves(watched, gk, 12*time.Second) {
		t.Fatalf("WATCHED mapper never picked up the mid-run CRD; WatchCRDsAndInvalidate did NOT refresh discovery")
	}
	t.Logf("WATCHED mapper resolved Widget after a mid-run CRD create with NO restart (fix works)")

	// NEGATIVE CONTROL: the unwatched mapper is still stale (its MemCache delegate was warmed empty and
	// nothing invalidated it). This reproduces the original "restart required" symptom.
	if gkResolves(unwatched, gk) {
		t.Fatalf("UNWATCHED mapper unexpectedly self-refreshed; the negative control is invalid")
	}
	t.Logf("UNWATCHED mapper is still stale (Widget unresolved) — reproduces the pre-fix 'restart required' behavior")

	// A manual Reset (what a process restart effectively forced) recovers the unwatched mapper.
	resetter, ok := unwatched.(interface{ Reset() })
	if !ok {
		t.Fatalf("production mapper does not implement Reset(); WatchCRDsAndInvalidate would be a no-op")
	}
	resetter.Reset()
	if !pollResolves(unwatched, gk, 8*time.Second) {
		t.Fatalf("after an explicit Reset the unwatched mapper still cannot resolve Widget")
	}
	t.Logf("UNWATCHED mapper resolved Widget only after an explicit Reset() (the manual-restart equivalent)")
}

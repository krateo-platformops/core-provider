package deploy

import (
	"context"
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
)

var guardGVR = schema.GroupVersionResource{
	Group:    "composition.krateo.io",
	Version:  "v1-0-0",
	Resource: "fireworksapps",
}

func newInstance(name, namespace string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   guardGVR.Group,
		Version: guardGVR.Version,
		Kind:    "FireworksApp",
	})
	u.SetName(name)
	u.SetNamespace(namespace)
	return u
}

func dynWith(objs ...runtime.Object) *fake.FakeDynamicClient {
	return fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{guardGVR: "FireworksAppList"},
		objs...,
	)
}

// When a teardown that would delete the CRD (!SkipCRD) runs while a composition instance
// still exists — even one WITHOUT the version label — Undeploy must refuse with
// ErrCompositionStillExist before tearing anything down, rather than cascade-deleting it.
func TestUndeploy_RefusesWhenUnlabeledInstanceExists(t *testing.T) {
	dyn := dynWith(newInstance("orphan", "default")) // no krateo.io/composition-version label

	err := Undeploy(context.Background(), nil, UndeployOptions{
		ControllerNamespace: "test-ns",
		GVR:                 guardGVR,
		DynamicClient:       dyn,
		SkipCRD:             false,
	})

	if !errors.Is(err, ErrCompositionStillExist) {
		t.Fatalf("expected ErrCompositionStillExist, got %v", err)
	}
}

// With SkipCRD=true (e.g. removing an old version's CDC while the shared CRD stays) the
// guard must not trip even if instances exist — the CRD is not being deleted.
func TestUndeploy_SkipCRDBypassesGuard(t *testing.T) {
	dyn := dynWith(newInstance("live", "default"))

	err := Undeploy(context.Background(), nil, UndeployOptions{
		ControllerNamespace: "test-ns",
		GVR:                 guardGVR,
		DynamicClient:       dyn,
		SkipCRD:             true,
		// no template paths: this fails later, but it must NOT be the still-exist guard.
	})

	if errors.Is(err, ErrCompositionStillExist) {
		t.Fatalf("guard must be bypassed when SkipCRD=true, got %v", err)
	}
}

// With no instances present the guard passes; Undeploy then proceeds (and fails later on
// the missing template paths) — the point is it does NOT return the still-exist sentinel.
func TestUndeploy_NoInstancesPassesGuard(t *testing.T) {
	dyn := dynWith()

	err := Undeploy(context.Background(), nil, UndeployOptions{
		ControllerNamespace: "test-ns",
		GVR:                 guardGVR,
		DynamicClient:       dyn,
		SkipCRD:             false,
	})

	if errors.Is(err, ErrCompositionStillExist) {
		t.Fatalf("guard should pass with zero instances, got still-exist sentinel: %v", err)
	}
}

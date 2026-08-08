package composition

import (
	"context"
	"fmt"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

// ---- pure classifier tests (no client) --------------------------------------------------------

func krateoCR(readyStatus, reason string, hasConds bool) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "composition.krateo.io/v0-1-0", "kind": "Portal",
		"metadata": map[string]any{"name": "p", "namespace": "ns"},
	}}
	if hasConds {
		_ = unstructured.SetNestedSlice(u.Object, []any{
			map[string]any{"type": "Ready", "status": readyStatus, "reason": reason},
		}, "status", "conditions")
	}
	return u
}

func TestClassifyChild_Krateo(t *testing.T) {
	cases := []struct {
		name   string
		obj    *unstructured.Unstructured
		expect childState
	}{
		{"ready-true", krateoCR("True", "Available", true), childHealthy},
		{"ready-false-unavailable", krateoCR("False", "Unavailable", true), childFailed},
		{"ready-false-creating", krateoCR("False", "Creating", true), childConverging},
		{"no-conditions", krateoCR("", "", false), childConverging},
	}
	for _, c := range cases {
		if got := classifyChild("composition.krateo.io", c.obj); got != c.expect {
			t.Errorf("%s: got %d want %d", c.name, got, c.expect)
		}
	}
}

func TestClassifyChild_Workloads(t *testing.T) {
	dep := func(ready, desired int64) *unstructured.Unstructured {
		u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "apps/v1", "kind": "Deployment"}}
		_ = unstructured.SetNestedField(u.Object, desired, "spec", "replicas")
		_ = unstructured.SetNestedField(u.Object, ready, "status", "readyReplicas")
		return u
	}
	if got := classifyChild("apps", dep(3, 3)); got != childHealthy {
		t.Errorf("deployment 3/3: got %d want healthy", got)
	}
	if got := classifyChild("apps", dep(1, 3)); got != childConverging {
		t.Errorf("deployment 1/3: got %d want converging", got)
	}
	// DaemonSet path
	ds := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "apps/v1", "kind": "DaemonSet"}}
	_ = unstructured.SetNestedField(ds.Object, int64(2), "status", "desiredNumberScheduled")
	_ = unstructured.SetNestedField(ds.Object, int64(2), "status", "numberReady")
	if got := classifyChild("apps", ds); got != childHealthy {
		t.Errorf("daemonset 2/2: got %d want healthy", got)
	}
}

func TestClassifyChild_Job(t *testing.T) {
	job := func(succeeded, failed, backoff int64) *unstructured.Unstructured {
		u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "batch/v1", "kind": "Job"}}
		_ = unstructured.SetNestedField(u.Object, backoff, "spec", "backoffLimit")
		_ = unstructured.SetNestedField(u.Object, succeeded, "status", "succeeded")
		_ = unstructured.SetNestedField(u.Object, failed, "status", "failed")
		return u
	}
	if got := classifyChild("batch", job(1, 0, 3)); got != childHealthy {
		t.Errorf("job succeeded: got %d want healthy", got)
	}
	if got := classifyChild("batch", job(0, 5, 3)); got != childFailed {
		t.Errorf("job failed>backoff: got %d want failed", got)
	}
	if got := classifyChild("batch", job(0, 0, 3)); got != childConverging {
		t.Errorf("job pending: got %d want converging", got)
	}
}

func TestClassifyChild_DefaultExistenceOnly(t *testing.T) {
	cm := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "ConfigMap"}}
	if got := classifyChild("", cm); got != childHealthy {
		t.Errorf("configmap present: got %d want healthy", got)
	}
}

// ---- childHealth GET paths (fake client) — fail-safe is load-bearing --------------------------

func chGVR(g, v, r string) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: g, Version: v, Resource: r}
}

func fakeDynWith(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	scheme := runtime.NewScheme()
	gvrToListKind := map[schema.GroupVersionResource]string{
		chGVR("apps", "v1", "deployments"):                  "DeploymentList",
		chGVR("composition.krateo.io", "v0-1-0", "portals"): "PortalList",
		chGVR("", "v1", "configmaps"):                       "ConfigMapList",
	}
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, gvrToListKind, objs...)
}

func TestChildHealth_NotFoundIsConverging(t *testing.T) {
	h := &handler{}
	ref := map[string]any{"apiVersion": "apps/v1", "resource": "deployments", "name": "absent", "namespace": "ns"}
	if got := h.childHealth(context.Background(), fakeDynWith(), ref); got != childConverging {
		t.Errorf("NotFound: got %d want converging", got)
	}
}

func TestChildHealth_ForbiddenIsHealthy(t *testing.T) {
	h := &handler{}
	dyn := fakeDynWith()
	dyn.PrependReactor("get", "deployments", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "deployments"}, "x", fmt.Errorf("no access"))
	})
	ref := map[string]any{"apiVersion": "apps/v1", "resource": "deployments", "name": "x", "namespace": "ns"}
	if got := h.childHealth(context.Background(), dyn, ref); got != childHealthy {
		t.Errorf("Forbidden must degrade to healthy: got %d", got)
	}
}

// ---- rollupManagedChildren aggregation --------------------------------------------------------

func setManaged(mg *unstructured.Unstructured, refs ...map[string]any) {
	items := make([]any, len(refs))
	for i, r := range refs {
		items[i] = r
	}
	_ = unstructured.SetNestedSlice(mg.Object, items, "status", "managed")
}

func TestRollup_AllHealthy(t *testing.T) {
	h := &handler{}
	cm := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": "c", "namespace": "ns"}}}
	mg := &unstructured.Unstructured{Object: map[string]any{}}
	setManaged(mg, map[string]any{"apiVersion": "v1", "resource": "configmaps", "name": "c", "namespace": "ns"})
	v := h.rollupManagedChildren(context.Background(), fakeDynWith(cm), mg)
	if !v.ready || v.reason != "Available" {
		t.Fatalf("all-healthy: got ready=%v reason=%s", v.ready, v.reason)
	}
}

func TestRollup_OneFailed(t *testing.T) {
	h := &handler{}
	bad := krateoCR("False", "Unavailable", true)
	bad.SetName("db")
	_ = unstructured.SetNestedField(bad.Object, "db", "metadata", "name")
	_ = unstructured.SetNestedField(bad.Object, "ns", "metadata", "namespace")
	mg := &unstructured.Unstructured{Object: map[string]any{}}
	setManaged(mg, map[string]any{"apiVersion": "composition.krateo.io/v0-1-0", "resource": "portals", "name": "db", "namespace": "ns"})
	v := h.rollupManagedChildren(context.Background(), fakeDynWith(bad), mg)
	if v.ready || v.reason != "Unavailable" || len(v.failing) != 1 {
		t.Fatalf("one-failed: got ready=%v reason=%s failing=%v", v.ready, v.reason, v.failing)
	}
}

func TestRollup_Converging(t *testing.T) {
	h := &handler{}
	mg := &unstructured.Unstructured{Object: map[string]any{}}
	// child not seeded => NotFound => converging (no failures)
	setManaged(mg, map[string]any{"apiVersion": "apps/v1", "resource": "deployments", "name": "web", "namespace": "ns"})
	v := h.rollupManagedChildren(context.Background(), fakeDynWith(), mg)
	if v.ready || v.reason != "Creating" {
		t.Fatalf("converging: got ready=%v reason=%s", v.ready, v.reason)
	}
}

func TestRollup_Empty(t *testing.T) {
	h := &handler{}
	mg := &unstructured.Unstructured{Object: map[string]any{}}
	v := h.rollupManagedChildren(context.Background(), fakeDynWith(), mg)
	if !v.ready {
		t.Fatalf("empty managed: got ready=%v", v.ready)
	}
}

// ---- readProjectedHealth ----------------------------------------------------------------------

func TestReadProjectedHealth(t *testing.T) {
	mkBool := func(b bool) *unstructured.Unstructured {
		u := &unstructured.Unstructured{Object: map[string]any{}}
		_ = unstructured.SetNestedField(u.Object, b, "status", "health", "ready")
		return u
	}
	mkStr := func(s string) *unstructured.Unstructured {
		u := &unstructured.Unstructured{Object: map[string]any{}}
		_ = unstructured.SetNestedField(u.Object, s, "status", "health", "ready")
		return u
	}
	for _, c := range []struct {
		name           string
		mg             *unstructured.Unstructured
		ready, present bool
	}{
		{"bool-true", mkBool(true), true, true},
		{"bool-false", mkBool(false), false, true},
		{"str-true", mkStr("true"), true, true},
		{"str-false", mkStr("no"), false, true},
		{"absent", &unstructured.Unstructured{Object: map[string]any{}}, false, false},
	} {
		gotReady, gotPresent := readProjectedHealth(c.mg)
		if gotReady != c.ready || gotPresent != c.present {
			t.Errorf("%s: got (ready=%v present=%v) want (%v %v)", c.name, gotReady, gotPresent, c.ready, c.present)
		}
	}
}

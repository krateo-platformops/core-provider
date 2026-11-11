//go:build envtest

// The scale-to-zero reconciler (#393) against a real apiserver: the helper's scale subresource GET
// and PATCH and its metadata-only, paginated LIST are apiserver behaviour, and a fake client would
// verify only that the fake agrees with this code.
//
//	Run: KUBEBUILDER_ASSETS=$(setup-envtest use 1.36.0 -p path) \
//	go test -tags envtest ./internal/controllers/compositiondefinitions/ -run ScaleToZeroEnvtest -v
package compositiondefinitions

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/krateo-platformops/core-provider/apis"
	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/tools/deploy"
	"github.com/krateo-platformops/plumbing/labels"
	rtv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	stzNS      = "krateo-system"
	stzVersion = "v1-0-0"
)

var stzGVR = schema.GroupVersionResource{Group: "composition.krateo.io", Version: stzVersion, Resource: "fireworksapps"}
var stzGVK = schema.GroupVersionKind{Group: "composition.krateo.io", Version: stzVersion, Kind: "FireworksApp"}

func stzCompositionCRD() *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "fireworksapps.composition.krateo.io"},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "composition.krateo.io", Scope: apiextensionsv1.NamespaceScoped,
			Names: apiextensionsv1.CustomResourceDefinitionNames{Plural: "fireworksapps", Singular: "fireworksapp", Kind: "FireworksApp", ListKind: "FireworksAppList"},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{Name: stzVersion, Served: true, Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
					Type: "object", XPreserveUnknownFields: ptr.To(true)}}}},
		},
	}
}

type stzEnv struct {
	cfg  *envtest.Environment
	kube client.Client
	r    *scaleToZeroReconciler
}

func startStzEnv(t *testing.T) *stzEnv {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Fatal("KUBEBUILDER_ASSETS is not set: this suite exists to run against a real apiserver, and a skip would read as a pass")
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "crds")},
		CRDs:                  []*apiextensionsv1.CustomResourceDefinition{stzCompositionCRD()},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	sc := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(sc)
	_ = apis.AddToScheme(sc)
	kube, err := client.New(cfg, client.Options{Scheme: sc})
	if err != nil {
		t.Fatal(err)
	}
	if err := kube.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: stzNS}}); err != nil {
		t.Fatal(err)
	}
	ManagementNamespace = stzNS
	return &stzEnv{cfg: env, kube: kube, r: &scaleToZeroReconciler{
		kube: kube, live: kube, log: logging.NewNopLogger(), now: time.Now,
		active: func(schema.GroupVersionResource) (int32, error) { return 1, nil },
	}}
}

// controllerDeployment creates the definition's controller Deployment at the given replicas.
func (e *stzEnv) controllerDeployment(t *testing.T, replicas int32) types.NamespacedName {
	t.Helper()
	key := deploy.ControllerDeploymentKey(stzNS, stzGVR)
	sel := map[string]string{"app": key.Name}
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name},
		Spec: appsv1.DeploymentSpec{Replicas: &replicas, Selector: &metav1.LabelSelector{MatchLabels: sel},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: sel},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "busybox"}}}}}}
	if err := e.kube.Create(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	return key
}

func (e *stzEnv) replicas(t *testing.T, key types.NamespacedName) int32 {
	t.Helper()
	s := &autoscalingv1.Scale{}
	if err := e.kube.SubResource("scale").Get(context.Background(), &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}}, s); err != nil {
		t.Fatal(err)
	}
	return s.Spec.Replicas
}

// definition creates a deployed (Ready, GVK recorded) definition. created orders siblings: the
// oldest owns the shared controller.
func (e *stzEnv) definition(t *testing.T, name string, optIn *bool, holdDown string, lastSeen *time.Time) *compositiondefinitionsv1alpha1.CompositionDefinition {
	t.Helper()
	ctx := context.Background()
	cd := &compositiondefinitionsv1alpha1.CompositionDefinition{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name}}
	cd.Spec.Chart = &compositiondefinitionsv1alpha1.ChartInfo{Url: "oci://example/charts/fireworks", Version: "1.0.0"}
	if optIn != nil {
		cd.Spec.Controller = &compositiondefinitionsv1alpha1.ControllerConfig{ScaleToZero: optIn}
		if holdDown != "" {
			cd.Spec.Controller.ScaleToZeroHoldDown = &holdDown
		}
	}
	if err := e.kube.Create(ctx, cd); err != nil {
		t.Fatal(err)
	}
	cd.Status.ApiVersion, cd.Status.Kind, cd.Status.Resource = stzGVK.GroupVersion().String(), stzGVK.Kind, stzGVR.Resource
	cd.SetConditions(rtv1.Available())
	if lastSeen != nil {
		ls := metav1.NewTime(*lastSeen)
		cd.Status.ScaleToZero = &compositiondefinitionsv1alpha1.ScaleToZeroStatus{LastSeen: &ls}
	}
	if err := e.kube.Status().Update(ctx, cd); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond) // creationTimestamp has one-second resolution: order siblings
	return cd
}

func (e *stzEnv) instance(t *testing.T, name string) {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(stzGVK)
	u.SetNamespace("default")
	u.SetName(name)
	u.SetLabels(map[string]string{labels.CompositionVersionLabel: stzVersion})
	if err := e.kube.Create(context.Background(), u); err != nil {
		t.Fatal(err)
	}
}

func (e *stzEnv) reconcile(t *testing.T, cd *compositiondefinitionsv1alpha1.CompositionDefinition) (reconcile.Result, *compositiondefinitionsv1alpha1.ScaleToZeroStatus) {
	t.Helper()
	// The wake watch needs a running manager; it is covered by the manager test below.
	e.r.watched.Store(stzGVK, struct{}{})
	res, err := e.r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Namespace: cd.Namespace, Name: cd.Name}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := &compositiondefinitionsv1alpha1.CompositionDefinition{}
	if err := e.kube.Get(context.Background(), client.ObjectKeyFromObject(cd), got); err != nil {
		t.Fatal(err)
	}
	return res, got.Status.ScaleToZero
}

// Each case runs in its own apiserver: the controller Deployment name is derived from the GVR, so
// cases sharing one would see each other's Deployment.
func TestScaleToZeroEnvtest(t *testing.T) {
	yes, no := true, false
	ago := func(d time.Duration) *time.Time { x := time.Now().Add(-d); return &x }

	t.Run("an empty Kind seen for the first time starts the hold-down and keeps the controller up", func(t *testing.T) {
		e := startStzEnv(t)
		key := e.controllerDeployment(t, 1)
		cd := e.definition(t, "a", &yes, "10m", nil)
		res, st := e.reconcile(t, cd)
		if r := e.replicas(t, key); r != 1 || st == nil || st.Reason != "IdleHoldDown" || st.LastSeen == nil {
			t.Fatalf("replicas=%d status=%+v; want 1, IdleHoldDown, lastSeen set", r, st)
		}
		if res.RequeueAfter <= 9*time.Minute || res.RequeueAfter > 10*time.Minute {
			t.Errorf("RequeueAfter=%s, want the rest of a 10m hold-down", res.RequeueAfter)
		}
	})

	t.Run("an empty Kind past its hold-down is scaled to zero", func(t *testing.T) {
		e := startStzEnv(t)
		key := e.controllerDeployment(t, 1)
		cd := e.definition(t, "a", &yes, "10m", ago(20*time.Minute))
		_, st := e.reconcile(t, cd)
		if r := e.replicas(t, key); r != 0 || st == nil || st.Reason != "Idle" || st.Replicas == nil || *st.Replicas != 0 || st.LastSeen == nil {
			t.Fatalf("replicas=%d status=%+v; want 0, Idle, replicas 0, lastSeen kept", r, st)
		}
	})

	t.Run("an instance wakes a zeroed controller, and lastSeen is NOT stored while instances exist", func(t *testing.T) {
		e := startStzEnv(t)
		key := e.controllerDeployment(t, 0)
		cd := e.definition(t, "a", &yes, "10m", ago(20*time.Minute))
		e.instance(t, "one")
		_, st := e.reconcile(t, cd)
		if r := e.replicas(t, key); r != 1 || st == nil || st.Reason != "Active" {
			t.Fatalf("replicas=%d status=%+v; want woken to 1, Active", r, st)
		}
		// The hazard this rule closes: a stored "now" ages while no event comes, and the delete of
		// the last instance would then scale down AT ONCE, skipping the hold-down.
		if st.LastSeen != nil {
			t.Errorf("lastSeen=%v stored while instances exist; want none, so the first empty pass starts a full hold-down", st.LastSeen)
		}
	})

	t.Run("the last instance's deletion starts a FULL hold-down, not an immediate scale-down", func(t *testing.T) {
		e := startStzEnv(t)
		key := e.controllerDeployment(t, 1)
		cd := e.definition(t, "a", &yes, "10m", nil)
		e.instance(t, "one")
		if _, st := e.reconcile(t, cd); st == nil || st.Reason != "Active" {
			t.Fatalf("premise: Active, got %+v", st)
		}
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(stzGVK)
		u.SetNamespace("default")
		u.SetName("one")
		if err := e.kube.Delete(context.Background(), u); err != nil {
			t.Fatal(err)
		}
		res, st := e.reconcile(t, cd)
		if r := e.replicas(t, key); r != 1 || st == nil || st.Reason != "IdleHoldDown" {
			t.Fatalf("replicas=%d status=%+v; want still 1 in a fresh hold-down", r, st)
		}
		if res.RequeueAfter <= 9*time.Minute {
			t.Errorf("RequeueAfter=%s, want close to the full 10m: the hold-down must start at the deletion", res.RequeueAfter)
		}
	})

	t.Run("a sibling that has not opted in keeps the shared controller up", func(t *testing.T) {
		e := startStzEnv(t)
		key := e.controllerDeployment(t, 1)
		owner := e.definition(t, "a-owner", &yes, "10m", ago(20*time.Minute))
		e.definition(t, "b-sibling", &no, "", nil)
		_, st := e.reconcile(t, owner)
		if r := e.replicas(t, key); r != 1 {
			t.Fatalf("replicas=%d, want the shared controller kept at 1", r)
		}
		if st == nil || len(st.HeldBy) != 1 || st.HeldBy[0] != "default/b-sibling" || st.LastSeen != nil {
			t.Errorf("status=%+v; want heldBy [default/b-sibling] and no stored lastSeen", st)
		}
	})

	t.Run("a held controller found at zero is woken", func(t *testing.T) {
		e := startStzEnv(t)
		key := e.controllerDeployment(t, 0)
		owner := e.definition(t, "a-owner", &yes, "10m", ago(20*time.Minute))
		e.definition(t, "b-sibling", nil, "", nil)
		e.reconcile(t, owner)
		if r := e.replicas(t, key); r != 1 {
			t.Errorf("replicas=%d, want woken to 1: a sibling that never opted in was added after the scale-down", r)
		}
	})

	t.Run("a follower writes no scale-to-zero state", func(t *testing.T) {
		e := startStzEnv(t)
		key := e.controllerDeployment(t, 1)
		e.definition(t, "a-owner", &yes, "10m", ago(20*time.Minute))
		follower := e.definition(t, "b-follower", &yes, "10m", nil)
		_, st := e.reconcile(t, follower)
		if st != nil {
			t.Errorf("follower status.scaleToZero=%+v, want none: the owner decides for the shared controller", st)
		}
		if r := e.replicas(t, key); r != 1 {
			t.Errorf("replicas=%d after a follower pass, want untouched 1", r)
		}
	})

	t.Run("opting out drops the state and does not touch replicas", func(t *testing.T) {
		e := startStzEnv(t)
		key := e.controllerDeployment(t, 0)
		cd := e.definition(t, "a", &no, "", ago(20*time.Minute))
		_, st := e.reconcile(t, cd)
		if st != nil {
			t.Errorf("status.scaleToZero=%+v after opting out, want removed", st)
		}
		// Waking on opt-out is the definition's own re-apply (KeepLiveReplicas false renders the
		// chart value), not this reconciler: it must not become a second writer of replicas.
		if r := e.replicas(t, key); r != 0 {
			t.Errorf("replicas=%d, want untouched 0", r)
		}
	})
}

// THE WAKE WATCH, through a running manager: the reconciler as main starts it (SetupScaleToZero),
// a controller held at 0 for an idle Kind, and one instance created. The helper watches nothing, so
// without the watch nothing would ever wake it.
func TestScaleToZeroEnvtestWakeWatch(t *testing.T) {
	yes := true
	e := startStzEnv(t)
	key := e.controllerDeployment(t, 0)
	old := time.Now().Add(-time.Hour)
	e.definition(t, "a", &yes, "10m", &old)

	cfg := e.cfg.Config
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: e.kube.Scheme(), Metrics: metricsserver.Options{BindAddress: "0"}})
	if err != nil {
		t.Fatal(err)
	}
	live, err := client.New(cfg, client.Options{Scheme: mgr.GetScheme(), Mapper: mgr.GetRESTMapper()})
	if err != nil {
		t.Fatal(err)
	}
	// The real template path is not mounted in a test; active=1 is what the shipped chart renders
	// (TestChartControllerReplicasReadsTheShippedTemplate).
	if err := setupScaleToZero(mgr, live, logging.NewNopLogger(), func(schema.GroupVersionResource) (int32, error) { return 1, nil }); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = mgr.Start(ctx) }()

	// CONTROL: idle and past its hold-down, it stays at 0 while there is no instance. A wake below
	// is then the watch's doing, not something the first pass would have done anyway.
	time.Sleep(5 * time.Second)
	if r := e.replicas(t, key); r != 0 {
		t.Fatalf("control: replicas=%d with no instance, want 0", r)
	}

	created := time.Now()
	e.instance(t, "first")
	deadline := created.Add(30 * time.Second)
	for e.replicas(t, key) != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("an instance was created and the controller was not woken within 30s (replicas=%d)", e.replicas(t, key))
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Measured, not asserted beyond the 30s bound: create -> scale patched, polled every 100ms. It
	// excludes pod scheduling and start, which envtest does not run.
	t.Logf("wake latency (instance create -> controller scale 1): %s", time.Since(created).Truncate(10*time.Millisecond))
}

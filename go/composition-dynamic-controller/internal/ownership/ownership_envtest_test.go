//go:build envtest

// core-provider docs/design/cdc-pooling.md, step 2, proven the way §5 asks: against a real
// kube-apiserver, a second reconciler for the same Kind x version is refused. Both controllers are
// built exactly as main.go builds one (builder.Build plus ownership.Option), so this proves the cdc's
// wiring and the Lease semantics together, not the runtime's in isolation.
//
//	Run: KUBEBUILDER_ASSETS=$(setup-envtest use -p path 1.36.0) \
//	go test -tags envtest ./internal/ownership/ -run Envtest -v
package ownership

import (
	"context"
	"log/slog"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/krateo-platformops/unstructured-runtime/pkg/controller"
	"github.com/krateo-platformops/unstructured-runtime/pkg/controller/builder"
	"github.com/krateo-platformops/unstructured-runtime/pkg/logging"
	corev1 "k8s.io/api/core/v1"
	apixv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// countingClient is an external client that only counts: every reconcile of this controller starts
// with Observe, so the count is how many reconciles ran.
type countingClient struct{ observes atomic.Int32 }

func (c *countingClient) Observe(context.Context, *unstructured.Unstructured) (controller.ExternalObservation, error) {
	c.observes.Add(1)
	return controller.ExternalObservation{ResourceExists: true, ResourceUpToDate: true}, nil
}
func (c *countingClient) Create(context.Context, *unstructured.Unstructured) error { return nil }
func (c *countingClient) Update(context.Context, *unstructured.Unstructured) error { return nil }
func (c *countingClient) Delete(context.Context, *unstructured.Unstructured) error { return nil }

// fixedPluralizer resolves the one Kind this test reconciles. The runtime's default pluralizer
// asks the cluster through an in-cluster config, which a test process does not have.
type fixedPluralizer struct{ gvr schema.GroupVersionResource }

func (p fixedPluralizer) GVKtoGVR(schema.GroupVersionKind) (schema.GroupVersionResource, error) {
	return p.gvr, nil
}

func TestEnvtest_ASecondControllerForTheSameKindVersionIsRefused(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS unset")
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest apiserver: %v", err)
	}
	defer func() { _ = env.Stop() }()
	ctx := context.Background()

	gvr := schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1-0-0", Resource: "fireworksapps"}
	lease := "v1-0-0.fireworksapps.composition.krateo.io" // what core-provider renders (templates.ControllerLeaseName)
	const ns = "krateo-system"

	preserve := true
	ac := apiextclient.NewForConfigOrDie(cfg)
	if _, err := ac.ApiextensionsV1().CustomResourceDefinitions().Create(ctx, &apixv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "fireworksapps.composition.krateo.io"},
		Spec: apixv1.CustomResourceDefinitionSpec{
			Group: gvr.Group,
			Names: apixv1.CustomResourceDefinitionNames{Plural: "fireworksapps", Singular: "fireworksapp", Kind: "FireworksApp", ListKind: "FireworksAppList"},
			Scope: apixv1.NamespaceScoped,
			Versions: []apixv1.CustomResourceDefinitionVersion{{
				Name: gvr.Version, Served: true, Storage: true,
				Schema:       &apixv1.CustomResourceValidation{OpenAPIV3Schema: &apixv1.JSONSchemaProps{Type: "object", XPreserveUnknownFields: &preserve}},
				Subresources: &apixv1.CustomResourceSubresources{Status: &apixv1.CustomResourceSubresourceStatus{}},
			}},
		},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	cs := kubernetes.NewForConfigOrDie(cfg)
	if _, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	dyn := dynamic.NewForConfigOrDie(cfg)
	waitFor(t, "the CRD to be served", 30*time.Second, func() bool {
		_, err := dyn.Resource(gvr).Namespace(ns).List(ctx, metav1.ListOptions{})
		return err == nil
	})
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": gvr.Group + "/" + gvr.Version, "kind": "FireworksApp",
		"metadata": map[string]any{"name": "app-1", "namespace": ns},
	}}
	if _, err := dyn.Resource(gvr).Namespace(ns).Create(ctx, obj, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	build := func(pod string) (*controller.Controller, *countingClient) {
		t.Helper()
		opt, err := Option(lease, ns, pod)
		if err != nil || opt == nil {
			t.Fatalf("Option for %s: %v (option %v)", pod, err, opt != nil)
		}
		c, err := builder.Build(ctx, builder.Configuration{Config: cfg, GVR: gvr, ProviderName: "composition-dynamic-controller"},
			builder.WithResyncInterval(time.Hour), builder.WithGracefulShutdownTimeout(5*time.Second), opt, builder.WithPluralizer(fixedPluralizer{gvr}),
			builder.WithLogger(logging.NewLogrLogger(logr.FromSlogHandler(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})).WithValues("pod", pod))))
		if err != nil {
			t.Fatalf("building %s: %v", pod, err)
		}
		ext := &countingClient{}
		c.SetExternalClient(ext)
		return c, ext
	}
	holder := func() string {
		l, err := cs.CoordinationV1().Leases(ns).Get(ctx, lease, metav1.GetOptions{})
		if err != nil || l.Spec.HolderIdentity == nil {
			return ""
		}
		return *l.Spec.HolderIdentity
	}

	a, extA := build("pod-a")
	ctxA, stopA := context.WithCancel(ctx)
	doneA := make(chan error, 1)
	go func() { doneA <- a.Run(ctxA, 1) }()
	waitFor(t, "the first controller to reconcile", 30*time.Second, func() bool { return extA.observes.Load() > 0 })
	if h := holder(); h != "pod-a" {
		t.Fatalf("Lease %s/%s holder = %q, want pod-a", ns, lease, h)
	}

	b, extB := build("pod-b")
	ctxB, stopB := context.WithCancel(ctx)
	defer stopB()
	doneB := make(chan error, 1)
	go func() { doneB <- b.Run(ctxB, 1) }()

	// Refused: it lists (Ready), and reconciles nothing for longer than a full lease duration (15s).
	waitFor(t, "the second controller to be Ready", 30*time.Second, func() bool { return b.ReadyzCheck(nil) == nil })
	time.Sleep(17 * time.Second)
	if n := extB.observes.Load(); n != 0 {
		t.Fatalf("the second controller for %s reconciled %d times while pod-a held the Lease", gvr, n)
	}
	if h := holder(); h != "pod-a" {
		t.Fatalf("Lease holder = %q while pod-a runs, want pod-a", h)
	}

	// The first stops (a rolling update's old pod): it drains, releases, and the second takes over.
	stopA()
	select {
	case err := <-doneA:
		if err != nil {
			t.Fatalf("pod-a Run: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("pod-a did not stop")
	}
	waitFor(t, "the second controller to take over", 10*time.Second, func() bool { return extB.observes.Load() > 0 })
	if h := holder(); h != "pod-b" {
		t.Fatalf("Lease holder = %q after the handover, want pod-b", h)
	}

	stopB()
	select {
	case err := <-doneB:
		if err != nil {
			t.Fatalf("pod-b Run: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("pod-b did not stop")
	}
}

func waitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", d, what)
}

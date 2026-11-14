//go:build envtest

// core-provider#508, measured the way it was found: against a real apiserver, with the REAL
// unstructured-runtime controller deciding what Ready ends up as. Found on krateo-057's alerts-page,
// whose helm reconcile was refused: Ready read Unknown/ObserveFailed (hiding the cause), and its
// lastTransitionTime moved on every pass while Synced's held.
//
//	Run: KUBEBUILDER_ASSETS=$(setup-envtest use -p path 1.36.0) \
//	go test -tags envtest ./internal/composition/ -run Envtest_AssessedFailure -v
package composition

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/krateo-platformops/unstructured-runtime/pkg/controller"
	"github.com/krateo-platformops/unstructured-runtime/pkg/controller/builder"
	"github.com/krateo-platformops/unstructured-runtime/pkg/tools"
	unstructuredtools "github.com/krateo-platformops/unstructured-runtime/pkg/tools/unstructured"
	"github.com/krateo-platformops/unstructured-runtime/pkg/tools/unstructured/condition"
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

type assessedPluralizer struct{ gvr schema.GroupVersionResource }

func (p assessedPluralizer) GVKtoGVR(schema.GroupVersionKind) (schema.GroupVersionResource, error) {
	return p.gvr, nil
}

// failingObserve fails every Observe after DIAGNOSING the failure, either the fixed way
// (failObserveWithFinding) or the way the cdc did before #508 (write the same finding, return the
// plain error).
type failingObserve struct {
	opts     tools.UpdateOptions
	fixed    bool
	observes atomic.Int32
}

func (f *failingObserve) Observe(ctx context.Context, mg *unstructured.Unstructured) (controller.ExternalObservation, error) {
	f.observes.Add(1)
	cause := errors.New(`reconciling helm chart: Select.widgets.templates.krateo.io "alerts-hasIncidents" is invalid`)
	if f.fixed {
		return controller.ExternalObservation{}, failObserveWithFinding(ctx, mg, cause, f.opts)
	}
	cond := condition.Unavailable()
	cond.Message = cause.Error()
	_ = unstructuredtools.SetConditions(mg, cond)
	if _, err := updateStatus(ctx, mg, f.opts); err != nil {
		return controller.ExternalObservation{}, err
	}
	return controller.ExternalObservation{}, cause
}
func (f *failingObserve) Create(context.Context, *unstructured.Unstructured) error { return nil }
func (f *failingObserve) Update(context.Context, *unstructured.Unstructured) error { return nil }
func (f *failingObserve) Delete(context.Context, *unstructured.Unstructured) error { return nil }

func TestEnvtest_AssessedFailure_KeepsTheFindingAndItsClock(t *testing.T) {
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

	gvr := schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1-8-119", Resource: "alertspages"}
	const ns = "krateo-system"
	preserve := true
	if _, err := apiextclient.NewForConfigOrDie(cfg).ApiextensionsV1().CustomResourceDefinitions().Create(ctx, &apixv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: gvr.Resource + "." + gvr.Group},
		Spec: apixv1.CustomResourceDefinitionSpec{
			Group: gvr.Group,
			Names: apixv1.CustomResourceDefinitionNames{Plural: gvr.Resource, Singular: "alertspage", Kind: "AlertsPage", ListKind: "AlertsPageList"},
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
	if _, err := kubernetes.NewForConfigOrDie(cfg).CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	dyn := dynamic.NewForConfigOrDie(cfg)
	waitUntil(t, "the CRD to be served", 30*time.Second, func() bool {
		_, err := dyn.Resource(gvr).Namespace(ns).List(ctx, metav1.ListOptions{})
		return err == nil
	})

	readyOf := func(name string) (status, reason, message, ltt string, synced bool) {
		u, err := dyn.Resource(gvr).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return
		}
		for _, c := range unstructuredtools.GetConditions(u) {
			switch c.Type {
			case condition.TypeReady:
				status, reason, message, ltt = string(c.Status), c.Reason, c.Message, c.LastTransitionTime.UTC().Format(time.RFC3339)
			case condition.TypeSynced:
				synced = c.Reason == condition.ReasonReconcileError
			}
		}
		return
	}

	for _, tc := range []struct {
		name       string
		fixed      bool
		wantStatus string
		wantReason string
	}{
		{"fixed: the finding stands", true, "False", condition.ReasonUnavailable},
		{"control, the pre-#508 shape: overwritten with ObserveFailed", false, "Unknown", condition.ReasonObserveFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := "alerts-page-fixed"
			if !tc.fixed {
				name = "alerts-page-control"
			}
			obj := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": gvr.Group + "/" + gvr.Version, "kind": "AlertsPage",
				"metadata": map[string]any{"name": name, "namespace": ns},
			}}
			if _, err := dyn.Resource(gvr).Namespace(ns).Create(ctx, obj, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			plural := assessedPluralizer{gvr}
			ext := &failingObserve{opts: tools.UpdateOptions{Pluralizer: plural, DynamicClient: dyn}, fixed: tc.fixed}
			c, err := builder.Build(ctx, builder.Configuration{Config: cfg, GVR: gvr, ProviderName: "composition-dynamic-controller"},
				builder.WithNamespace(ns), builder.WithPluralizer(plural), builder.WithResyncInterval(time.Hour),
				builder.WithMaxRetries(1000), builder.WithGracefulShutdownTimeout(2*time.Second))
			if err != nil {
				t.Fatal(err)
			}
			c.SetExternalClient(ext)
			runCtx, stop := context.WithCancel(ctx)
			done := make(chan error, 1)
			go func() { done <- c.Run(runCtx, 1) }()
			defer func() { stop(); <-done }()

			// Synced=ReconcileError is the runtime's own write on this path in BOTH arms, so it is the
			// witness that the runtime has processed a failed Observe at least once.
			waitUntil(t, "the runtime to record a failed observe", 30*time.Second, func() bool {
				_, _, _, _, synced := readyOf(name)
				return synced && ext.observes.Load() >= 1
			})
			status, reason, message, ltt1, _ := readyOf(name)
			if status != tc.wantStatus || reason != tc.wantReason {
				t.Fatalf("Ready = %s/%s (%q), want %s/%s", status, reason, message, tc.wantStatus, tc.wantReason)
			}
			if tc.fixed && !strings.Contains(message, "alerts-hasIncidents") {
				t.Errorf("the finding must carry its cause, got %q", message)
			}

			// The clock: let the runtime retry (the failure requeues with backoff) and read again.
			first := ext.observes.Load()
			waitUntil(t, "two more failed passes", 30*time.Second, func() bool { return ext.observes.Load() >= first+2 })
			time.Sleep(time.Second) // the last pass's status write lands
			_, _, _, ltt2, _ := readyOf(name)
			t.Logf("%s: observes=%d, Ready lastTransitionTime %s -> %s", name, ext.observes.Load(), ltt1, ltt2)
			if tc.fixed && ltt1 != ltt2 {
				t.Errorf("with the finding standing, Ready's lastTransitionTime moved (%s -> %s) though nothing transitioned", ltt1, ltt2)
			}
		})
	}
}

func waitUntil(t *testing.T, what string, d time.Duration, cond func() bool) {
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

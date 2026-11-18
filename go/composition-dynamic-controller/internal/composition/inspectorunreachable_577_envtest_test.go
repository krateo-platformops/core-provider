//go:build envtest

// core-provider#577, against a real apiserver with the REAL unstructured-runtime controller deciding
// what Ready ends up as. On krateo-057 a core-provider roll refused connections to chart-inspector
// for ~35 s, and every composition reconciling in that window recorded Ready=False/Unavailable.
//
//	Run: KUBEBUILDER_ASSETS=$(setup-envtest use -p path 1.36.0) \
//	go test -tags envtest ./internal/composition/ -run Envtest_InspectorUnreachable -v
package composition

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/krateo-platformops/composition-dynamic-controller/internal/conditionreason"
	"github.com/krateo-platformops/unstructured-runtime/pkg/controller"
	"github.com/krateo-platformops/unstructured-runtime/pkg/controller/builder"
	"github.com/krateo-platformops/unstructured-runtime/pkg/tools"
	unstructuredtools "github.com/krateo-platformops/unstructured-runtime/pkg/tools/unstructured"
	"github.com/krateo-platformops/unstructured-runtime/pkg/tools/unstructured/condition"
	corev1 "k8s.io/api/core/v1"
	apixv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// rbacObserve is Observe's derived-RBAC step and nothing else: the real applyDerivedRBAC against the
// chart-inspector at url, and on failure the real failObserveOnDerivedRBAC, exactly as Observe calls
// them (composition.go).
type rbacObserve struct {
	h        *handler
	opts     tools.UpdateOptions
	observes atomic.Int32
}

func (o *rbacObserve) Observe(ctx context.Context, mg *unstructured.Unstructured) (controller.ExternalObservation, error) {
	o.observes.Add(1)
	_, err := o.h.applyDerivedRBAC(ctx, mg, pkgFor577, fakeDynWith(), mg.GetName()+"-abcde")
	if err != nil {
		return controller.ExternalObservation{}, failObserveOnDerivedRBAC(ctx, mg, err, time.Now(), o.opts)
	}
	return controller.ExternalObservation{ResourceExists: true, ResourceUpToDate: true}, nil
}
func (o *rbacObserve) Create(context.Context, *unstructured.Unstructured) error { return nil }
func (o *rbacObserve) Update(context.Context, *unstructured.Unstructured) error { return nil }
func (o *rbacObserve) Delete(context.Context, *unstructured.Unstructured) error { return nil }

func TestEnvtest_InspectorUnreachable_KeepsReadyForTheGrace(t *testing.T) {
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

	gvr := schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1-0-0", Resource: "fireworks"}
	const ns = "krateo-system"
	preserve := true
	if _, err := apiextclient.NewForConfigOrDie(cfg).ApiextensionsV1().CustomResourceDefinitions().Create(ctx, &apixv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: gvr.Resource + "." + gvr.Group},
		Spec: apixv1.CustomResourceDefinitionSpec{
			Group: gvr.Group,
			Names: apixv1.CustomResourceDefinitionNames{Plural: gvr.Resource, Singular: "firework", Kind: "Firework", ListKind: "FireworkList"},
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

	get := func(name string) *unstructured.Unstructured {
		u, err := dyn.Resource(gvr).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	// setStatusConditions writes conds onto the stored object's status, retrying on conflict with the
	// controller's own writes.
	setStatusConditions := func(name string, conds ...metav1.Condition) {
		t.Helper()
		for i := 0; ; i++ {
			u := get(name)
			for _, c := range conds {
				// Raw write: SetConditions would keep the stored transition time on a same-status write,
				// and the point of the backdating below is to move it.
				all := unstructuredtools.GetConditions(u)
				replaced := false
				for j := range all {
					if all[j].Type == c.Type {
						all[j], replaced = c, true
					}
				}
				if !replaced {
					all = append(all, c)
				}
				raw := make([]any, 0, len(all))
				for _, x := range all {
					m, err := toMap(x)
					if err != nil {
						t.Fatal(err)
					}
					raw = append(raw, m)
				}
				if err := unstructured.SetNestedSlice(u.Object, raw, "status", "conditions"); err != nil {
					t.Fatal(err)
				}
			}
			_, err := dyn.Resource(gvr).Namespace(ns).UpdateStatus(ctx, u, metav1.UpdateOptions{})
			if err == nil {
				return
			}
			if !apierrors.IsConflict(err) || i > 20 {
				t.Fatal(err)
			}
		}
	}

	healthySince := metav1.NewTime(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
	readyTrue := condition.Available()
	readyTrue.LastTransitionTime = healthySince

	inspector500 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"render failed"}`, http.StatusInternalServerError)
	}))
	defer inspector500.Close()

	for _, tc := range []struct {
		name, url string
		kept      bool
	}{
		{"refused: Ready kept, then recorded once the grace ends", refusedURL(), true},
		{"control, a 500: recorded at once", inspector500.URL, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := "fw-kept"
			if !tc.kept {
				name = "fw-control"
			}
			obj := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": gvr.Group + "/" + gvr.Version, "kind": "Firework",
				"metadata": map[string]any{"name": name, "namespace": ns},
			}}
			if _, err := dyn.Resource(gvr).Namespace(ns).Create(ctx, obj, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			// Healthy as last assessed, before the controller starts.
			setStatusConditions(name, readyTrue)

			plural := assessedPluralizer{gvr}
			ext := &rbacObserve{
				h:    &handler{pluralizer: stubPluralizerForRBAC{}, chartInspectorUrl: tc.url, saName: "cdc", saNamespace: ns},
				opts: tools.UpdateOptions{Pluralizer: plural, DynamicClient: dyn},
			}
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

			// Synced=ReconcileError is the runtime's own write after a failed Observe, in both arms: the
			// witness that a pass has been fully processed.
			synced := func() bool {
				s := condOf(get(name), condition.TypeSynced)
				return s != nil && s.Reason == condition.ReasonReconcileError
			}
			waitUntil(t, "a failed observe to be recorded", 30*time.Second, func() bool { return synced() && ext.observes.Load() >= 1 })
			first := ext.observes.Load()
			waitUntil(t, "two more failed passes", 30*time.Second, func() bool { return ext.observes.Load() >= first+2 })
			time.Sleep(time.Second) // the last pass's status write lands

			u := get(name)
			ready, cr := condOf(u, condition.TypeReady), condOf(u, conditionreason.TypeChartRenderable)
			t.Logf("%s after %d passes: Ready=%s/%s since %s; ChartRenderable=%s/%s; Synced: %s",
				name, ext.observes.Load(), ready.Status, ready.Reason, ready.LastTransitionTime.UTC().Format(time.RFC3339),
				cr.Status, cr.Reason, condOf(u, condition.TypeSynced).Message)
			if cr.Status != metav1.ConditionFalse || cr.Reason != conditionreason.ChartInspectorUnavailable.String() {
				t.Fatalf("ChartRenderable = %s/%s, want False/%s", cr.Status, cr.Reason, conditionreason.ChartInspectorUnavailable)
			}
			if !tc.kept {
				if ready.Status != metav1.ConditionFalse || ready.Reason != condition.ReasonUnavailable {
					t.Fatalf("control: Ready = %s/%s after a 500, want False/Unavailable", ready.Status, ready.Reason)
				}
				return
			}
			if ready.Status != metav1.ConditionTrue || ready.Reason != condition.ReasonAvailable || !ready.LastTransitionTime.Equal(&healthySince) {
				t.Fatalf("Ready = %s/%s since %s inside the grace, want True/Available since %s, untouched",
					ready.Status, ready.Reason, ready.LastTransitionTime, healthySince)
			}
			if msg := condOf(u, condition.TypeSynced).Message; !strings.Contains(msg, "Ready is kept as last assessed until") {
				t.Errorf("Synced does not say Ready is being kept: %q", msg)
			}

			// End the grace: chart-inspector has now been unreachable for 6 minutes.
			stale := *cr
			stale.LastTransitionTime = metav1.NewTime(time.Now().Add(-6 * time.Minute))
			setStatusConditions(name, stale)
			waitUntil(t, "Ready to be recorded after the grace", 60*time.Second, func() bool {
				r := condOf(get(name), condition.TypeReady)
				return r != nil && r.Status == metav1.ConditionFalse
			})
			u = get(name)
			ready = condOf(u, condition.TypeReady)
			if ready.Reason != condition.ReasonUnavailable || !strings.Contains(ready.Message, "connection refused") {
				t.Errorf("after the grace Ready = %s/%s %q, want Unavailable naming the refused connection", ready.Status, ready.Reason, ready.Message)
			}
			if rc := condOf(u, "RBACComplete"); rc == nil || rc.Reason != conditionreason.RBACNotEvaluated.String() {
				t.Errorf("after the grace RBACComplete = %+v, want %s as before #577", rc, conditionreason.RBACNotEvaluated)
			}
		})
	}
}

func toMap(c metav1.Condition) (map[string]any, error) {
	return runtime.DefaultUnstructuredConverter.ToUnstructured(&c)
}

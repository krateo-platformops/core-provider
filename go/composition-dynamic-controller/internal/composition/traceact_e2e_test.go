//go:build integration
// +build integration

package composition

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gobuffalo/flect"
	"github.com/krateo-platformops/plumbing/tracecontext"
	"github.com/krateo-platformops/unstructured-runtime/pkg/controller"
	"github.com/krateo-platformops/unstructured-runtime/pkg/controller/builder"
	ctrlevent "github.com/krateo-platformops/unstructured-runtime/pkg/controller/event"
	runtimeMeta "github.com/krateo-platformops/unstructured-runtime/pkg/meta"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/e2e-framework/klient/decoder"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
)

// unstructured-runtime#77, the cdc's half: WHERE the cdc declares its act. The rule itself (a new
// root per pass, only the first act for a new krateo.io/traceparent value joins the upstream) is
// pinned in unstructured-runtime. What only this repo can pin is that Observe calls
// controller.StartAct AFTER the release-not-found return and before it applies anything.
//
// So this drives the REAL runtime loop (builder.Build, Run) with the cdc's handler against kind,
// recording spans through builder.WithTracerProvider, on a composition that arrives the way a
// frontend-created one does: carrying a traceparent and no release. The runtime's default turns
// that creation into an Observe (CRCreated -> Observe), which finds no release and must join
// nothing; the Create it enqueues installs and must be the act that joins. Then a NEW traceparent
// on the existing composition reaches Observe (the cdc's CRUpdated -> Observe), which now applies,
// so its "apply" step must join that new upstream.
//
// Had StartAct sat before the release check, the first arm would see "apply" joined at the no-op
// Observe and "create" only linked: the frontend's trace would never show the install.
//
// It runs in its own namespace so this controller never reconciles the other steps' compositions.
func assessTraceActPlacement(ctx context.Context, t *testing.T, cfg *envconf.Config, handler controller.ExternalClient, c *rest.Config) context.Context {
	const (
		traceNS = "trace-77"
		name    = "trace-act"
		up1     = "00-7a0000000000000000000000000000a1-00000000000000a1-01"
		up2     = "00-7a0000000000000000000000000000a2-00000000000000a2-01"
	)
	if handler == nil || c == nil {
		t.Fatal("control: the Setup step must have built the handler")
	}

	cs, err := kubernetes.NewForConfig(cfg.Client().RESTConfig())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: traceNS}}, metav1.CreateOptions{}); err != nil && !errors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
	// The runtime loop runs as the controller's ServiceAccount (c, test-sa), as in production, and
	// SetSAToken grants it compositions only in its own namespace. Grant the same in traceNS. The
	// test's own writes below (create, then a new traceparent) run as the cluster admin, the way a
	// user or the frontend makes them.
	if _, err := cs.RbacV1().Roles(traceNS).Create(ctx, &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: "test-sa-role", Namespace: traceNS},
		Rules:      []rbacv1.PolicyRule{{APIGroups: []string{"composition.krateo.io"}, Resources: []string{"*"}, Verbs: []string{"*"}}},
	}, metav1.CreateOptions{}); err != nil && !errors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
	if _, err := cs.RbacV1().RoleBindings(traceNS).Create(ctx, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "test-sa-role-binding", Namespace: traceNS},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: "test-sa", Namespace: namespace}},
		RoleRef:    rbacv1.RoleRef{Kind: "Role", Name: "test-sa-role", APIGroup: "rbac.authorization.k8s.io"},
	}, metav1.CreateOptions{}); err != nil && !errors.IsAlreadyExists(err) {
		t.Fatal(err)
	}

	var obj unstructured.Unstructured
	if err := decoder.DecodeFile(os.DirFS(filepath.Join(testdataPath, "compositions")), "focus.yaml", &obj); err != nil {
		t.Fatal(err)
	}
	version := obj.GetLabels()["krateo.io/composition-version"]
	gvr := schema.GroupVersionResource{
		Group:    "composition.krateo.io",
		Version:  version,
		Resource: flect.Pluralize(strings.ToLower(obj.GetKind())),
	}
	obj.SetName(name)
	obj.SetNamespace(traceNS)
	labels := obj.GetLabels()
	labels["krateo.io/release-name"] = name
	obj.SetLabels(labels)
	obj.SetAnnotations(map[string]string{runtimeMeta.AnnotationKeyTraceparent: up1})

	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	defer func() { _ = tp.Shutdown(context.Background()) }()

	ctrl, err := builder.Build(ctx, builder.Configuration{Config: c, GVR: gvr, ProviderName: "trace-act"},
		builder.WithNamespace(traceNS),
		builder.WithPluralizer(FakePluralizer{}),
		builder.WithGracefulShutdownTimeout(0),
		// As the cdc's main.go configures it: an annotation change reaches Observe.
		builder.WithActionEvent(ctrlevent.CRUpdated, ctrlevent.Observe),
		builder.WithTracerProvider(tp),
	)
	if err != nil {
		t.Fatal(err)
	}
	ctrl.SetExternalClient(handler)
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); _ = ctrl.Run(runCtx, 1) }()
	defer func() { stop(); <-done }()

	cli := dynamic.NewForConfigOrDie(cfg.Client().RESTConfig()).Resource(gvr).Namespace(traceNS)
	if _, err := cli.Create(ctx, &obj, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	sc1, sc2 := mustParse(t, up1), mustParse(t, up2)

	// ARM 1: a new composition. Wait for the act joined to up1, then for the marker.
	joined1 := waitJoined(t, rec, sc1, 4*time.Minute)
	if len(joined1) != 1 || joined1[0].Name() != "create" {
		t.Fatalf("a new composition's upstream must be joined ONCE, by the install (create); got %s", spanNames(joined1))
	}
	waitMarker(ctx, t, cli, name, tracecontext.Canonical(sc1), time.Minute)
	// Control: the install's own root starts before its act, so the Observe that found no release
	// is the SECOND root before it. Fewer, and this arm never exercised the no-op Observe.
	if n := rootsBefore(rec, joined1[0]); n < 2 {
		t.Errorf("control: expected the Observe that found no release to run before the install's pass, saw %d roots before the act", n)
	}
	// unstructured-runtime v1.15.0: the root of a pass that ACTS carries provider_runtime.acted=true,
	// so the tail sampler keeps it; the Observe that found no release did not act and carries none.
	if v, ok := rootActed(rootOf(t, rec, joined1[0])); !ok || !v {
		t.Errorf("the install's root must carry %s=true, got (%v, present %v)", tracecontext.AttrActed, v, ok)
	}
	if _, ok := rootActed(firstRoot(t, rec)); ok {
		t.Errorf("the first pass (an Observe that found no release) acted nothing, so its root must carry no %s", tracecontext.AttrActed)
	}

	// ARM 2: a new traceparent on the existing composition reaches Observe, which applies.
	patch := []byte(`{"metadata":{"annotations":{"` + runtimeMeta.AnnotationKeyTraceparent + `":"` + up2 + `"}}}`)
	if _, err := cli.Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		t.Fatal(err)
	}
	joined2 := waitJoined(t, rec, sc2, 3*time.Minute)
	if len(joined2) != 1 || joined2[0].Name() != "apply" {
		t.Fatalf("an existing composition's new upstream must be joined ONCE, by Observe's apply; got %s", spanNames(joined2))
	}
	waitMarker(ctx, t, cli, name, tracecontext.Canonical(sc2), time.Minute)
	if v, ok := rootActed(rootOf(t, rec, joined2[0])); !ok || !v {
		t.Errorf("Observe's apply root must carry %s=true, got (%v, present %v)", tracecontext.AttrActed, v, ok)
	}

	// Every pass is its own root, never an upstream child; and up1 was never re-joined.
	for _, s := range rec.Ended() {
		if s.Name() == tracecontext.SpanReconcile && s.Parent().IsValid() {
			t.Errorf("a reconcile root has a parent %s: passes must start new roots", s.Parent().SpanID())
		}
	}
	if j := joinedTo(rec.Ended(), sc1); len(j) != 1 {
		t.Errorf("up1 must stay joined once after later passes; got %s", spanNames(j))
	}

	// Clean up through the controller, so the release is uninstalled.
	if err := cli.Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
		t.Logf("deleting %s: %v", name, err)
	}
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		if _, err := cli.Get(ctx, name, metav1.GetOptions{}); errors.IsNotFound(err) {
			break
		}
		time.Sleep(2 * time.Second)
	}
	return ctx
}

func mustParse(t *testing.T, tp string) trace.SpanContext {
	t.Helper()
	d := tracecontext.Decide(map[string]string{tracecontext.AnnotationKeyTraceparent: tp}, "", time.Time{}, time.Now(), tracecontext.DefaultContinueWindow)
	if !d.Remote.IsValid() {
		t.Fatalf("control: fixture traceparent %q must parse", tp)
	}
	return d.Remote
}

func joinedTo(spans []sdktrace.ReadOnlySpan, up trace.SpanContext) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range spans {
		if s.Parent().TraceID() == up.TraceID() && s.Parent().SpanID() == up.SpanID() {
			out = append(out, s)
		}
	}
	return out
}

// waitJoined waits until a span joined to up has ended, then a few more seconds so a wrong SECOND
// join (the defect's shape) would be recorded too, and returns every span joined to up.
func waitJoined(t *testing.T, rec *tracetest.SpanRecorder, up trace.SpanContext, timeout time.Duration) []sdktrace.ReadOnlySpan {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if len(joinedTo(rec.Ended(), up)) > 0 {
			time.Sleep(10 * time.Second)
			return joinedTo(rec.Ended(), up)
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("no span joined to %s within %s; %d spans ended: %s", up.SpanID(), timeout, len(rec.Ended()), spanNames(rec.Ended()))
	return nil
}

func waitMarker(ctx context.Context, t *testing.T, cli dynamic.ResourceInterface, name, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	got := ""
	for time.Now().Before(deadline) {
		u, err := cli.Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			if got = u.GetAnnotations()[tracecontext.AnnotationKeyTraceparentContinued]; got == want {
				return
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("marker %s = %q, want %q", tracecontext.AnnotationKeyTraceparentContinued, got, want)
}

// rootsBefore counts reconcile roots that started before s did.
func rootsBefore(rec *tracetest.SpanRecorder, s sdktrace.ReadOnlySpan) int {
	n := 0
	for _, r := range rec.Ended() {
		if r.Name() == tracecontext.SpanReconcile && r.StartTime().Before(s.StartTime()) {
			n++
		}
	}
	return n
}

// rootOf is the reconcile root the act links back to (the act -> root link, present because roots
// are head-sampled here).
func rootOf(t *testing.T, rec *tracetest.SpanRecorder, act sdktrace.ReadOnlySpan) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, l := range act.Links() {
		for _, r := range rec.Ended() {
			if r.Name() == tracecontext.SpanReconcile && r.SpanContext().SpanID() == l.SpanContext.SpanID() {
				return r
			}
		}
	}
	t.Fatalf("no recorded reconcile root is linked from act %q (links %d)", act.Name(), len(act.Links()))
	return nil
}

// firstRoot is the earliest-started reconcile root.
func firstRoot(t *testing.T, rec *tracetest.SpanRecorder) sdktrace.ReadOnlySpan {
	t.Helper()
	var first sdktrace.ReadOnlySpan
	for _, r := range rec.Ended() {
		if r.Name() == tracecontext.SpanReconcile && (first == nil || r.StartTime().Before(first.StartTime())) {
			first = r
		}
	}
	if first == nil {
		t.Fatal("no reconcile root recorded")
	}
	return first
}

func rootActed(s sdktrace.ReadOnlySpan) (bool, bool) {
	for _, kv := range s.Attributes() {
		if kv.Key == tracecontext.AttrActed {
			return kv.Value.AsBool(), true
		}
	}
	return false, false
}

func spanNames(spans []sdktrace.ReadOnlySpan) string {
	names := make([]string, 0, len(spans))
	for _, s := range spans {
		names = append(names, s.Name())
	}
	return "[" + strings.Join(names, " ") + "]"
}

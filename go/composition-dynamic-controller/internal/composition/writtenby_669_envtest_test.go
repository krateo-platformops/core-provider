//go:build envtest

// core-provider#669 through the handler's own Create and Observe, against a real apiserver and real
// helm storage: every composition status the cdc writes names this binary in status.writtenBy, and
// the stamp never causes a write of its own.
//
//	Run: KUBEBUILDER_ASSETS=$(setup-envtest use -p path 1.36.0) \
//	go test -tags envtest ./internal/composition/ -run Envtest_669 -v
package composition

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/krateo-platformops/composition-dynamic-controller/internal/writtenby"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
)

// statusPuts669 counts the status updates the handler sends for the compositions.
type statusPuts669 struct {
	n    *atomic.Int32
	next http.RoundTripper
}

func (s statusPuts669) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/"+gvr636.Resource+"/") && strings.HasSuffix(r.URL.Path, "/status") {
		s.n.Add(1)
	}
	return s.next.RoundTrip(r)
}

func TestEnvtest_669_WrittenBy(t *testing.T) {
	e := start636(t)
	// One ConfigMap and nothing else. With chart636 (hooks, Jobs, a PVC) every Observe here reported
	// drift and dispatched an Update, so no pass was ever at rest, and the no-op arm needs one.
	a := e.arm(t, "ns669", getter636{url: serve419Chart(t), version: "1.1.10"})
	first := writtenby.Stamp{Version: "2.13.135", Commit: "c0ffee135"}
	a.h.writtenBy = first
	var puts atomic.Int32
	counted := rest.CopyConfig(e.cfg)
	counted.WrapTransport = func(rt http.RoundTripper) http.RoundTripper { return statusPuts669{n: &puts, next: rt} }
	a.h.kubeconfig = counted

	stored := func() (map[string]string, string) {
		t.Helper()
		u := a.get()
		w, _, _ := unstructured.NestedStringMap(u.Object, "status", "writtenBy")
		return w, u.GetResourceVersion()
	}
	want := func(s writtenby.Stamp) map[string]string {
		return map[string]string{"name": "composition-dynamic-controller", "version": s.Version, "commit": s.Commit}
	}
	same := func(a, b map[string]string) bool {
		return len(a) == len(b) && a["name"] == b["name"] && a["version"] == b["version"] && a["commit"] == b["commit"]
	}

	// Create writes the first status (Ready=False/Creating): it names this binary.
	u, err := e.dyn.Resource(gvr636).Namespace(a.ns).Create(e.ctx, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": gvr636.GroupVersion().String(), "kind": "Firework",
		"metadata": map[string]any{"name": "fw", "namespace": a.ns},
		"spec":     map[string]any{"k": "v1"},
	}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.h.Create(e.ctx, u); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := a.history(); got != "1:deployed" {
		t.Fatalf("control: history %q after Create, want 1:deployed", got)
	}
	if got, _ := stored(); !same(got, want(first)) {
		t.Fatalf("after Create: status.writtenBy = %v, want %v", got, want(first))
	}

	// Observe until a pass changes nothing (the first promotes Creating to Available).
	var rv string
	for i := 0; ; i++ {
		_, before := stored()
		if err := a.observe(); err != nil {
			t.Fatal(err)
		}
		var got map[string]string
		got, rv = stored()
		if !same(got, want(first)) {
			t.Fatalf("after Observe %d: status.writtenBy = %v, want %v", i+1, got, want(first))
		}
		if rv == before {
			t.Logf("at rest after %d Observe passes; Ready %s", i+1, a.ready())
			break
		}
		if i == 5 {
			t.Fatalf("control: not at rest after 6 Observe passes (resourceVersion still moving, %s), so the no-op arm below would measure nothing", rv)
		}
	}
	if a.ready() != "True/Available" {
		t.Fatalf("control: at rest with Ready %s, want True/Available", a.ready())
	}

	// THE NO-OP RULE. At rest every Observe still sends the status (updateStatus, unconditionally),
	// with the stored stamp, and the apiserver records nothing.
	puts.Store(0)
	const passes = 3
	for i := 0; i < passes; i++ {
		if err := a.observe(); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("at rest: %d Observe passes sent %d status updates", passes, puts.Load())
	if puts.Load() < passes {
		t.Fatalf("control: %d status updates in %d passes; the passes did not reach the status write, so an unchanged resourceVersion would prove nothing", puts.Load(), passes)
	}
	if _, got := stored(); got != rv {
		t.Errorf("at rest the composition is still being written: resourceVersion %s -> %s", rv, got)
	}

	// A NEW BINARY. Its first Observe, which changes nothing else, writes its version.
	next := writtenby.Stamp{Version: "2.13.136", Commit: "c0ffee136"}
	a.h.writtenBy = next
	if err := a.observe(); err != nil {
		t.Fatal(err)
	}
	got, after := stored()
	if !same(got, want(next)) {
		t.Errorf("after an Observe by the next binary: status.writtenBy = %v, want %v", got, want(next))
	}
	if after == rv {
		t.Errorf("control: the next binary's Observe did not write, so status.writtenBy = %v was not its write", got)
	}
	if a.ready() != "True/Available" {
		t.Errorf("the next binary's pass changed Ready to %s", a.ready())
	}
}

//go:build envtest

// The drain isolation of pool_drain_test.go against the real things: a real apiserver, real controllers,
// real ownership Leases, the real /readyz, and a member that is slow to drain because a reconcile of one
// of its compositions is in flight.
package manager

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/krateo-platformops/composition-dynamic-controller/internal/health"
	"github.com/krateo-platformops/composition-dynamic-controller/internal/metrics"
	"github.com/krateo-platformops/composition-dynamic-controller/internal/tools/archive"
	"github.com/krateo-platformops/composition-dynamic-controller/pkg/meta"
	"github.com/krateo-platformops/plumbing/kubeutil/event"
	"github.com/krateo-platformops/unstructured-runtime/pkg/logging"
	"github.com/krateo-platformops/unstructured-runtime/pkg/ratelimit"
)

const drainPod = "cdc-pool-web-0"

// heldGetter holds every reconcile that reaches it, ignoring its context as helm's own uninstall does,
// until release is closed. It is how a member of the pool is slow to drain.
type heldGetter struct {
	release          chan struct{}
	inFlight, peak   *atomic.Int32
	once             *sync.Once
	firstReconcileIn chan struct{}
}

func newHeldGetter(t *testing.T) heldGetter {
	g := heldGetter{release: make(chan struct{}), inFlight: &atomic.Int32{}, peak: &atomic.Int32{}, once: &sync.Once{}, firstReconcileIn: make(chan struct{})}
	t.Cleanup(g.let)
	return g
}
func (g heldGetter) let() { g.once.Do(func() { close(g.release) }) }
func (g heldGetter) Get(*unstructured.Unstructured) (*archive.Info, error) {
	if n := g.inFlight.Add(1); n > g.peak.Load() {
		g.peak.Store(n)
	}
	defer g.inFlight.Add(-1)
	<-g.release
	return nil, errors.New("the held reconcile was let go")
}
func (g heldGetter) Locate(u *unstructured.Unstructured) (*archive.Info, error) { return g.Get(u) }
func (g heldGetter) WithLogger(logging.Logger) archive.Getter                   { return g }

// knownPlurals maps a test Kind to its resource; the process's own reads discovery in-cluster.
type knownPlurals map[schema.GroupVersionKind]schema.GroupVersionResource

func (k knownPlurals) GVKtoGVR(gvk schema.GroupVersionKind) (schema.GroupVersionResource, error) {
	gvr, ok := k[gvk]
	if !ok {
		return gvr, fmt.Errorf("unknown kind %s", gvk)
	}
	return gvr, nil
}

// runtimeLog records, per Kind x version, the controller runtime's own account of each controller:
// started, took the Lease, released it. At every start it reads the Lease from the apiserver.
type runtimeLog struct {
	logging.Logger
	s   *runtimeLogState
	gvr string
}

type runtimeLogState struct {
	holder func(gvr string) string
	mu     sync.Mutex
	lines  map[string][]string
	bad    []string // starts while the predecessor still held the Lease
}

func (l runtimeLog) WithValues(kv ...any) logging.Logger {
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i] == string(metrics.AttrCompositionGVR) {
			l.gvr = fmt.Sprint(kv[i+1])
		}
	}
	return l
}
func (l runtimeLog) WithName(string) logging.Logger { return l }
func (l runtimeLog) Info(msg string, _ ...any) {
	if l.gvr == "" {
		return
	}
	var line string
	switch {
	case msg == "Starting controller":
		line = "start"
	case strings.HasPrefix(msg, "Holding the ownership lease"):
		line = "hold"
	case msg == "Released the ownership lease":
		line = "release"
	case strings.HasPrefix(msg, "Graceful shutdown period expired"):
		line = "grace expired"
	default:
		return
	}
	holder := ""
	if line == "start" {
		holder = l.s.holder(l.gvr)
	}
	l.s.mu.Lock()
	defer l.s.mu.Unlock()
	if line == "start" {
		holding := false
		for _, p := range l.s.lines[l.gvr] {
			holding = p == "hold" || holding && p != "release"
		}
		if holding || holder != "" {
			l.s.bad = append(l.s.bad, fmt.Sprintf("%s started with the Lease held by %q (predecessor holding: %v)", l.gvr, holder, holding))
		}
	}
	l.s.lines[l.gvr] = append(l.s.lines[l.gvr], line)
}

func (s *runtimeLogState) count(gvr schema.GroupVersionResource, line string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, l := range s.lines[metrics.ControllerAttr(gvr.Group, gvr.Version, gvr.Resource).Value.AsString()] {
		if l == line {
			n++
		}
	}
	return n
}

func (s *runtimeLogState) check(t *testing.T) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range s.bad {
		t.Error(b)
	}
	if len(s.lines) == 0 {
		t.Error("control: the runtime logged nothing, so the overlap check proved nothing")
	}
}

// poolEnv is a pool running against an apiserver, its /readyz served as the kubelet sees it.
type poolEnv struct {
	t        *testing.T
	pool     *Pool
	log      *runtimeLogState
	get      heldGetter
	cs       kubernetes.Interface
	dyn      dynamic.Interface
	readyURL string
	stop     func() error
}

const drainNS = "krateo-system"

func leaseOf(gvr schema.GroupVersionResource) string {
	return gvr.Version + "." + gvr.Resource + "." + gvr.Group
}
func kindOf(gvr schema.GroupVersionResource) string {
	return strings.ToUpper(gvr.Resource[:1]) + gvr.Resource[1:len(gvr.Resource)-1]
}
func keyOf(gvr schema.GroupVersionResource) string { return gvr.Resource + "-" + gvr.Version }

func startPoolEnv(t *testing.T, grace time.Duration, gvrs ...schema.GroupVersionResource) *poolEnv {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS unset")
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest apiserver: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	ctx := context.Background()
	ac := apiextclient.NewForConfigOrDie(cfg)
	e := &poolEnv{t: t, cs: kubernetes.NewForConfigOrDie(cfg), dyn: dynamic.NewForConfigOrDie(cfg), get: newHeldGetter(t)}
	if _, err := e.cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: drainNS}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	plurals := knownPlurals{}
	for _, gvr := range gvrs {
		plurals[gvr.GroupVersion().WithKind(kindOf(gvr))] = gvr
		if _, err := ac.ApiextensionsV1().CustomResourceDefinitions().Create(ctx, compositionCRD(gvr, kindOf(gvr)), metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		cm := memberCM(drainNS, gvr)
		cm.Data[KeyLeaseName] = leaseOf(gvr)
		if _, err := e.cs.CoreV1().ConfigMaps(drainNS).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	for _, gvr := range gvrs {
		eventually(t, gvr.String()+" served", func() bool { _, err := e.dyn.Resource(gvr).List(ctx, metav1.ListOptions{}); return err == nil })
	}
	index := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cdc-pool-web-members", Namespace: drainNS}}
	if _, err := e.cs.CoreV1().ConfigMaps(drainNS).Create(ctx, index, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	e.log = &runtimeLogState{lines: map[string][]string{}, holder: func(gvr string) string {
		p := strings.Split(gvr, "/")
		return e.holder(schema.GroupVersionResource{Group: p[0], Version: p[1], Resource: p[2]})
	}}
	proc := testProcess()
	proc.Governor = ratelimit.New(ratelimit.Options{Ceiling: 4, Shared: true})
	proc.Config = cfg
	proc.Log = runtimeLog{Logger: logging.NewNopLogger(), s: e.log}
	proc.PodNamespace, proc.PodName = drainNS, drainPod
	proc.PackageInfoGetter = e.get
	proc.Pluralizer = plurals
	proc.EventRecorder = *event.NewAPIRecorder(&events.FakeRecorder{})
	proc.GracefulShutdownTimeout = grace
	e.pool = &Pool{Process: proc, Client: e.cs, Namespace: drainNS, Index: index.Name, Backoff: func(int) time.Duration { return 200 * time.Millisecond }}

	healthCtx, stopHealth := context.WithCancel(ctx)
	t.Cleanup(stopHealth)
	addr, err := health.Serve(healthCtx, "127.0.0.1:0", e.pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	e.readyURL = "http://" + addr.String() + health.ReadinessPath

	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- e.pool.Run(runCtx) }()
	e.stop = sync.OnceValue(func() error {
		stop()
		select {
		case err := <-done:
			return err
		case <-time.After(60 * time.Second):
			return errors.New("Run did not return after the context was cancelled")
		}
	})
	t.Cleanup(func() { e.get.let(); _ = e.stop() }) // before the apiserver stops
	t.Cleanup(func() { e.log.check(t) })            // whatever else the test found
	return e
}

// index makes the membership index exactly members: a GVR and its configuration's checksum.
func (e *poolEnv) index(members map[schema.GroupVersionResource]string) {
	e.t.Helper()
	ctx := context.Background()
	cur, err := e.cs.CoreV1().ConfigMaps(drainNS).Get(ctx, e.pool.Index, metav1.GetOptions{})
	if err != nil {
		e.t.Fatal(err)
	}
	cur.Data = map[string]string{}
	for gvr, sum := range members {
		cur.Data[keyOf(gvr)] = entry(e.t, keyOf(gvr)+"-configmap", sum)
	}
	if _, err := e.cs.CoreV1().ConfigMaps(drainNS).Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *poolEnv) holder(gvr schema.GroupVersionResource) string {
	l, err := e.cs.CoordinationV1().Leases(drainNS).Get(context.Background(), leaseOf(gvr), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return ""
	}
	if err != nil {
		return "unreadable: " + err.Error()
	}
	if l.Spec.HolderIdentity == nil {
		return ""
	}
	return *l.Spec.HolderIdentity
}

// readyz probes as the kubelet does, with its 5s timeout (values.yaml cdc.readinessProbe).
func (e *poolEnv) readyz() (code int, body string) {
	e.t.Helper()
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(e.readyURL)
	if err != nil {
		e.t.Fatalf("/readyz did not answer the probe: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (e *poolEnv) eventuallyReady(what string) {
	e.t.Helper()
	eventually(e.t, what, func() bool { code, _ := e.readyz(); return code == http.StatusOK })
}

// busy gives gvr a composition whose reconcile is held, so gvr's controller cannot drain.
func (e *poolEnv) busy(gvr schema.GroupVersionResource) {
	e.t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvr.GroupVersion().WithKind(kindOf(gvr)))
	u.SetName("one")
	u.SetNamespace("default")
	u.SetLabels(map[string]string{meta.CompositionVersionLabel: gvr.Version})
	u.SetFinalizers([]string{"composition.krateo.io/finalizer"}) // the runtime adds it through an in-cluster client
	if _, err := e.dyn.Resource(gvr).Namespace("default").Create(context.Background(), u, metav1.CreateOptions{}); err != nil {
		e.t.Fatal(err)
	}
	eventually(e.t, gvr.Resource+"'s reconcile is in flight", func() bool { return e.get.inFlight.Load() == 1 })
}

var (
	slow  = schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1-0-0", Resource: "slowpages"}
	quick = schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1-0-0", Resource: "quickpages"}
	later = schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1-0-0", Resource: "laterpages"}
)

// One member's configuration changes while its reconcile is in flight, so it drains for as long as the
// test holds it. The probe keeps answering, ready: a member's drain is that member's (#653); a member joins and reconciles; a member leaves and releases
// its Lease; the slow member leaves and rejoins. When it has drained, exactly one successor starts, with
// the Lease free, and the pool is Ready.
func TestEnvtest_PoolDrainBlocksNobody(t *testing.T) {
	e := startPoolEnv(t, 60*time.Second, slow, quick, later)
	e.index(map[schema.GroupVersionResource]string{slow: "1", quick: "1"})
	e.eventuallyReady("both members ready")
	e.busy(slow)
	eventually(t, "quick holds its Lease", func() bool { return e.holder(quick) == drainPod })

	e.index(map[schema.GroupVersionResource]string{slow: "2", quick: "1", later: "1"}) // slow is replaced, later joins
	eventually(t, "the pool's own account names the draining member", func() bool {
		err := e.pool.membersReady(nil)
		return err != nil && strings.Contains(err.Error(), keyOf(slow)) && strings.Contains(err.Error(), "draining")
	})
	if code, body := e.readyz(); code != http.StatusOK {
		t.Errorf("/readyz = %d %q while one member drains: one member decided the pod's readiness", code, body)
	}
	eventually(t, "later runs while slow drains", func() bool { return e.holder(later) == drainPod && e.log.count(later, "hold") == 1 })

	e.index(map[schema.GroupVersionResource]string{slow: "2", later: "1"}) // quick leaves
	eventually(t, "quick released its Lease while slow drains", func() bool { return e.log.count(quick, "release") == 1 && e.holder(quick) == "" })

	e.index(map[schema.GroupVersionResource]string{later: "1"}) // slow leaves, still draining
	e.eventuallyReady("ready: a member that left does not count while it drains")
	if got, want := e.pool.Members(), []string{keyOf(later)}; !reflect.DeepEqual(got, want) {
		t.Errorf("Members() = %v, want %v", got, want)
	}
	e.index(map[schema.GroupVersionResource]string{slow: "3", later: "1"}) // and rejoins
	eventually(t, "the rejoined member waits for its drain", func() bool { return e.pool.membersReady(nil) != nil })
	if code, body := e.readyz(); code != http.StatusOK {
		t.Errorf("/readyz = %d %q for a member rejoining", code, body)
	}
	if n, h := e.log.count(slow, "start"), e.holder(slow); n != 1 || h != drainPod || e.log.count(slow, "release") != 0 {
		t.Fatalf("slow must still be draining its one controller, holding its Lease: %d starts, holder %q", n, h)
	}

	e.get.let()
	e.eventuallyReady("the slow member's successor is ready")
	if n := e.log.count(slow, "start"); n != 2 {
		t.Errorf("slow started %d controllers, want 2: the first, and one for the last of its three index changes", n)
	}
	if n := e.log.count(later, "start"); n != 1 {
		t.Errorf("later was disturbed: started %d times", n)
	}
	time.Sleep(time.Second) // past informer startup, as TestEnvtest_TwoMembersInOneProcess explains
	if err := e.stop(); err != nil {
		t.Errorf("Run returned %v on a clean shutdown", err)
	}
	for _, gvr := range []schema.GroupVersionResource{slow, later} {
		if h := e.holder(gvr); h != "" {
			t.Errorf("%s: Lease still held by %q after exit", gvr.Resource, h)
		}
	}
}

// A drain that outlives its grace (a helm operation longer than GracefulShutdownTimeout): the runtime
// stops waiting, releases the Lease, and the key restarts, alone. What it does NOT do is stop the
// reconcile that ignored its context; see docs/design/cdc-pooling.md §5c "A drain that outlives its
// grace". The peak is logged, not asserted: it is the runtime's contract, not the pool's.
func TestEnvtest_PoolDrainThatOutlivesItsGrace(t *testing.T) {
	const grace = 2 * time.Second
	e := startPoolEnv(t, grace, slow, quick)
	e.index(map[schema.GroupVersionResource]string{slow: "1", quick: "1"})
	e.eventuallyReady("both members ready")
	e.busy(slow)
	start := time.Now()
	e.index(map[schema.GroupVersionResource]string{slow: "2", quick: "1"})
	eventually(t, "the pool's account names the draining member while /readyz stays ready", func() bool {
		err := e.pool.membersReady(nil)
		code, _ := e.readyz()
		return err != nil && strings.Contains(err.Error(), "draining") && code == http.StatusOK
	})
	eventually(t, "the key restarted although its predecessor's reconcile never ended", func() bool { return e.log.count(slow, "start") == 2 })
	e.eventuallyReady("the successor is ready")
	if d := time.Since(start); d < grace {
		t.Errorf("the successor was ready after %s, inside its predecessor's %s drain", d, grace)
	}
	if e.log.count(slow, "grace expired") != 1 || e.log.count(slow, "release") != 1 {
		t.Error("control: the predecessor was to exceed its grace, then release its Lease")
	}
	if n := e.log.count(quick, "start"); n != 1 {
		t.Errorf("quick was disturbed: started %d times", n)
	}
	eventually(t, "the successor holds the Lease", func() bool { return e.log.count(slow, "hold") == 2 })
	time.Sleep(time.Second)
	t.Logf("reconciles of one composition in flight at once, in one process: now %d, peak %d", e.get.inFlight.Load(), e.get.peak.Load())
}

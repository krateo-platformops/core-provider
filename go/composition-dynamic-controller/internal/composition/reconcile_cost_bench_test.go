package composition

// The missing instrument for #202.
//
// Every memory claim on #202 so far has been structural: a count of the materializations
// hc.Reconcile performs per call, plus pprof shares naming render and merge symbols. That argument
// puts the dominant term three to four orders of magnitude above the DecodeRelease path the issue
// was originally chasing — but it is an argument, not a measurement, and nothing in this repo could
// measure it, because there was no benchmark for hc.Reconcile and no chart fixture to feed one.
//
// This is that benchmark. It reports ALLOCATED BYTES PER RECONCILE against a real apiserver with a
// real installed release, which is the unit the fix candidates have to move.
//
// WHY PER-RECONCILE AND NOT PER-HOUR, since the issue is stated in GB/h. The leading fix candidate
// changes the RECONCILE RATE (drive self-heal from a watch on the children, demote the 60s timer to
// a backstop). A GB/h figure would fall for free under that change and prove nothing about the cost
// of a render. So the denominator has to be the reconcile, and b.N supplies it honestly.
//
// WHY NOT RSS. GOMEMLIMIT (#247) pins the arena, so less allocation shows up as lower GC pressure at
// the SAME RSS, and `kubectl top` cannot see it at all — the instrument error that produced the
// retracted "115Mi max" on the issue. b.ReportAllocs measures what is actually allocated.
//
// THE PRODUCTION CLIENT SHAPE MATTERS, AND GETTING IT WRONG WOULD RE-MEASURE A SOLVED PROBLEM.
// helm.NewClient without WithCachedClients falls back to plumbing's uncached RESTClientGetter, whose
// ToRESTMapper does a full API discovery per client. That was 45.3% of allocation on krateo-057 and
// it is ALREADY FIXED (handler.cachedClients, documented at composition.go:180-200). A benchmark that
// omitted the option would measure the fix's absence and report a cause that no longer exists. So
// this harness passes WithCachedClients exactly as the five production call sites do.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
	"github.com/krateo-platformops/plumbing/helm/v3"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chartutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// No shared namespace constant on purpose: each arm gets its own (see createNamespace).

// childTemplate renders one ConfigMap per index. ConfigMaps are deliberate: they are a native kind
// the apiserver serves without any CRD, so the per-child cost measured here is the cost of helm
// handling a child, not of establishing a custom resource. The real charts on 057 render Deployments
// and CRDs, which are strictly more expensive — so a figure from this fixture is a FLOOR.
const childTemplate = `apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .Release.Name }}-child-%d
data:
  payload: %q
`

// buildChartArchive writes a chart with `children` templates, each carrying payloadKB of data, and
// returns the .tgz path.
//
// payloadKB is a SEPARATE DIMENSION from the child count on purpose. The first run of this benchmark
// measured a fixed cost of 17.94 MB per reconcile — 97.6% of a one-child reconcile, which confirmed
// the mechanism — but x60/h came to 1.13 GB/h against the 341 GB/h measured on the fleet, a 302x gap.
// The candidate explanation is manifest SIZE rather than child count, and the only way to tell them
// apart is to vary one with the other held fixed. Varying both at once would have produced a curve
// that fit either story.
func buildChartArchive(t testing.TB, dir string, children, payloadKB int) string {
	t.Helper()

	payload := strings.Repeat("x", payloadKB*1024)

	ch := &chart.Chart{
		Metadata: &chart.Metadata{
			APIVersion: chart.APIVersionV1,
			Name:       "bench202",
			Version:    "0.1.0",
		},
	}
	for i := 0; i < children; i++ {
		ch.Templates = append(ch.Templates, &chart.File{
			Name: fmt.Sprintf("templates/child-%d.yaml", i),
			Data: []byte(fmt.Sprintf(childTemplate, i, payload)),
		})
	}

	path, err := chartutil.Save(ch, dir)
	if err != nil {
		t.Fatalf("saving chart archive: %v", err)
	}
	return path
}

// serveChart serves the archive over HTTP, which is what production does.
//
// Not a local path: plumbing's loadChart goes through its own getter, and the DiskCache that
// production enables means the FETCH is cached while the PARSE is not. Serving over HTTP with the
// cache in play reproduces that exactly — which is the whole point, because the claim under test is
// that loader.LoadArchive re-parses the gzip+tar on every single reconcile.
func serveChart(t testing.TB, archive string) string {
	t.Helper()
	dir := filepath.Dir(archive)
	srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
	t.Cleanup(srv.Close)
	return srv.URL + "/" + filepath.Base(archive)
}

// startAPIServer brings up envtest and returns a rest.Config.
func startAPIServer(t testing.TB) *rest.Config {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; see the header of crdwatch_envtest_test.go")
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("starting envtest: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	return cfg
}

// createNamespace makes one namespace per arm.
//
// PER-ARM, because the first run of this benchmark shared one namespace across all three arms and
// each inherited the previous arm's ConfigMaps — the controls printed "11/10" and "61/50" children
// live. Isolation is cheaper than reasoning about what leaked.
func createNamespace(b *testing.B, cs *kubernetes.Clientset, name string) {
	b.Helper()
	_, err := cs.CoreV1().Namespaces().Create(context.Background(),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{})
	if err != nil {
		b.Fatalf("creating namespace %s: %v", name, err)
	}
}

// arm is one fully installed fixture, set up ONCE in the parent benchmark.
//
// Setup lives in the parent rather than inside b.Run because the testing package re-invokes a
// b.Run closure for each calibration round (b.N=1, then larger). The first version installed the
// release inside the closure, so the second round failed with "cannot re-use a name that is still
// in use" — the benchmark body must be safe to run repeatedly, which means it may contain nothing
// but the timed work.
type arm struct {
	label     string
	children  int
	payloadKB int
	namespace string
	release   string
	chartURL  string
}

// benchArms is the experimental design, and the two groups answer different questions.
//
// GROUP 1 varies child count at a fixed 1KB payload — the original three arms, reproduced so the
// new run is comparable with the recorded one rather than only with itself.
//
// GROUP 2 holds the child count at 1 and varies the payload from 1KB to 1MB. One child means the
// per-child terms are constant across the whole group, so any growth is attributable to manifest
// SIZE alone. That is the 302x gap's leading candidate, and this is the arm that can refute it: if
// a 1MB manifest costs about what a 1KB one costs, size is not the missing variable and the gap
// lies somewhere I have not looked.
var benchArms = []struct {
	label     string
	children  int
	payloadKB int
}{
	{"children=1/payload=1KB", 1, 1},
	{"children=10/payload=1KB", 10, 1},
	{"children=50/payload=1KB", 50, 1},
	{"children=1/payload=64KB", 1, 64},
	{"children=1/payload=256KB", 1, 256},
	{"children=1/payload=1MB", 1, 1024},
}

// BenchmarkReconcileCost measures allocated bytes per hc.Reconcile at several child counts.
//
// READ THE CONTROLS BEFORE READING THE NUMBERS. A Reconcile that errors early, or one against a
// release with no children, is FAST — and a fast result here would read as good news while actually
// meaning the instrument measured nothing. So each arm asserts, before timing, that the release
// exists and that the expected number of children are live in the cluster. A zero or a surprisingly
// small figure with those assertions passing is a finding; without them it is an artifact.
func BenchmarkReconcileCost(b *testing.B) {
	cfg := startAPIServer(b)

	// NewCachedClients returns a VALUE; WithCachedClients takes a pointer. Taking the address of the
	// one shared value is the point — a per-arm copy would reintroduce the per-client discovery this
	// benchmark exists not to measure.
	cached, err := helm.NewCachedClients(cfg)
	if err != nil {
		b.Fatalf("building shared discovery (the #202 fix this benchmark must NOT re-measure): %v", err)
	}

	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		b.Fatalf("clientset: %v", err)
	}

	ctx := context.Background()

	// SETUP ALL ARMS FIRST, in the parent, where it runs exactly once.
	var arms []arm
	for i, spec := range benchArms {
		a := arm{
			label:     spec.label,
			children:  spec.children,
			payloadKB: spec.payloadKB,
			namespace: fmt.Sprintf("bench-202-a%d", i),
			release:   fmt.Sprintf("bench-a%d", i),
			chartURL:  serveChart(b, buildChartArchive(b, b.TempDir(), spec.children, spec.payloadKB)),
		}
		createNamespace(b, cs, a.namespace)

		hc, err := helm.NewClient(cfg, helm.WithCachedClients(&cached), helm.WithNamespace(a.namespace))
		if err != nil {
			b.Fatalf("helm client for %s: %v", a.namespace, err)
		}
		if _, err := hc.Install(ctx, a.release, a.chartURL, &helmconfig.InstallConfig{
			Namespace:    a.namespace,
			ActionConfig: &helmconfig.ActionConfig{ChartVersion: "0.1.0"},
		}); err != nil {
			hc.Close()
			b.Fatalf("installing fixture %s: %v", a.release, err)
		}

		// CONTROL 1 — the release must exist. Without this, a Reconcile that bails with "no
		// deployed releases" would be timed as if it had done the work.
		rel, err := hc.GetRelease(ctx, a.release, &helmconfig.GetConfig{Namespace: a.namespace})
		if err != nil || rel == nil {
			hc.Close()
			b.Fatalf("CONTROL FAILED: release %s absent after install (err=%v) — a Reconcile against "+
				"no release is cheap and would read as a low cost per reconcile", a.release, err)
		}
		hc.Close()

		// CONTROL 2 — EXACTLY the expected children must be live. Equality, not a floor.
		//
		// The first version asserted `live < children` and passed while printing "11/10" and "61/50",
		// because the arms shared a namespace and each inherited the previous one's ConfigMaps. A
		// one-sided assertion cannot tell "rendered correctly" from "contaminated by a neighbour" —
		// only the printed denominator revealed it. Both bounds now, in an isolated namespace.
		cms, err := cs.CoreV1().ConfigMaps(a.namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			b.Fatalf("CONTROL FAILED: listing children in %s: %v", a.namespace, err)
		}
		live, payloadBytes := 0, 0
		for _, cm := range cms.Items {
			if p := cm.Data["payload"]; p != "" {
				live++
				payloadBytes += len(p)
			}
		}
		if live != a.children {
			b.Fatalf("CONTROL FAILED: %d children live in %s, want EXACTLY %d — the fixture did not "+
				"render what this arm claims to measure, so any per-child figure from it is wrong",
				live, a.namespace, a.children)
		}

		// CONTROL 2b — the PAYLOAD must be the size this arm claims.
		//
		// The size group is worthless without this. A payload silently truncated anywhere between
		// the template and the apiserver would make every size arm measure the same small manifest,
		// and the result — "size does not matter" — would refute the leading hypothesis for the 302x
		// gap on the strength of a broken fixture. Same class of hole as the `live < children` floor
		// that let contamination through: an assertion about presence says nothing about magnitude.
		wantBytes := a.children * a.payloadKB * 1024
		if payloadBytes != wantBytes {
			b.Fatalf("CONTROL FAILED: %d payload bytes live in %s, want EXACTLY %d (%d children x %dKB) "+
				"— this arm cannot measure a manifest-size effect it did not actually create",
				payloadBytes, a.namespace, wantBytes, a.children, a.payloadKB)
		}
		b.Logf("arm %s ready: release present, %d/%d children live, %d payload bytes in %s",
			a.label, live, a.children, payloadBytes, a.namespace)

		arms = append(arms, a)
	}

	// THEN time each arm. The closure below contains nothing but the measured work, so it is safe
	// for the testing package to re-invoke per calibration round.
	for _, a := range arms {
		b.Run(a.label, func(b *testing.B) {
			hc, err := helm.NewClient(cfg,
				helm.WithCachedClients(&cached),
				helm.WithNamespace(a.namespace))
			if err != nil {
				b.Fatalf("helm client: %v", err)
			}
			defer hc.Close()

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				res, err := hc.Reconcile(ctx, a.release, a.chartURL, &helmconfig.UpgradeConfig{
					ActionConfig: &helmconfig.ActionConfig{
						ChartVersion:  "0.1.0",
						TakeOwnership: true,
					},
					MaxHistory: 10,
				})
				if err != nil {
					b.Fatalf("reconcile %d/%d: %v", i, b.N, err)
				}
				// CONTROL 3, inside the loop — a steady-state reconcile must report NO change.
				// If it reports a change every time, the fixture is oscillating and the figure is
				// the cost of a real upgrade rather than of a no-op convergence check, which is
				// what the fleet actually pays 60 times an hour.
				if res != nil && res.Changed && i > 0 {
					b.Fatalf("CONTROL FAILED at iteration %d: reconcile reports Changed on an "+
						"unchanged release — this arm is measuring an upgrade, not a steady-state "+
						"convergence check", i)
				}
			}
		})
	}
}

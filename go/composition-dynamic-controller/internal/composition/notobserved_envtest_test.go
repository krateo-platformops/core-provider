//go:build envtest

// Deterministic verification of #310 — the cdc treating provider-runtime's Ready=Unknown/NotObserved
// as UNEVALUATED rather than converging — against a real apiserver, with the NotObserved condition
// written by the REAL provider-runtime rather than patched in by this test.
//
// # Why this test exists at all
//
// The acceptance criterion #310 shipped with is not observable on a healthy cluster, and that is
// structural rather than bad luck. installer rolled alert-provider 0.2.60 (the first v1.8.2-built
// provider) on demo with a corpus checked BEFORE the roll so a zero could not be blamed on an empty
// one — 27 Alert CRs under 4 parent compositions — and took 21 samples across the upgrade:
//
//	every sample: alerts=27 {'True/Available': 27}
//	NotObserved children caught: 0        eligible/excluded/total: 0/0/0
//	OUTCOME: NEVER CAUGHT -- the absence of an opportunity to test, NOT a pass
//
// A child reads Ready=Unknown/NotObserved only when its provider's Observe FAILS. A rollout where
// nothing fails cannot produce the condition. So each of the remaining adopters will report NEVER
// CAUGHT too, and the danger installer named is the right one: not one test that cannot fail, but a
// SERIES of honest non-results whose accumulation reads as a track record.
//
// This test replaces that criterion with one that has a denominator.
//
// # Why the condition is not patched in
//
// Writing Unknown/NotObserved into a child's status directly would verify that the cdc maps a
// string. It would not verify that provider-runtime PRODUCES that string, and the claim worth
// testing is the pair — a rename on either side is the regression this is for. So the condition
// here comes from provider-runtime v1.8.2's own error path, in a separate process, reconciling real
// CRs against this apiserver.
//
// # Why the child's group matters, and would silently void this test if changed
//
// classifyChild dispatches on strings.HasSuffix(group, "krateo.io") (childhealth.go). The
// verification kind is e2e.krateo.io/v1alpha1, so its children route to krateoReady, which is where
// the NotObserved arm lives. A group NOT ending in krateo.io falls through to existence-is-health,
// every arm below would report the parent healthy, and the suite would pass while testing nothing.
// TestNotObservedEnvtest_GroupDispatchIsLoadBearing pins that.
//
//	Run: KUBEBUILDER_ASSETS=$(setup-envtest use -p path 1.36.0) \
//	go test -tags envtest ./internal/composition/ -run NotObservedEnvtest -v
package composition

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	apixv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicclient "k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

const (
	childGroup    = "e2e.krateo.io"
	childVersion  = "v1alpha1"
	childKind     = "TestChild"
	childResource = "testchildren"
	childNS       = "default"
)

// The two control kinds. Separate kinds rather than separate objects of the same kind, because the
// provider watches the kind, not the object.
const (
	unmappedGroup    = "control.krateo.io"
	unmappedResource = "unmanagedchildren"
	unmappedKind     = "UnmanagedChild"

	nonKrateoGroup    = "control.example.com"
	nonKrateoResource = "foreignchildren"
	nonKrateoKind     = "ForeignChild"
)

var (
	childGVR     = schema.GroupVersionResource{Group: childGroup, Version: childVersion, Resource: childResource}
	unmappedGVR  = schema.GroupVersionResource{Group: unmappedGroup, Version: childVersion, Resource: unmappedResource}
	nonKrateoGVR = schema.GroupVersionResource{Group: nonKrateoGroup, Version: childVersion, Resource: nonKrateoResource}
)

// ---------------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------------

type harness struct {
	cfg *rest.Config
	dyn dynamicclient.Interface
}

// ONE apiserver and ONE provider process for the whole suite, built once.
//
// Per-test setup was the first thing I wrote and it was wrong on cost, not on correctness: each arm
// paid for an apiserver start, a `go build` and a provider cold start, and one arm measured 141.76s.
// Six of those overran the suite timeout — so the suite FAILED while every arm that ran passed,
// which is a worse failure than a red assertion because the cause is nowhere in the output.
//
// Arms share the apiserver and use distinct child names, so they remain independent of each other's
// objects without re-paying the fixed cost.
var (
	sharedOnce    sync.Once
	sharedHarness *harness
	sharedEnv     *envtest.Environment
	sharedStop    func()
	sharedErr     error
)

// TestMain exists ONLY to tear the shared apiserver down. Setup stays lazy in getHarness so that a
// missing KUBEBUILDER_ASSETS still produces a per-test `--- SKIP`, which the CI job greps for and
// fails on: a TestMain that exited 0 on missing assets would turn a misconfigured runner into a
// silent green, which is the exact shape this suite exists to eliminate.
func TestMain(m *testing.M) {
	code := m.Run()
	if sharedStop != nil {
		sharedStop()
	}
	if sharedEnv != nil {
		_ = sharedEnv.Stop()
	}
	os.Exit(code)
}

func getHarness(t *testing.T) *harness {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via setup-envtest")
	}
	sharedOnce.Do(func() {
		sharedHarness, sharedErr = buildHarness(t)
	})
	if sharedErr != nil {
		t.Fatalf("shared harness: %v", sharedErr)
	}
	return sharedHarness
}

func buildHarness(t *testing.T) (*harness, error) {
	sharedEnv = &envtest.Environment{}
	cfg, err := sharedEnv.Start()
	if err != nil {
		return nil, fmt.Errorf("starting envtest: %w", err)
	}

	if err := installChildCRD(cfg); err != nil {
		return nil, fmt.Errorf("installing the %s CRD: %w", childKind, err)
	}

	// WAIT FOR Established BEFORE the provider starts, and this is the other half of the 141s.
	// controller-runtime's informer retries a failed initial LIST with backoff, so a provider
	// started against a not-yet-Established CRD can sit in that backoff for tens of seconds while
	// the test waits for a condition it cannot yet write.
	if err := waitCRDEstablished(cfg); err != nil {
		return nil, err
	}

	dyn, err := dynamicclient.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("dynamic client: %w", err)
	}

	h := &harness{cfg: cfg, dyn: dyn}
	sharedStop = startTestProvider(t, sharedEnv, cfg)
	return h, nil
}

// waitCRDEstablished polls the CRD's own Established condition. It FAILS rather than proceeding on
// timeout: continuing would start the provider against a kind the apiserver does not serve yet, and
// every arm would then fail for that reason instead of for its own.
func waitCRDEstablished(cfg *rest.Config) error {
	c, err := apiextclient.NewForConfig(cfg)
	if err != nil {
		return err
	}
	for _, name := range []string{
		childResource + "." + childGroup,
		unmappedResource + "." + unmappedGroup,
		nonKrateoResource + "." + nonKrateoGroup,
	} {
		ok := false
		deadline := time.Now().Add(60 * time.Second)
		for !ok && time.Now().Before(deadline) {
			crd, err := c.ApiextensionsV1().CustomResourceDefinitions().
				Get(context.Background(), name, metav1.GetOptions{})
			if err == nil {
				for _, cond := range crd.Status.Conditions {
					if cond.Type == apixv1.Established && cond.Status == apixv1.ConditionTrue {
						ok = true
					}
				}
			}
			if !ok {
				time.Sleep(200 * time.Millisecond)
			}
		}
		if !ok {
			return fmt.Errorf("CRD %s never became Established", name)
		}
	}
	return nil
}

// installChildCRD registers the verification kind WITH A STATUS SUBRESOURCE. Without the
// subresource provider-runtime's status write is silently dropped by the apiserver and every arm
// below would see a child with no conditions — which classifyChild reports as healthy, so the suite
// would pass vacuously. The schema is x-preserve-unknown so the condition shape stays
// provider-runtime's business rather than being re-declared here.
func installChildCRD(cfg *rest.Config) error {
	for _, k := range []struct{ group, plural, kind string }{
		{childGroup, childResource, childKind},
		// A SECOND krateo.io kind that NO provider reconciles. The unmapped-reason control needs a
		// child sitting at Unknown with a reason nothing maps, and it cannot be a TestChild: the
		// provider polls every second and would overwrite the condition between the patch and the
		// read. Two writers on one field means the control was measuring whichever wrote last —
		// which is exactly how the first version of that control failed.
		{unmappedGroup, unmappedResource, unmappedKind},
		// A kind whose group does NOT end in krateo.io, for the group-dispatch control. It must be
		// a REAL registered kind: a reference to an unregistered group GETs a 404, which
		// classifyChild reads as childAbsent (converging), not as the unmodelled-group
		// fall-through. Those are different code paths and only one of them is the dependency the
		// other arms rest on.
		{nonKrateoGroup, nonKrateoResource, nonKrateoKind},
	} {
		if err := installOneCRD(cfg, k.group, k.plural, k.kind); err != nil {
			return fmt.Errorf("%s/%s: %w", k.group, k.kind, err)
		}
	}
	return nil
}

func installOneCRD(cfg *rest.Config, group, plural, kind string) error {
	c, err := apiextclient.NewForConfig(cfg)
	if err != nil {
		return err
	}
	tru := true
	crd := &apixv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: plural + "." + group},
		Spec: apixv1.CustomResourceDefinitionSpec{
			Group: group,
			Scope: apixv1.NamespaceScoped,
			Names: apixv1.CustomResourceDefinitionNames{
				Plural:   plural,
				Singular: strings.ToLower(kind),
				Kind:     kind,
				ListKind: kind + "List",
			},
			Versions: []apixv1.CustomResourceDefinitionVersion{{
				Name:    childVersion,
				Served:  true,
				Storage: true,
				Subresources: &apixv1.CustomResourceSubresources{
					Status: &apixv1.CustomResourceSubresourceStatus{},
				},
				Schema: &apixv1.CustomResourceValidation{
					OpenAPIV3Schema: &apixv1.JSONSchemaProps{
						Type: "object",
						Properties: map[string]apixv1.JSONSchemaProps{
							"spec": {
								Type: "object",
								Properties: map[string]apixv1.JSONSchemaProps{
									"failObserve": {Type: "boolean"},
									"unready":     {Type: "boolean"},
								},
							},
							"status": {Type: "object", XPreserveUnknownFields: &tru},
						},
					},
				},
			}},
		},
	}
	_, err = c.ApiextensionsV1().CustomResourceDefinitions().
		Create(context.Background(), crd, metav1.CreateOptions{})
	return err
}

// startTestProvider builds and runs go/testprovider as a SUBPROCESS.
//
// A subprocess rather than an in-process reconciler, for two reasons and the first is the binding
// one. (1) go/testprovider is a separate module so that provider-runtime stays out of the cdc's
// dependency graph — the cdc imports it zero times in production, and it has no business in the
// cdc's release builds or its vulnerability surface. A separate module cannot be imported by this
// package at all. (2) It is also closer to production: its own process, its own manager, its own
// cache and watches, talking to the apiserver over the network.
// startTestProvider builds and runs go/testprovider as a SUBPROCESS, once for the whole suite.
//
// A subprocess rather than an in-process reconciler, for two reasons and the first is binding.
// (1) go/testprovider is a separate module so that provider-runtime stays out of the cdc's
// dependency graph — the cdc imports it zero times in production and it has no business in the
// cdc's release builds or its vulnerability surface — and a separate module cannot be imported by
// this package at all. (2) It is also closer to production: its own process, its own manager, its
// own cache and watches, talking to the apiserver over the network.
//
// NOTHING HERE MAY HANG OFF THE FIRST TEST'S *testing.T. t.TempDir() is removed when that test
// ends and t.Cleanup would cancel the provider after it — so the binary and kubeconfig would
// vanish, and every later arm would fail with the provider gone. The lifetime is the suite's, so
// the temp dir is explicit and the cancel is returned for TestMain.
func startTestProvider(t *testing.T, env *envtest.Environment, _ *rest.Config) func() {
	t.Helper()

	dir, err := os.MkdirTemp("", "notobserved-envtest-")
	if err != nil {
		t.Fatalf("temp dir for the test provider: %v", err)
	}

	bin := filepath.Join(dir, "testprovider")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = filepath.Join("..", "..", "..", "testprovider")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the test provider: %v\n%s", err, out)
	}

	// A kubeconfig FILE, because the subprocess loads its config the way a real provider does.
	user, err := env.ControlPlane.AddUser(envtest.User{
		Name:   "testprovider",
		Groups: []string{"system:masters"},
	}, nil)
	if err != nil {
		t.Fatalf("adding the provider's apiserver user: %v", err)
	}
	kc, err := user.KubeConfig()
	if err != nil {
		t.Fatalf("rendering the provider's kubeconfig: %v", err)
	}
	kcPath := filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(kcPath, kc, 0o600); err != nil {
		t.Fatalf("writing the provider's kubeconfig: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, bin, "-poll", "1s")
	cmd.Env = append(os.Environ(), "KUBECONFIG="+kcPath)
	providerLogPath = filepath.Join(dir, "testprovider.log")
	logFile, err := os.Create(providerLogPath)
	if err != nil {
		t.Fatalf("creating the provider log: %v", err)
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the test provider: %v", err)
	}

	return func() {
		cancel()
		_ = cmd.Wait()
		_ = logFile.Close()
		_ = os.RemoveAll(dir)
	}
}

// providerLogPath lets a failing arm print the provider's own output. The provider HAVING STARTED
// is not the provider HAVING RECONCILED, so no sleep is treated as readiness anywhere in this
// suite: every arm polls for the condition it needs and, on timeout, says what the provider was
// doing instead.
var providerLogPath string

func dumpProviderLog(t *testing.T) {
	t.Helper()
	if providerLogPath == "" {
		return
	}
	if b, err := os.ReadFile(providerLogPath); err == nil {
		t.Logf("--- test provider log (tail) ---\n%s", tailBytes(b, 4000))
	}
}

func tailBytes(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[len(b)-n:])
}

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

func (h *harness) createChild(t *testing.T, name string, failObserve, unready bool) {
	t.Helper()
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": childGroup + "/" + childVersion,
		"kind":       childKind,
		"metadata":   map[string]any{"name": name, "namespace": childNS},
		"spec":       map[string]any{"failObserve": failObserve, "unready": unready},
	}}
	if _, err := h.dyn.Resource(childGVR).Namespace(childNS).
		Create(context.Background(), obj, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating child %s: %v", name, err)
	}
}

func (h *harness) setFailObserve(t *testing.T, name string, fail bool) {
	t.Helper()
	patch := fmt.Sprintf(`{"spec":{"failObserve":%t}}`, fail)
	if _, err := h.dyn.Resource(childGVR).Namespace(childNS).Patch(context.Background(),
		name, "application/merge-patch+json", []byte(patch), metav1.PatchOptions{}); err != nil {
		t.Fatalf("patching child %s: %v", name, err)
	}
}

// waitForChildReady polls until the child's Ready condition matches (status, reason).
//
// Polling rather than a fixed wait, and it FAILS rather than proceeding on timeout: a test that
// proceeded with the condition absent would be asserting about a child the provider had not yet
// touched, and the parent would read healthy — a false pass with the right shape.
func (h *harness) waitForChildReady(t *testing.T, name, wantStatus, wantReason string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		obj, err := h.dyn.Resource(childGVR).Namespace(childNS).
			Get(context.Background(), name, metav1.GetOptions{})
		if err == nil {
			conds, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
			for _, c := range conds {
				cm, ok := c.(map[string]any)
				if !ok || stringField(cm, "type") != "Ready" {
					continue
				}
				last = stringField(cm, "status") + "/" + stringField(cm, "reason")
				if stringField(cm, "status") == wantStatus && stringField(cm, "reason") == wantReason {
					return
				}
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	dumpProviderLog(t)
	t.Fatalf("child %s never reached Ready=%s/%s; last observed %q\n\n"+
		"If this is %q, the provider wrote the condition but with a different reason than "+
		"provider-runtime v1.8.2 used to — which is the cross-repo rename this suite exists to "+
		"catch, not a flake to retry", name, wantStatus, wantReason, last, last)
}

// parentWith builds the in-memory parent. It is NOT created in the apiserver, deliberately:
// deriveReadyFromHealth reads status.managed off the object it is handed and stampReady writes the
// condition back onto that same object in memory (support.go). Persisting the parent would add a
// CRD and a round-trip that the path under test does not involve.
func parentWith(children ...string) *unstructured.Unstructured {
	managed := make([]any, 0, len(children))
	for _, name := range children {
		managed = append(managed, map[string]any{
			"apiVersion": childGroup + "/" + childVersion,
			"resource":   childResource,
			"name":       name,
			"namespace":  childNS,
		})
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "composition.krateo.io/v1-0-0",
		"kind":       "Parent",
		"metadata":   map[string]any{"name": "parent", "namespace": childNS},
		"status":     map[string]any{"managed": managed},
	}}
}

// rollup drives the REAL production chain: deriveReadyFromHealth builds a dynamic client from
// h.kubeconfig, calls rollupManagedChildren, folds the verdict through resolveReady and writes it
// with stampReady (support.go:374-377, :70). Nothing about the mapping is reconstructed here.
func (h *harness) rollup(t *testing.T, mg *unstructured.Unstructured) (status, reason string) {
	t.Helper()
	hd := &handler{kubeconfig: h.cfg}
	if err := hd.deriveReadyFromHealth(context.Background(), mg, &statusManagerOpts{}); err != nil {
		t.Fatalf("deriveReadyFromHealth: %v", err)
	}
	conds, _, _ := unstructured.NestedSlice(mg.Object, "status", "conditions")
	for _, c := range conds {
		if cm, ok := c.(map[string]any); ok && stringField(cm, "type") == "Ready" {
			return stringField(cm, "status"), stringField(cm, "reason")
		}
	}
	t.Fatal("the parent carries no Ready condition after deriveReadyFromHealth")
	return "", ""
}

// ---------------------------------------------------------------------------
// the arms
// ---------------------------------------------------------------------------

// POSITIVE CONTROL, and it runs first on purpose. If this arm fails, every other arm's result is
// uninterpretable: an instrument that cannot report a healthy parent is not measuring health.
func TestNotObservedEnvtest_AllHealthyIsAvailable(t *testing.T) {
	h := getHarness(t)
	h.createChild(t, "ok-a", false, false)
	h.createChild(t, "ok-b", false, false)
	h.waitForChildReady(t, "ok-a", "True", "Available")
	h.waitForChildReady(t, "ok-b", "True", "Available")

	status, reason := h.rollup(t, parentWith("ok-a", "ok-b"))
	if status != "True" {
		t.Errorf("parent is %s/%s with every child Available; the rollup cannot report health",
			status, reason)
	}
}

// ARM (a) — THE DEFECT. One child NotObserved, siblings healthy: the parent must stay Ready=True.
//
// Before #310 this reported False/Creating, because NotObserved fell to krateoReady's default arm
// and counted as converging. A provider restarting mid-rollout would have turned every composition
// using its kind Ready=False at once.
func TestNotObservedEnvtest_OneNotObservedSiblingHealthyStaysTrue(t *testing.T) {
	h := getHarness(t)
	h.createChild(t, "bad", true, false)
	h.createChild(t, "good", false, false)
	h.waitForChildReady(t, "bad", "Unknown", "NotObserved")
	h.waitForChildReady(t, "good", "True", "Available")

	status, reason := h.rollup(t, parentWith("bad", "good"))
	if status != "True" {
		t.Errorf("parent is %s/%s; one NotObserved child with a healthy sibling must leave the "+
			"parent Ready=True (Available or AvailablePartialCoverage). %s/%s is the pre-#310 "+
			"behaviour: NotObserved counted as converging", status, reason, status, reason)
	}
	if reason != "Available" && reason != "AvailablePartialCoverage" {
		t.Errorf("parent reason %q is Ready=True but outside the documented pair", reason)
	}
}

// ARM (b) — every child NotObserved: the parent reports Unknown/ZeroEvidence, not False.
//
// Unknown rather than False is the whole point. False wedges every consumer gating on readiness,
// which is the direction that turns an honesty fix into an outage; Unknown with this exact reason is
// what installer's readiness gate accepts (_helpers.tpl).
func TestNotObservedEnvtest_AllNotObservedIsZeroEvidence(t *testing.T) {
	h := getHarness(t)
	h.createChild(t, "bad-1", true, false)
	h.createChild(t, "bad-2", true, false)
	h.waitForChildReady(t, "bad-1", "Unknown", "NotObserved")
	h.waitForChildReady(t, "bad-2", "Unknown", "NotObserved")

	status, reason := h.rollup(t, parentWith("bad-1", "bad-2"))
	if status != "Unknown" || reason != "ZeroEvidence" {
		t.Errorf("parent is %s/%s, want Unknown/ZeroEvidence. If this is False/anything, the "+
			"unevaluated==total path is not being reached and every consumer gating on readiness "+
			"would wedge", status, reason)
	}
}

// ARM (c) — the MIXED PARENT, which the #310 criterion EXCLUDES rather than judges.
//
// One NotObserved child plus a sibling genuinely converging. The parent reports False/Creating —
// correctly, because of the Creating sibling, which has nothing to do with NotObserved. This arm
// exists so that a mixed parent is never recorded as a #310 violation: without it, a checker seeing
// False/Creating next to a NotObserved child would file a false failure, and the hole was in the
// acceptance criterion rather than in the code.
//
// The sibling's False/Creating must come from a SUCCESSFUL Observe (spec.unready), not a failing
// one — a failing Observe would produce NotObserved again and collapse the two states this arm
// exists to tell apart.
func TestNotObservedEnvtest_MixedParentIsExcludedNotJudged(t *testing.T) {
	h := getHarness(t)
	h.createChild(t, "mix-bad", true, false)
	h.createChild(t, "mix-creating", false, true)
	h.waitForChildReady(t, "mix-bad", "Unknown", "NotObserved")
	h.waitForChildReady(t, "mix-creating", "False", "Creating")

	status, reason := h.rollup(t, parentWith("mix-bad", "mix-creating"))
	if status != "False" || reason != "Creating" {
		t.Errorf("parent is %s/%s, want False/Creating. This arm asserts the mixed case is "+
			"EXCLUDED from the #310 criterion, not that it passes it — the Creating sibling is "+
			"what drives False and it is unrelated to NotObserved", status, reason)
	}
}

// ARM (d) — RECOVERY. When Observe succeeds again the parent returns to True/Available.
//
// A one-way mapping would pass arms (a) and (b) and still leave a composition stuck at the verdict
// it reached while its provider was down.
func TestNotObservedEnvtest_RecoversWhenObserveSucceedsAgain(t *testing.T) {
	h := getHarness(t)
	h.createChild(t, "flap", true, false)
	h.waitForChildReady(t, "flap", "Unknown", "NotObserved")

	if status, reason := h.rollup(t, parentWith("flap")); status != "Unknown" || reason != "ZeroEvidence" {
		t.Fatalf("precondition: parent is %s/%s, want Unknown/ZeroEvidence before recovery",
			status, reason)
	}

	h.setFailObserve(t, "flap", false)
	h.waitForChildReady(t, "flap", "True", "Available")

	status, reason := h.rollup(t, parentWith("flap"))
	if status != "True" || reason != "Available" {
		t.Errorf("parent is %s/%s after the child recovered, want True/Available; the mapping is "+
			"one-way and a composition would stay at its outage-time verdict", status, reason)
	}
}

// createControlChild creates an object of a kind NO provider reconciles, carrying the Ready
// condition given. Writing the condition by hand is acceptable here and only here: these are the
// control arms, and a control may construct the thing the measurement observes. The measurement
// arms above never do — their conditions come from provider-runtime.
func (h *harness) createControlChild(t *testing.T, gvr schema.GroupVersionResource, kind, name, status, reason string) {
	t.Helper()
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": gvr.Group + "/" + gvr.Version,
		"kind":       kind,
		"metadata":   map[string]any{"name": name, "namespace": childNS},
	}}
	created, err := h.dyn.Resource(gvr).Namespace(childNS).
		Create(context.Background(), obj, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating control child %s/%s: %v", kind, name, err)
	}
	if err := unstructured.SetNestedSlice(created.Object, []any{map[string]any{
		"type": "Ready", "status": status, "reason": reason,
		"lastTransitionTime": "2026-11-04T00:00:00Z",
	}}, "status", "conditions"); err != nil {
		t.Fatalf("building the control condition: %v", err)
	}
	if _, err := h.dyn.Resource(gvr).Namespace(childNS).
		UpdateStatus(context.Background(), created, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("writing control child %s/%s status: %v", kind, name, err)
	}
	// Read it back. An UpdateStatus that silently did nothing would leave the child with no
	// conditions, which classifyChild reports as HEALTHY — so both controls would pass for the
	// wrong reason and stop constraining anything.
	got, err := h.dyn.Resource(gvr).Namespace(childNS).
		Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading back control child %s/%s: %v", kind, name, err)
	}
	conds, found, _ := unstructured.NestedSlice(got.Object, "status", "conditions")
	if !found || len(conds) != 1 {
		t.Fatalf("control child %s/%s carries %d conditions after UpdateStatus, want 1 — the "+
			"status write did not stick and this control would pass vacuously", kind, name, len(conds))
	}
}

func controlRef(gvr schema.GroupVersionResource, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "composition.krateo.io/v1-0-0",
		"kind":       "Parent",
		"metadata":   map[string]any{"name": "parent", "namespace": childNS},
		"status": map[string]any{"managed": []any{map[string]any{
			"apiVersion": gvr.Group + "/" + gvr.Version,
			"resource":   gvr.Resource,
			"name":       name,
			"namespace":  childNS,
		}}},
	}}
}

// CONTROL — the rollup is sensitive to the REASON, not to bare Unknown.
//
// This is the in-suite stand-in for "run arm (a) with the #310 branch removed" (the mechanical
// version of that lives in CI, which can edit the source). A child under a krateo.io group at
// Unknown with a reason NOTHING maps must drive the parent away from True. If it reports True, the
// rollup is excusing any Unknown and the NotObserved arms above prove nothing about NotObserved.
//
// It uses UnmanagedChild rather than TestChild deliberately. The first version of this control used
// a TestChild and patched its status — and FAILED, because the provider polls every second and
// overwrote the patched condition before the rollup read it. Two writers on one field means the
// control measured whichever wrote last, which is a false negative dressed as a result.
func TestNotObservedEnvtest_UnknownWithAnotherReasonIsNotExcused(t *testing.T) {
	h := getHarness(t)
	h.createControlChild(t, unmappedGVR, unmappedKind, "unmapped", "Unknown", "SomeReasonNobodyMaps")

	status, reason := h.rollup(t, controlRef(unmappedGVR, "unmapped"))
	if status == "True" {
		t.Errorf("parent is %s/%s for a child at Unknown/SomeReasonNobodyMaps under a krateo.io "+
			"group. The rollup is excusing ANY Unknown, so the NotObserved arms prove nothing "+
			"about NotObserved specifically", status, reason)
	}
}

// CONTROL — the krateo.io group suffix is load-bearing, and the first two versions of this arm
// were built on a premise that turned out to be FALSE. The arm is kept in its corrected form
// because what it refuted is worth having written down.
//
// What I believed: a child whose group does not end in krateo.io falls through to
// existence-is-health, so the same condition under a different suffix reports HEALTHY.
//
// What classifyChild actually does: existence-is-health applies only to NATIVE groups
// (isNativeGroup — ConfigMap, Service, RBAC, ...). The default arm, for "a CUSTOM resource nobody
// told us how to read", returns childUnevaluated. So a non-krateo.io custom child at
// Unknown/NotObserved is unevaluated too, and BOTH groups produce Unknown/ZeroEvidence: that
// condition does not discriminate, and the version of this arm that used it failed telling me so.
//
// The suffix IS load-bearing, for a different condition: Ready=True.
//
//	krateo.io group       Ready=True -> krateoReady    -> childHealthy
//	other custom group    Ready=True -> default arm    -> childUnevaluated
//
// Which names precisely which arms above depend on the suffix: the ones with a HEALTHY SIBLING. A
// healthy sibling under a non-krateo.io group would be unevaluated rather than healthy, and arm (a)
// would read Unknown/ZeroEvidence instead of True — it would fail rather than pass vacuously, which
// is a better failure mode than I had assumed but still not the one the arm claims to test.
func TestNotObservedEnvtest_GroupDispatchIsLoadBearing(t *testing.T) {
	// The arms above use krateo.io-group children, so they depend on krateoReady being reached. This
	// proves the group decides it, with an input on which the two arms disagree.
	//
	// It used Ready=True until #399: a non-krateo custom child was then unevaluated (ZeroEvidence)
	// while a krateo one was True. #399 reads a third party's Ready condition, so at Ready=True the
	// two now AGREE (True), and that input proves nothing about dispatch. Re-read on #399: no arm
	// above uses a non-krateo child, so none of them changed.
	//
	// Ready=Unknown/NotObserved still splits them. krateoReady treats NotObserved as "nothing assessed
	// it" (unevaluated, so the lone child's parent is Unknown/ZeroEvidence), while thirdPartyReady
	// knows no vocabulary of a third party's and reads any non-True Ready as converging (False).
	h := getHarness(t)
	h.createControlChild(t, unmappedGVR, unmappedKind, "kr-notobserved", "Unknown", "NotObserved")
	h.createControlChild(t, nonKrateoGVR, nonKrateoKind, "fg-notobserved", "Unknown", "NotObserved")
	h.createControlChild(t, nonKrateoGVR, nonKrateoKind, "fg-healthy", "True", "Available")

	krStatus, krReason := h.rollup(t, controlRef(unmappedGVR, "kr-notobserved"))
	fgStatus, fgReason := h.rollup(t, controlRef(nonKrateoGVR, "fg-notobserved"))
	okStatus, okReason := h.rollup(t, controlRef(nonKrateoGVR, "fg-healthy"))

	if krStatus != "Unknown" || krReason != "ZeroEvidence" {
		t.Errorf("a krateo.io-group child at Ready=Unknown/NotObserved gave %s/%s, want Unknown/ZeroEvidence: "+
			"krateoReady is not being reached, and every arm above depends on it", krStatus, krReason)
	}
	if fgStatus != "False" {
		t.Errorf("a non-krateo.io custom child at the SAME Ready=Unknown/NotObserved gave %s/%s, want False "+
			"(converging): thirdPartyReady (#399) reads any non-True Ready as not ready", fgStatus, fgReason)
	}
	if krStatus == fgStatus {
		t.Errorf("both groups produced %s for the same condition: the group makes no difference, so this "+
			"suite's children are not being dispatched the way its arms assume", krStatus)
	}
	// #399 itself, through a real apiserver: a third party's Ready=True is now evidence.
	if okStatus != "True" {
		t.Errorf("a non-krateo.io custom child at Ready=True gave %s/%s, want True (#399)", okStatus, okReason)
	}
}

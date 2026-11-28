package compositiondefinitions

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/tools/restactionrbac"
	"github.com/krateo-platformops/plumbing/labels"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

var (
	planGVR = schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1-0-0", Resource: "fireworksapps"}
	planGVK = planGVR.GroupVersion().WithKind("FireworksApp")
)

func planComposition(ns, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(planGVK)
	u.SetNamespace(ns)
	u.SetName(name)
	u.SetUID(types.UID("uid-" + name))
	u.SetLabels(map[string]string{labels.CompositionVersionLabel: planGVR.Version})
	return u
}

func planDefinition() *compositiondefinitionsv1alpha1.CompositionDefinition {
	cd := &compositiondefinitionsv1alpha1.CompositionDefinition{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "d"}}
	cd.Spec.ApiRef = &compositiondefinitionsv1alpha1.ApiReference{Name: "ra", Namespace: "krateo-system",
		Extras: &apiextensionsv1.JSON{Raw: []byte(`{"tier":"gold"}`)}}
	return cd
}

func planDyn(objs ...runtime.Object) *fake.FakeDynamicClient {
	return fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		planGVR: "FireworksAppList", restActionsGVR: "RESTActionList"}, objs...)
}

func restAction(gen int64) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "templates.krateo.io/v1", "kind": "RESTAction",
		"metadata": map[string]any{"namespace": "krateo-system", "name": "ra", "generation": gen}}}
	return u
}

type planSnowplow struct {
	srv    *httptest.Server
	calls  atomic.Int64
	status atomic.Int32
	big    string // the composition whose request is answered 431
}

func newPlanSnowplow(t *testing.T) *planSnowplow {
	s := &planSnowplow{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.calls.Add(1)
		if st := s.status.Load(); st != 0 {
			w.WriteHeader(int(st))
			return
		}
		ex := map[string]any{}
		_ = json.Unmarshal([]byte(r.URL.Query().Get("extras")), &ex)
		ns, _ := ex["compositionNamespace"].(string)
		if n, _ := ex["compositionName"].(string); s.big != "" && n == s.big {
			w.WriteHeader(http.StatusRequestHeaderFieldsTooLarge)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"readSet": []restactionrbac.Resource{
			{Version: "v1", Resource: "pods", Namespace: ns, Verb: "list"},
			{Version: "v1", Resource: "secrets", Verb: "get", NamespaceUnknown: true}}})
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func withSnowplowURL(t *testing.T, url string) {
	old := SnowplowURL
	SnowplowURL = url
	t.Cleanup(func() { SnowplowURL = old })
}

func plannerFor691(sp *planSnowplow, dyn *fake.FakeDynamicClient) *apiRefPlanner {
	return newAPIRefPlanner(dyn, func() *restactionrbac.Client { return restactionrbac.New(sp.srv.URL) })
}

// N compositions with the same input are resolved once, however many passes ask: the second pass
// makes no call. A new composition adds exactly one. A RESTAction whose generation moved is asked
// again; one whose generation cannot be read is never cached.
func TestPlanner_CachesByGenerationAndExtras(t *testing.T) {
	sp := newPlanSnowplow(t)
	withSnowplowURL(t, sp.srv.URL)
	dyn := planDyn(restAction(1), planComposition("a", "c1"), planComposition("b", "c2"))
	p := plannerFor691(sp, dyn)
	cd := planDefinition()
	ctx := context.Background()

	plan, err := p.plan(ctx, cd, planGVR)
	if err != nil || len(plan.Compositions) != 2 || sp.calls.Load() != 2 {
		t.Fatalf("first pass: err=%v compositions=%d calls=%d, want 2 compositions and 2 calls", err, len(plan.Compositions), sp.calls.Load())
	}
	again, err := p.plan(ctx, cd, planGVR)
	if err != nil || sp.calls.Load() != 2 || again.digest != plan.digest {
		t.Fatalf("second pass: err=%v calls=%d (want still 2), digest equal=%v", err, sp.calls.Load(), again.digest == plan.digest)
	}
	if _, err := dyn.Resource(planGVR).Namespace("c").Create(ctx, planComposition("c", "c3"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.plan(ctx, cd, planGVR); err != nil || sp.calls.Load() != 3 {
		t.Fatalf("a third composition: err=%v calls=%d, want 3", err, sp.calls.Load())
	}
	// The RESTAction changes: every composition is asked again.
	if _, err := dyn.Resource(restActionsGVR).Namespace("krateo-system").Update(ctx, restAction(2), metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.plan(ctx, cd, planGVR); err != nil || sp.calls.Load() != 6 {
		t.Fatalf("a new RESTAction generation: err=%v calls=%d, want 6", err, sp.calls.Load())
	}
	// Its generation cannot be read: nothing is cached, nothing is assumed.
	dyn.PrependReactor("get", "restactions", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("forbidden")
	})
	_, _ = p.plan(ctx, cd, planGVR)
	_, _ = p.plan(ctx, cd, planGVR)
	if sp.calls.Load() != 12 {
		t.Errorf("with an unreadable generation calls=%d, want 12 (3 compositions asked on each of 2 passes)", sp.calls.Load())
	}
}

// The residual: the same read-set reaches the plan, and the rows still namespaceUnknown for a
// composition are named; the namespaces are the compositions'.
func TestPlan_NamesTheResidualAndTheNamespaces(t *testing.T) {
	sp := newPlanSnowplow(t)
	withSnowplowURL(t, sp.srv.URL)
	p := plannerFor691(sp, planDyn(restAction(1), planComposition("a", "c1"), planComposition("b", "c2")))
	plan, err := p.plan(context.Background(), planDefinition(), planGVR)
	if err != nil {
		t.Fatal(err)
	}
	st := apiRefRBACStatus(plan)
	if len(st.Namespaces) != 2 || st.Namespaces[0] != "a" || st.Namespaces[1] != "b" {
		t.Errorf("namespaces = %v", st.Namespaces)
	}
	if len(st.ClusterWide) != 1 || st.ClusterWide[0].Resource != "secrets" || st.ClusterWide[0].Verb != "get" {
		t.Errorf("clusterWide = %+v, want secrets/get once", st.ClusterWide)
	}
}

// What each failure of /rbac means for the Roles. 503 and a server error: not determined (Unknown,
// Roles untouched). 431: a refusal with its own reason. 422: exactly what it is today, a stage that
// cannot be enumerated (False/StageUnresolvable), neither an empty read-set nor a transient failure.
func TestPlan_FailuresAreNamedAndNeverAnEmptyPlan(t *testing.T) {
	for _, c := range []struct {
		status    int
		determine bool // a *notDetermined
		condition string
	}{
		{http.StatusServiceUnavailable, true, "Unknown/RolesNotDetermined"},
		{http.StatusInternalServerError, true, "Unknown/RolesNotDetermined"},
		{http.StatusUnprocessableEntity, false, "False/StageUnresolvable"},
	} {
		sp := newPlanSnowplow(t)
		sp.status.Store(int32(c.status))
		withSnowplowURL(t, sp.srv.URL)
		p := plannerFor691(sp, planDyn(restAction(1), planComposition("a", "c1")))
		plan, err := p.plan(context.Background(), planDefinition(), planGVR)
		if err == nil || plan != nil {
			t.Fatalf("%d: plan=%v err=%v, want an error and no plan", c.status, plan, err)
		}
		var nd *notDetermined
		if errors.As(err, &nd) != c.determine {
			t.Errorf("%d: notDetermined = %v, want %v (%v)", c.status, !c.determine, c.determine, err)
		}
		cd := planDefinition()
		recordApiRefRBAC(cd, err)
		got := cd.GetCondition(conditionTypeApiRefRBACComplete)
		if string(got.Status)+"/"+string(got.Reason) != c.condition {
			t.Errorf("%d: condition = %s/%s, want %s", c.status, got.Status, got.Reason, c.condition)
		}
	}
}

// A composition list that fails is not "no compositions": that would delete every Role.
func TestPlan_AFailedCompositionListIsNotDetermined(t *testing.T) {
	sp := newPlanSnowplow(t)
	withSnowplowURL(t, sp.srv.URL)
	dyn := planDyn(restAction(1))
	dyn.PrependReactor("list", "fireworksapps", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("etcd unavailable")
	})
	plan, err := plannerFor691(sp, dyn).plan(context.Background(), planDefinition(), planGVR)
	var nd *notDetermined
	if plan != nil || !errors.As(err, &nd) {
		t.Fatalf("plan=%v err=%v, want a notDetermined error", plan, err)
	}
}

// The condition when the plan was resolved: True/ReadSetResolved, or True/ClusterWideRows when rows
// stay cluster-wide, which are then named in status.
func TestRecordApiRefRBAC_ClusterWideRowsAreNamed(t *testing.T) {
	cd := planDefinition()
	recordApiRefRBAC(cd, nil)
	if c := cd.GetCondition(conditionTypeApiRefRBACComplete); c.Reason != "ReadSetResolved" {
		t.Errorf("no residual: reason = %s", c.Reason)
	}
	cd.Status.ApiRefRBAC = &compositiondefinitionsv1alpha1.ApiRefRBACStatus{
		ClusterWide: []compositiondefinitionsv1alpha1.ApiRefClusterWideRow{{Resource: "secrets", Verb: "get"}}}
	recordApiRefRBAC(cd, nil)
	if c := cd.GetCondition(conditionTypeApiRefRBACComplete); c.Reason != "ClusterWideRows" || c.Status != metav1.ConditionTrue {
		t.Errorf("residual: condition = %s/%s", c.Status, c.Reason)
	}
	cd.Spec.ApiRef = nil
	recordApiRefRBAC(cd, nil)
	if cd.Status.ApiRefRBAC != nil || cd.GetCondition(conditionTypeApiRefRBACComplete).Reason != "" {
		t.Errorf("without an apiRef both go: %+v", cd.Status.ApiRefRBAC)
	}
}

// A composition whose request snowplow refuses (431) beside a normal one: the plan is made, the normal
// one's reads are in it, and the refused one is named (not an error, not an empty set).
func TestPlan_AComposition431IsNamedAndDoesNotFailThePlan(t *testing.T) {
	sp := newPlanSnowplow(t)
	sp.big = "huge"
	withSnowplowURL(t, sp.srv.URL)
	p := plannerFor691(sp, planDyn(restAction(1), planComposition("a", "ok"), planComposition("b", "huge")))
	plan, err := p.plan(context.Background(), planDefinition(), planGVR)
	if err != nil {
		t.Fatalf("one refused composition failed the plan: %v", err)
	}
	if len(plan.Unresolved) != 1 || plan.Unresolved[0].Namespace != "b" || plan.Unresolved[0].Name != "huge" || plan.Unresolved[0].Reason != "ExtrasTooLarge" {
		t.Fatalf("unresolved = %+v", plan.Unresolved)
	}
	if st := apiRefRBACStatus(plan); len(st.Namespaces) != 1 || st.Namespaces[0] != "a" {
		t.Errorf("the normal composition's namespace must be planned, got %v", st.Namespaces)
	}
	cd := planDefinition()
	cd.Status.ApiRefRBAC = apiRefRBACStatus(plan)
	recordApiRefRBAC(cd, nil)
	if c := cd.GetCondition(conditionTypeApiRefRBACComplete); c.Reason != "ExtrasTooLarge" || c.Status != metav1.ConditionFalse {
		t.Errorf("condition = %s/%s", c.Status, c.Reason)
	}
}

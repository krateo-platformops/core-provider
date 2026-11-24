package composition

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/krateo-platformops/composition-dynamic-controller/internal/conditionreason"
	"github.com/krateo-platformops/unstructured-runtime/pkg/controller"
	"github.com/krateo-platformops/unstructured-runtime/pkg/pluralizer"
	"github.com/krateo-platformops/unstructured-runtime/pkg/tools"
	unstructuredtools "github.com/krateo-platformops/unstructured-runtime/pkg/tools/unstructured"
	"github.com/krateo-platformops/unstructured-runtime/pkg/tools/unstructured/condition"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

// cond646 is a condition without its transition time: what a consumer decides on.
func cond646(c metav1.Condition) string {
	return fmt.Sprintf("%s=%s/%s %q", c.Type, c.Status, c.Reason, c.Message)
}

func conds646(conds []metav1.Condition) map[string]string {
	out := map[string]string{}
	for _, c := range conds {
		out[c.Type] = cond646(c)
	}
	return out
}

func answer646(t *testing.T, code int, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

type noGVR646 struct{ pluralizer.PluralizerInterface }

func (noGVR646) GVKtoGVR(schema.GroupVersionKind) (schema.GroupVersionResource, error) {
	return schema.GroupVersionResource{}, errors.New("no such kind")
}

// Every way applyDerivedRBAC fails, through the real applyDerivedRBAC. The oracle is Observe: what Create
// returns is what failObserveOnDerivedRBAC PERSISTS for the same failure on the same object, type by type,
// with the same error. The rows also state the outcome, so the two cannot be wrong together.
func TestCreateFailedOnDerivedRBAC_CarriesWhatObserveRecords(t *testing.T) {
	at := func(c metav1.Condition, age time.Duration) metav1.Condition {
		c.LastTransitionTime = metav1.NewTime(time.Now().Add(-age).Truncate(time.Second))
		return c
	}
	creating := conditionreason.Creating.WithMessage("an earlier failure")
	inspectorDown := conditionreason.ChartInspectorUnavailable.WithMessage("could not reach chart-inspector")
	for _, tc := range []struct {
		name     string
		url      string
		plural   pluralizer.PluralizerInterface
		stored   []metav1.Condition
		want     map[string]string // type -> status/reason
		readyHas string
		errHas   string
	}{
		{name: "the chart refused the values", url: answer646(t, 400, templateRefusedBody(t, "audience is required")),
			want: map[string]string{"ChartRenderable": "False/TemplateRefused", "RBACComplete": "Unknown/NotEvaluated", "Ready": "False/Unavailable"}, readyHas: "audience is required"},
		{name: "refused, over a stored InstallFailing: as Observe", url: answer646(t, 400, templateRefusedBody(t, "audience is required")),
			stored: []metav1.Condition{conditionreason.InstallFailing.WithMessage("the first install has failed 3 times")},
			want:   map[string]string{"ChartRenderable": "False/TemplateRefused", "RBACComplete": "Unknown/NotEvaluated", "Ready": "False/Unavailable"}, readyHas: "audience is required"},
		{name: "the values break the schema", url: answer646(t, 400, schemaViolationBody(t, "replicas must be an integer")),
			want: map[string]string{"ChartRenderable": "False/ValuesSchemaViolation", "RBACComplete": "Unknown/NotEvaluated", "Ready": "False/Unavailable"}, readyHas: "replicas must be an integer"},
		{name: "an unserved kind", url: answer646(t, 422, `{"message":"no matches for kind","reason":"Invalid","code":422}`),
			want: map[string]string{"ChartRenderable": "False/UnservedKind", "RBACComplete": "Unknown/NotEvaluated", "Ready": "False/Unavailable"}, readyHas: "no matches for kind"},
		{name: "chart-inspector failed", url: answer646(t, 500, `{"message":"boom"}`),
			want: map[string]string{"ChartRenderable": "False/InspectorUnavailable", "RBACComplete": "Unknown/NotEvaluated", "Ready": "False/Unavailable"}, readyHas: "boom"},
		{name: "the RBAC apply was refused: it rendered", url: answer646(t, 200, `[{"group":"","version":"v1","resource":"configmaps","verbs":["*"]}]`),
			want: map[string]string{"ChartRenderable": "True/Renderable", "RBACComplete": "False/ApplyFailed", "Ready": "False/Unavailable"}, readyHas: "applying rbac"},
		{name: "before the render: nothing computed but Ready", url: "http://127.0.0.1:9", plural: noGVR646{},
			want: map[string]string{"Ready": "False/Unavailable"}, readyHas: "converting GVK to GVR"},
		{name: "no answer, first create: no Ready to keep", url: refusedURL(),
			want: map[string]string{"ChartRenderable": "False/InspectorUnavailable", "RBACComplete": "Unknown/NotEvaluated", "Ready": "False/Unavailable"}, readyHas: "connection refused"},
		{name: "no answer, inside the grace: the last Ready is kept", url: refusedURL(), stored: []metav1.Condition{creating},
			want: map[string]string{"ChartRenderable": "False/InspectorUnavailable", "Ready": "False/Creating"}, readyHas: "an earlier failure", errHas: "Ready is kept as last assessed until"},
		{name: "no answer, the grace over", url: refusedURL(), stored: []metav1.Condition{creating, at(inspectorDown, 6*time.Minute)},
			want: map[string]string{"ChartRenderable": "False/InspectorUnavailable", "RBACComplete": "Unknown/NotEvaluated", "Ready": "False/Unavailable"}, readyHas: "connection refused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, now := context.Background(), time.Now()
			fail := func() (*unstructured.Unstructured, error) {
				mg := ndPortal("")
				if err := unstructuredtools.SetConditions(mg, tc.stored...); err != nil {
					t.Fatal(err)
				}
				var plural pluralizer.PluralizerInterface = stubPluralizerForRBAC{}
				if tc.plural != nil {
					plural = tc.plural
				}
				h := &handler{pluralizer: plural, chartInspectorUrl: tc.url, saName: "cdc", saNamespace: "krateo-system"}
				refusing := fakeDynWith() // any RBAC write is refused
				refusing.PrependReactor("*", "*", func(a clienttesting.Action) (bool, runtime.Object, error) {
					return a.GetVerb() != "get", nil, errors.New("forbidden")
				})
				_, err := h.applyDerivedRBAC(ctx, mg, pkgFor577, refusing, "portal-abcde")
				if err == nil {
					t.Fatal("applyDerivedRBAC succeeded")
				}
				return mg, err
			}
			mg, cause := fail()
			got, err := createFailedOnDerivedRBAC(ctx, mg, cause, now)

			observed, ocause := fail()
			dyn := frFakeDyn(ndPortal(""))
			oerr := failObserveOnDerivedRBAC(ctx, observed, ocause, now, srOpts(dyn))
			if !controller.IsReadyAssessed(oerr) || err.Error() != oerr.Error() {
				t.Errorf("err\n create  %v\n observe %v", err, oerr)
			}
			have, want := conds646(got), conds646(conditionsOn(storedPortal(t, dyn), "ChartRenderable", "RBACComplete", "Ready"))
			if len(have) != len(got) || fmt.Sprint(have) != fmt.Sprint(want) {
				t.Errorf("conditions\n create  %v\n observe %v", have, want)
			}
			if len(got) != len(tc.want) {
				t.Errorf("%d conditions, want %d: %v", len(got), len(tc.want), have)
			}
			for _, c := range got {
				if string(c.Status)+"/"+c.Reason != tc.want[c.Type] {
					t.Errorf("%s, want %s", cond646(c), tc.want[c.Type])
				}
				if c.Type == condition.TypeReady && !strings.Contains(c.Message, tc.readyHas) {
					t.Errorf("Ready message %q lacks %q", c.Message, tc.readyHas)
				}
			}
			if !strings.Contains(err.Error(), tc.errHas) {
				t.Errorf("err %q lacks %q", err, tc.errHas)
			}
			if n := installFailureCount(mg); n != 0 {
				t.Errorf("counted as %d failed install attempts", n)
			}
		})
	}
}

// The one difference from Observe: Create runs because the release is ABSENT, so a stored Ready=True is not
// kept while chart-inspector gives no answer. CONTROL: the same object on Observe's path is kept.
func TestCreateFailedOnDerivedRBAC_AStoredTrueIsNotKept(t *testing.T) {
	mg := fireworkFor577(t, condition.Available())
	cause := rbacFailureFrom(t, mg, refusedURL())
	if keep, _ := keepReadyWhileInspectorUnreachable(mg, cause, time.Now()); !keep {
		t.Fatal("control: Observe would not keep it, so this tests nothing")
	}
	conds, err := createFailedOnDerivedRBAC(context.Background(), mg, cause, time.Now())
	if got := conds646(conds)["Ready"]; !strings.HasPrefix(got, "Ready=False/Unavailable") || err.Error() != cause.Error() {
		t.Errorf("Ready %s, err %v", got, err)
	}
	if got := conds646(conds)["RBACComplete"]; !strings.HasPrefix(got, "RBACComplete=Unknown/NotEvaluated") {
		t.Errorf("RBACComplete %s", got)
	}
}

// When a failed Create's conditions are not carried: each a state in which writing them would be wrong.
func TestCarryOnFailedCreate(t *testing.T) {
	conds := []metav1.Condition{conditionreason.ChartTemplateRefused.WithMessage("m"), conditionreason.Unavailable.WithMessage("m")}
	boom := errors.New("boom")
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	deleting := ndPortal("")
	now := metav1.Now()
	deleting.SetDeletionTimestamp(&now)
	deleting.SetFinalizers([]string{"x"})
	unreadable := frFakeDyn(ndPortal(""))
	unreadable.PrependReactor("get", "portals", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("the apiserver is unavailable")
	})
	for _, tc := range []struct {
		name    string
		ctx     context.Context
		err     error
		conds   []metav1.Condition
		dyn     *dynamicfake.FakeDynamicClient
		carried bool
	}{
		{"control: a failure", context.Background(), boom, conds, frFakeDyn(ndPortal("")), true},
		{"no error", context.Background(), nil, conds, frFakeDyn(ndPortal("")), false},
		{"nothing computed", context.Background(), boom, nil, frFakeDyn(ndPortal("")), false},
		{"a lost status race", context.Background(), fmt.Errorf("x: %w", tools.ErrStatusConflict), conds, frFakeDyn(ndPortal("")), false},
		{"a cancelled pass", cancelled, boom, conds, frFakeDyn(ndPortal("")), false},
		{"being deleted", context.Background(), boom, conds, frFakeDyn(deleting), false},
		{"gone", context.Background(), boom, conds, frFakeDyn(), false},
		{"deletion could not be ruled out", context.Background(), boom, conds, unreadable, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The object Create holds was read before any deletion: only the re-read can say.
			err := carryOnFailedCreate(tc.ctx, ndPortal(""), tc.err, tc.conds, srOpts(tc.dyn))
			if tc.err == nil {
				if err != nil {
					t.Fatalf("err %v", err)
				}
				return
			}
			if !errors.Is(err, tc.err) || err.Error() != tc.err.Error() {
				t.Errorf("err %v, want %v unchanged", err, tc.err)
			}
			// As Create returns it when helm was not called.
			got := failedCreateConditions(controller.CreateNotCommitted(err))
			if carried := len(got) > 0; carried != tc.carried || (carried && fmt.Sprint(conds646(got)) != fmt.Sprint(conds646(conds))) {
				t.Errorf("carried %v, want %v", conds646(got), tc.carried)
			}
		})
	}
}

// A failed install attempt keeps the Ready InstallFailing gives it: Creating is the runtime's, so nothing is
// returned for it; from the third it is InstallFailing. What the pass computed before is carried beside it.
func TestFailedCreateConditions_InstallFailingKeepsReady(t *testing.T) {
	computed := []metav1.Condition{conditionreason.ChartRenderable.WithMessage("ok"), conditionreason.RBACApplied.WithMessage("ok")}
	attempt := func(ready metav1.Condition) error {
		return &createFailed{err: fmt.Errorf("installing helm chart: %w", &installFailed{err: errors.New("hook failed"), ready: ready, recorded: true}), conds: computed}
	}
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"no error", nil, ""},
		{"neither", errors.New("x"), ""},
		{"the first two attempts", attempt(conditionreason.Creating.WithMessage("hook failed")), "ChartRenderable=True/Renderable RBACComplete=True/Applied"},
		{"from the third", attempt(conditionreason.InstallFailing.WithMessage("hook failed")), "ChartRenderable=True/Renderable RBACComplete=True/Applied Ready=False/InstallFailing"},
		{"from the third, nothing computed", &installFailed{err: errors.New("x"), ready: conditionreason.InstallFailing.WithMessage("m")}, "Ready=False/InstallFailing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, c := range failedCreateConditions(tc.err) {
				got = append(got, fmt.Sprintf("%s=%s/%s", c.Type, c.Status, c.Reason))
			}
			if s := strings.Join(got, " "); s != tc.want {
				t.Errorf("%q, want %q", s, tc.want)
			}
		})
	}
}

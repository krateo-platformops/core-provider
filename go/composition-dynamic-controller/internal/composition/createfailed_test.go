package composition

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
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
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

// A Forbidden from anywhere but the RBAC apply is not a refusal of the RBAC.
func (noGVR646) GVKtoGVR(schema.GroupVersionKind) (schema.GroupVersionResource, error) {
	return schema.GroupVersionResource{}, forbidden646
}

var (
	crGR646      = schema.GroupResource{Group: "rbac.authorization.k8s.io", Resource: "clusterroles"}
	forbidden646 = apierrors.NewForbidden(crGR646, "x", errors.New("attempting to grant RBAC permissions not currently held"))
	invalid646   = apierrors.NewInvalid(schema.GroupKind{Group: "rbac.authorization.k8s.io", Kind: "ClusterRole"}, "x", nil)
	conflict646  = apierrors.NewConflict(crGR646, "x", errors.New("modified"))
	notFound646  = apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, "gone")
	teapot646    = &apierrors.StatusError{ErrStatus: metav1.Status{Status: "Failure", Code: 403, Reason: "SomethingNew", Message: "a reason nobody classified"}}
	renders646   = `[{"group":"","version":"v1","resource":"configmaps","verbs":["*"]}]`
	// chart-inspector's own failure carrying a template's words: what is read is its marker, not the text.
	unmarked646 = `{"message":"execution error at (x): audience is required","reason":"BadRequest","code":400}`
)

func terminating646() error {
	err := apierrors.NewForbidden(schema.GroupResource{Resource: "roles"}, "x", errors.New("being terminated"))
	err.ErrStatus.Details.Causes = append(err.ErrStatus.Details.Causes, metav1.StatusCause{Type: corev1.NamespaceTerminatingCause})
	return err
}

// Every way applyDerivedRBAC fails, through the real applyDerivedRBAC, against what Observe persists for the
// same failure on the same object. ChartRenderable, RBACComplete and the error are always Observe's. Ready is
// Observe's exactly where the failure is a verdict, or kept inside the grace; otherwise Observe says
// Unavailable and Create returns none (the runtime's Creating stands), or the InstallFailing it keeps. Both
// sides are asserted, and the rows state the outcome, so the two cannot be wrong together.
func TestCreateFailedOnDerivedRBAC_AgainstObserve(t *testing.T) {
	at := func(c metav1.Condition, age time.Duration) metav1.Condition {
		c.LastTransitionTime = metav1.NewTime(time.Now().Add(-age).Truncate(time.Second))
		return c
	}
	creating := conditionreason.Creating.WithMessage("an earlier failure")
	unavailable := conditionreason.Unavailable.WithMessage("a refusal, since fixed")
	failing := conditionreason.InstallFailing.WithMessage("the first install has failed 3 times")
	inspectorDown := conditionreason.ChartInspectorUnavailable.WithMessage("could not reach chart-inspector")
	renders := answer646(t, 200, renders646)
	const notEvaluated, down = "Unknown/NotEvaluated", "False/InspectorUnavailable"
	for _, tc := range []struct {
		name      string
		url       string
		apply     error // what every RBAC write gets
		plural    pluralizer.PluralizerInterface
		stored    []metav1.Condition
		counted   int64
		chart     string
		rbac      string
		ready     string // "": none is returned
		asObserve bool   // Ready is what Observe records
		readyHas  string
		errHas    string
	}{
		// VERDICTS.
		{name: "the chart refused the values", url: answer646(t, 400, templateRefusedBody(t, "audience is required")),
			chart: "False/TemplateRefused", rbac: notEvaluated, ready: "False/Unavailable", asObserve: true, readyHas: "audience is required"},
		{name: "the values break the schema", url: answer646(t, 400, schemaViolationBody(t, "replicas must be an integer")),
			chart: "False/ValuesSchemaViolation", rbac: notEvaluated, ready: "False/Unavailable", asObserve: true, readyHas: "replicas must be an integer"},
		{name: "a verdict over InstallFailing: as Observe", url: answer646(t, 400, templateRefusedBody(t, "audience is required")), stored: []metav1.Condition{failing}, counted: 3,
			chart: "False/TemplateRefused", rbac: notEvaluated, ready: "False/Unavailable", asObserve: true, readyHas: "audience is required"},
		{name: "RBAC forbidden, as an escalation is", url: renders, apply: forbidden646,
			chart: "True/Renderable", rbac: "False/ApplyFailed", ready: "False/Unavailable", asObserve: true, readyHas: "not currently held"},
		{name: "RBAC invalid", url: renders, apply: invalid646,
			chart: "True/Renderable", rbac: "False/ApplyFailed", ready: "False/Unavailable", asObserve: true, readyHas: "is invalid"},
		// NOT VERDICTS.
		{name: "an unserved kind", url: answer646(t, 422, `{"message":"no matches for kind","reason":"Invalid","code":422}`),
			chart: "False/UnservedKind", rbac: notEvaluated},
		{name: "an unmarked 400", url: answer646(t, 400, unmarked646), chart: "False/InspectorRejected", rbac: notEvaluated},
		{name: "another 4xx", url: answer646(t, 404, "404 page not found"), chart: "False/InspectorRejected", rbac: notEvaluated},
		{name: "chart-inspector failed", url: answer646(t, 500, `{"message":"boom"}`), chart: down, rbac: notEvaluated},
		{name: "an answer outside the protocol", url: answer646(t, 302, ""), chart: down, rbac: notEvaluated},
		{name: "an answer that cannot be read", url: answer646(t, 200, "<html>"), chart: down, rbac: notEvaluated},
		{name: "RBAC: a conflict", url: renders, apply: conflict646, chart: "True/Renderable", rbac: "False/ApplyFailed"},
		{name: "RBAC: a timeout", url: renders, apply: apierrors.NewTimeoutError("slow", 1), chart: "True/Renderable", rbac: "False/ApplyFailed"},
		{name: "RBAC: a deadline", url: renders, apply: context.DeadlineExceeded, chart: "True/Renderable", rbac: "False/ApplyFailed"},
		{name: "RBAC: a 5xx", url: renders, apply: apierrors.NewInternalError(errors.New("etcd")), chart: "True/Renderable", rbac: "False/ApplyFailed"},
		{name: "RBAC: the network", url: renders, apply: errors.New("connection reset by peer"), chart: "True/Renderable", rbac: "False/ApplyFailed"},
		{name: "RBAC: no such namespace", url: renders, apply: notFound646, chart: "True/Renderable", rbac: "False/ApplyFailed"},
		{name: "RBAC: a reason nobody classified, at 403", url: renders, apply: teapot646, chart: "True/Renderable", rbac: "False/ApplyFailed"},
		{name: "before the render, a Forbidden resolving the kind", url: "http://127.0.0.1:9", plural: noGVR646{}},
		// KEPT: the grace.
		{name: "no answer, first create: no Ready to keep", url: refusedURL(), chart: down, rbac: notEvaluated},
		{name: "no answer, inside the grace: the last Ready is kept", url: refusedURL(), stored: []metav1.Condition{creating},
			chart: down, ready: "False/Creating", asObserve: true, readyHas: "an earlier failure", errHas: "Ready is kept as last assessed until"},
		{name: "no answer, inside the grace: Unavailable is kept", url: refusedURL(), stored: []metav1.Condition{unavailable}, counted: 3,
			chart: down, ready: "False/Unavailable", asObserve: true, readyHas: "since fixed", errHas: "Ready is kept as last assessed until"},
		{name: "no answer, the grace over", url: refusedURL(), stored: []metav1.Condition{unavailable, at(inspectorDown, 6*time.Minute)}, chart: down, rbac: notEvaluated},
		// KEPT: InstallFailing.
		{name: "an unserved kind over InstallFailing", url: answer646(t, 422, `{"message":"no matches for kind"}`), stored: []metav1.Condition{failing}, counted: 3,
			chart: "False/UnservedKind", rbac: notEvaluated, ready: "False/InstallFailing", readyHas: "the first install has failed 3 times (0 automatic retries so far); last error: hook failed; the next retry did not start: generating RBAC"},
		{name: "a conflict over InstallFailing", url: renders, apply: conflict646, stored: []metav1.Condition{failing}, counted: 3,
			chart: "True/Renderable", rbac: "False/ApplyFailed", ready: "False/InstallFailing", readyHas: "the next retry did not start: applying rbac"},
		{name: "no answer, the grace over, over InstallFailing", url: refusedURL(), stored: []metav1.Condition{failing, at(inspectorDown, 6*time.Minute)}, counted: 3,
			chart: down, rbac: notEvaluated, ready: "False/InstallFailing", readyHas: "the next retry did not start"},
		{name: "counted as failing, Ready since a refusal that was fixed", url: answer646(t, 500, "boom"), stored: []metav1.Condition{unavailable}, counted: 4,
			chart: down, rbac: notEvaluated, ready: "False/InstallFailing", readyHas: "failed 4 times"},
		{name: "InstallFailing stored, its count not", url: answer646(t, 500, "boom"), stored: []metav1.Condition{failing}, counted: 2,
			chart: down, rbac: notEvaluated, ready: "False/InstallFailing", readyHas: failing.Message},
		{name: "control: two failures are Creating", url: answer646(t, 500, "boom"), stored: []metav1.Condition{creating}, counted: 2, chart: down, rbac: notEvaluated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, now := context.Background(), time.Now()
			fail := func() (*unstructured.Unstructured, error) {
				mg := ndPortal("")
				if err := unstructuredtools.SetConditions(mg, tc.stored...); err != nil {
					t.Fatal(err)
				}
				if tc.counted > 0 {
					_ = unstructured.SetNestedMap(mg.Object, map[string]any{"count": tc.counted, "lastAt": "x", "lastError": "hook failed"}, "status", "installFailures")
				}
				var plural pluralizer.PluralizerInterface = stubPluralizerForRBAC{}
				if tc.plural != nil {
					plural = tc.plural
				}
				h := &handler{pluralizer: plural, chartInspectorUrl: tc.url, saName: "cdc", saNamespace: "krateo-system"}
				refusing := fakeDynWith()
				refusing.PrependReactor("*", "*", func(a clienttesting.Action) (bool, runtime.Object, error) {
					return tc.apply != nil && a.GetVerb() != "get", nil, tc.apply
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
			// applyDerivedRBAC stamps each condition it writes with the real clock (the condition
			// package's own time.Now, no `now` is passed in), and "Ready is kept ... until" is that
			// stamp + the grace. Two fail() calls can straddle a second boundary, so give the Observe
			// composition the instants of the Create one: both passes then see ONE instant.
			sameInstant(t, mg, observed)
			dyn := frFakeDyn(ndPortal(""))
			oerr := failObserveOnDerivedRBAC(ctx, observed, ocause, now, srOpts(dyn))
			if !controller.IsReadyAssessed(oerr) || err.Error() != oerr.Error() {
				t.Errorf("err\n create  %v\n observe %v", err, oerr)
			}
			if !strings.Contains(err.Error(), tc.errHas) {
				t.Errorf("err %q lacks %q", err, tc.errHas)
			}
			have, obs := conds646(got), conds646(unstructuredtools.GetConditions(storedPortal(t, dyn)))
			if len(have) != len(got) {
				t.Errorf("a type returned twice: %v", got)
			}
			for _, typ := range []string{"ChartRenderable", "RBACComplete"} {
				if have[typ] != obs[typ] {
					t.Errorf("%s\n create  %s\n observe %s", typ, have[typ], obs[typ])
				}
			}
			for typ, want := range map[string]string{"ChartRenderable": tc.chart, "RBACComplete": tc.rbac, "Ready": tc.ready} {
				if have[typ] != "" && want == "" || want != "" && !strings.HasPrefix(have[typ], typ+"="+want+" ") {
					t.Errorf("%s: returned %q, want %q", typ, have[typ], want)
				}
			}
			if !strings.Contains(have["Ready"], tc.readyHas) {
				t.Errorf("Ready %q lacks %q", have["Ready"], tc.readyHas)
			}
			switch {
			case tc.asObserve && have["Ready"] != obs["Ready"]:
				t.Errorf("Ready\n create  %s\n observe %s", have["Ready"], obs["Ready"])
			case !tc.asObserve && !strings.HasPrefix(obs["Ready"], "Ready=False/Unavailable "):
				t.Errorf("Observe is said to differ, and records %q", obs["Ready"])
			}
			if n := installFailureCount(mg); n != tc.counted {
				t.Errorf("%d failed install attempts counted, had %d", n, tc.counted)
			}
		})
	}
}

// One refused object makes the apply refused, whatever failed beside it, in any order and at any depth. ApplyRBAC
// returns its first failure today, so this is what holds if it ever returns them all.
func TestRBACRefused_Mixed(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"forbidden, then a conflict", errors.Join(forbidden646, conflict646), true},
		{"a conflict, then forbidden", errors.Join(conflict646, forbidden646), true},
		{"wrapped, a conflict, then invalid", fmt.Errorf("x: %w", errors.Join(conflict646, fmt.Errorf("y: %w", invalid646))), true},
		{"a conflict and a missing namespace", errors.Join(conflict646, notFound646), false},
		{"a namespace being deleted", terminating646(), false},
		{"a namespace being deleted, and forbidden", errors.Join(terminating646(), forbidden646), true},
		{"a 403 with a reason nobody classified", teapot646, false},
	} {
		if got := rbacRefused(tc.err); got != tc.want {
			t.Errorf("%s: refused %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Every ChartRenderable reason is classified HERE, by name: one added to conditionreason and not to this table
// fails, whatever the code does with it. Read from the source and from the rendered table, so a reason is not
// missed by being declared and not listed.
func TestChartVerdict_EveryReasonIsClassified(t *testing.T) {
	classified := map[string]bool{
		"Renderable":            false, // not a failure
		"TemplateRefused":       true,  // the chart's own templates refused these values
		"ValuesSchemaViolation": true,  // the values do not satisfy the chart's schema
		"UnservedKind":          false, // resolves itself once the CRD is served
		"InspectorRejected":     false, // the cdc and chart-inspector disagree; says nothing of the chart
		"InspectorUnavailable":  false, // chart-inspector failed, or gave no answer
	}
	seen := map[string]bool{}
	for _, r := range conditionreason.Table() {
		if r.Type() != conditionreason.TypeChartRenderable {
			continue
		}
		seen[r.String()] = true
		if want, ok := classified[r.String()]; !ok {
			t.Errorf("ChartRenderable reason %s is not classified: is it a verdict on the chart or the values?", r)
		} else if got := chartVerdict(r); got != want {
			t.Errorf("%s: verdict %v, want %v", r, got, want)
		}
	}
	file, err := parser.ParseFile(token.NewFileSet(), "../conditionreason/conditionreason.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	declared := 0
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		if f, ok := call.Fun.(*ast.Ident); !ok || f.Name != "on" {
			return true
		}
		if typ, ok := call.Args[0].(*ast.Ident); ok && typ.Name == "TypeChartRenderable" {
			declared++
		}
		return true
	})
	if declared != len(classified) || len(seen) != len(classified) {
		t.Errorf("%d reasons classified, %d declared, %d in the table (%v)", len(classified), declared, len(seen), seen)
	}
	if chartVerdict(conditionreason.Reason{}) {
		t.Error("an unknown reason is a verdict")
	}
}

// Create runs because the release is ABSENT, so a stored Ready=True is not
// kept while chart-inspector gives no answer. CONTROL: the same object on Observe's path is kept.
func TestCreateFailedOnDerivedRBAC_AStoredTrueIsNotKept(t *testing.T) {
	mg := fireworkFor577(t, condition.Available())
	cause := rbacFailureFrom(t, mg, refusedURL())
	if keep, _ := keepReadyWhileInspectorUnreachable(mg, cause, time.Now()); !keep {
		t.Fatal("control: Observe would not keep it, so this tests nothing")
	}
	conds, err := createFailedOnDerivedRBAC(context.Background(), mg, cause, time.Now())
	if got := conds646(conds)["Ready"]; got != "" || err.Error() != cause.Error() {
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

// sameInstant copies the LastTransitionTime of every condition on from onto the condition of the same
// type on to, so two compositions built by two calls of one function carry one set of timestamps.
func sameInstant(t *testing.T, from, to *unstructured.Unstructured) {
	t.Helper()
	at := map[string]metav1.Time{}
	for _, c := range unstructuredtools.GetConditions(from) {
		at[c.Type] = c.LastTransitionTime
	}
	conds := unstructuredtools.GetConditions(to)
	for i := range conds {
		if ts, ok := at[conds[i].Type]; ok {
			conds[i].LastTransitionTime = ts
		}
	}
	if err := unstructuredtools.SetConditions(to, conds...); err != nil {
		t.Fatal(err)
	}
}

package composition

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/krateo-platformops/composition-dynamic-controller/internal/conditionreason"
	"github.com/krateo-platformops/composition-dynamic-controller/internal/snowplow"
	"github.com/krateo-platformops/plumbing/kubeutil/event"
	"github.com/krateo-platformops/unstructured-runtime/pkg/tools/statusprojection"
	unstructuredtools "github.com/krateo-platformops/unstructured-runtime/pkg/tools/unstructured"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/events"
)

func apiResolvedOf(t *testing.T, mg *unstructured.Unstructured) metav1.Condition {
	t.Helper()
	for _, c := range unstructuredtools.GetConditions(mg) {
		if c.Type == ConditionTypeApiResolved {
			return c
		}
	}
	t.Fatalf("no %s condition", ConditionTypeApiResolved)
	return metav1.Condition{}
}

// The Access card's case (Diego, 2026-11-16): the `urlStage` stage resolved, the `bundle` stage
// did not. The url mapping is a plain path; the bundle mapping is guarded with `// empty`, so it
// keeps the value an earlier pass left.
func partialFixture(outcomes snowplow.StageOutcomes) (*handler, *unstructured.Unstructured, *events.FakeRecorder) {
	rec := events.NewFakeRecorder(10)
	h := &handler{
		apiResolver: &fakeResolver{
			api:      map[string]any{"urlStage": map[string]any{"url": "https://fresh.example"}, "error": map[string]any{"code": float64(404)}},
			outcomes: outcomes,
		},
		eventRecorder: *event.NewAPIRecorder(rec),
		statusDataTemplate: []statusprojection.Mapping{
			{ForPath: "url", Expression: `${ .api.urlStage.url }`},
			{ForPath: "caBundleSynced", Expression: `${ .api.bundle.synced // empty }`},
		},
	}
	mg := fireworksMg(1)
	mg.Object["status"] = map[string]any{"url": "https://old.example", "caBundleSynced": true}
	return h, mg, rec
}

func available(t *testing.T, h *handler, mg *unstructured.Unstructured) {
	t.Helper()
	if err := h.setStatus(context.Background(), mg, &statusManagerOpts{chartURL: "u", chartVersion: "v", conditionType: ConditionTypeAvailable}); err != nil {
		t.Fatalf("setStatus: %v", err)
	}
}

// ApiResolved is True ONLY when snowplow confirmed every stage (`[]`). Every other verdict still
// projects what resolved.
func TestApiResolvedFollowsTheStageOutcomes(t *testing.T) {
	failed := snowplow.StageOutcomes{Present: true, Failed: []snowplow.StageOutcome{{Name: "bundle", Reason: "NotFound"}}}
	cases := []struct {
		name     string
		outcomes snowplow.StageOutcomes
		status   metav1.ConditionStatus
		reason   string
		inMsg    []string
		event    bool
	}{
		{"[]: True", snowplow.StageOutcomes{Present: true}, metav1.ConditionTrue, conditionreason.ReasonApiResolved, []string{"every RESTAction stage succeeded"}, false},
		{"a failed stage: False/ApiPartiallyResolved naming it", failed, metav1.ConditionFalse, conditionreason.ReasonApiPartiallyResolved,
			[]string{"1 RESTAction stage did not succeed", "bundle: NotFound", "status.unrefreshedFields"}, true},
		{"truncated: False/ApiPartiallyResolved with the count", snowplow.StageOutcomes{Present: true, Truncated: true, TruncatedFailed: 57},
			metav1.ConditionFalse, conditionreason.ReasonApiPartiallyResolved, []string{"57 RESTAction stages did not succeed", "truncated"}, true},
		{"no header: Unknown/StageOutcomesUnknown", snowplow.StageOutcomes{}, metav1.ConditionUnknown, conditionreason.ReasonStageOutcomesUnknown,
			[]string{"sent no X-Snowplow-Stage-Outcomes header"}, false},
		{"an unreadable header: Unknown, never success", snowplow.StageOutcomes{Present: true, Unreadable: "the header is not a JSON array"},
			metav1.ConditionUnknown, conditionreason.ReasonStageOutcomesUnknown, []string{"could not be read (the header is not a JSON array)"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, mg, rec := partialFixture(c.outcomes)
			available(t, h, mg)
			cond := apiResolvedOf(t, mg)
			if cond.Status != c.status || cond.Reason != c.reason {
				t.Fatalf("ApiResolved = %s/%s, want %s/%s (%s)", cond.Status, cond.Reason, c.status, c.reason, cond.Message)
			}
			for _, s := range c.inMsg {
				if !strings.Contains(cond.Message, s) {
					t.Errorf("message %q lacks %q", cond.Message, s)
				}
			}
			// What resolved is projected whatever the verdict.
			if v, _, _ := unstructured.NestedString(mg.Object, "status", "url"); v != "https://fresh.example" {
				t.Errorf("status.url = %q: the stage that resolved was not projected", v)
			}
			select {
			case ev := <-rec.Events:
				if !c.event {
					t.Errorf("unexpected event %q", ev)
				} else if !strings.Contains(ev, "Warning") || !strings.Contains(ev, "ApiRefPartiallyResolved") {
					t.Errorf("event = %q, want Warning/ApiRefPartiallyResolved", ev)
				}
			default:
				if c.event {
					t.Error("no event for a partial resolve")
				}
			}
			// Ready is not touched by any of them.
			for _, rc := range unstructuredtools.GetConditions(mg) {
				if rc.Type == "Ready" && rc.Status != metav1.ConditionTrue {
					t.Errorf("Ready = %s/%s: ApiResolved must not regress readiness", rc.Status, rc.Reason)
				}
			}
		})
	}
}

func unrefreshedOf(t *testing.T, mg *unstructured.Unstructured) ([]string, bool) {
	t.Helper()
	v, found, err := unstructured.NestedStringSlice(mg.Object, "status", "unrefreshedFields")
	if err != nil {
		t.Fatalf("status.unrefreshedFields: %v", err)
	}
	return v, found
}

// status.unrefreshedFields names the fields holding a value this pass did not write: the guarded
// bundle field kept its old value and is listed; the url written fresh is not.
func TestUnrefreshedFieldsNamesTheStaleValue(t *testing.T) {
	h, mg, _ := partialFixture(snowplow.StageOutcomes{Present: true, Failed: []snowplow.StageOutcome{{Name: "bundle", Reason: "NotFound"}}})
	available(t, h, mg)
	got, found := unrefreshedOf(t, mg)
	if !found || !reflect.DeepEqual(got, []string{"caBundleSynced"}) {
		t.Fatalf("unrefreshedFields = %v (found %v), want [caBundleSynced]", got, found)
	}
	// The control: the listed value really is the old one.
	if v, _, _ := unstructured.NestedBool(mg.Object, "status", "caBundleSynced"); !v {
		t.Error("caBundleSynced lost its earlier value, so the fixture does not exercise a stale field")
	}
}

// Written on every projection, `[]` included, so absent means not measured.
func TestUnrefreshedFieldsIsAnEmptyListWhenEverythingWasWritten(t *testing.T) {
	h, mg, _ := partialFixture(snowplow.StageOutcomes{Present: true})
	h.apiResolver.(*fakeResolver).api["bundle"] = map[string]any{"synced": true}
	available(t, h, mg)
	got, found := unrefreshedOf(t, mg)
	if !found || len(got) != 0 {
		t.Fatalf("unrefreshedFields = %v (found %v), want present and empty", got, found)
	}
}

// jq's `//` treats false like null, so `${ .x // empty }` over a stage that resolved and said
// false writes NOTHING, and the old true survives with every stage succeeded. That is a stale
// value under ApiResolved=True, and the list names it: the case no condition reason can catch.
func TestUnrefreshedFieldsCatchesAFalseSwallowedByAlternative(t *testing.T) {
	h, mg, _ := partialFixture(snowplow.StageOutcomes{Present: true})
	h.apiResolver.(*fakeResolver).api["bundle"] = map[string]any{"synced": false}
	available(t, h, mg)
	if c := apiResolvedOf(t, mg); c.Status != metav1.ConditionTrue {
		t.Fatalf("ApiResolved = %s/%s, want True: every stage succeeded", c.Status, c.Reason)
	}
	if v, _, _ := unstructured.NestedBool(mg.Object, "status", "caBundleSynced"); !v {
		t.Fatal("caBundleSynced was written false, so this jq does not behave as the comment says")
	}
	if got, _ := unrefreshedOf(t, mg); !reflect.DeepEqual(got, []string{"caBundleSynced"}) {
		t.Fatalf("unrefreshedFields = %v, want [caBundleSynced]: the stale true must be named", got)
	}
}

// No projection this pass (paused): a list from an earlier pass would describe a pass that is not
// this one, so it is removed.
func TestUnrefreshedFieldsIsRemovedWhenNoProjectionRan(t *testing.T) {
	h, mg, _ := partialFixture(snowplow.StageOutcomes{Present: true})
	_ = unstructured.SetNestedStringSlice(mg.Object, []string{"url"}, "status", "unrefreshedFields")
	if err := h.setStatus(context.Background(), mg, &statusManagerOpts{chartURL: "u", chartVersion: "v", conditionType: ConditionTypeReconcileGracefullyPaused}); err != nil {
		t.Fatalf("setStatus: %v", err)
	}
	if got, found := unrefreshedOf(t, mg); found {
		t.Fatalf("unrefreshedFields = %v survived a pass with no projection", got)
	}
}

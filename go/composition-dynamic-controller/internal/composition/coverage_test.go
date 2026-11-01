package composition

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// THE CASE THIS EXISTS FOR. Ready=True over a minority of the children it names must be
// distinguishable by a MACHINE, not only by reading English.
//
// The count has been in the condition message since #121, and a message is prose: a dependency
// gate, a dashboard and a post-roll check all read the reason and none of them parse English. So
// the coverage gap was invisible to everything that acts on it, which is why nobody noticed how
// large it had become.
//
// Real numbers, measured by the installer session on two clusters.
func TestAvailableReason_PartialCoverageIsDistinguishable(t *testing.T) {
	for _, c := range []struct {
		name               string
		unevaluated, total int
	}{
		{"PortalAgents/portal-agents, the worst observed", 118, 121},
		{"ObservabilityPage/observability-page", 72, 75},
		{"ReviewsPage/reviews-page, identical on both clusters", 71, 74},
		{"IncidentsPage/incidents-page", 62, 65},
		{"CompositionsPage/compositions-page", 62, 67},
	} {
		if got := availableReason(c.unevaluated, c.total); got != ReasonAvailablePartialCoverage {
			t.Errorf("%s (%d of %d unevaluated): reason = %q, want %q — a machine cannot otherwise "+
				"tell this from a fully-assessed composition",
				c.name, c.unevaluated, c.total, got, ReasonAvailablePartialCoverage)
		}
	}
}

// THE CONTROL, and it is what stops this from relabelling every composition. A composition whose
// children were mostly assessed must stay plain Available — the claim still covers what it is
// about. Without this arm, a function that always returned PartialCoverage would pass the test
// above while making the new reason meaningless.
func TestAvailableReason_MostlyAssessedStaysAvailable(t *testing.T) {
	for _, c := range []struct {
		name               string
		unevaluated, total int
	}{
		{"Authn/authn — all core, nothing unevaluated", 0, 10},
		{"one unevaluated child out of many", 1, 121},
		{"just under half", 60, 121},
		{"exactly half is NOT a minority claim", 60, 120},
		{"no children at all", 0, 0},
	} {
		if got := availableReason(c.unevaluated, c.total); got != "Available" {
			t.Errorf("%s (%d of %d): reason = %q, want Available", c.name, c.unevaluated, c.total, got)
		}
	}
}

// The boundary, stated explicitly because it is the one judgement call in the change. "Most of them
// were not assessed" is the line, so a bare majority crosses it and exactly half does not.
func TestAvailableReason_TheBoundaryIsTheMajority(t *testing.T) {
	if got := availableReason(61, 120); got != ReasonAvailablePartialCoverage {
		t.Errorf("61 of 120 unevaluated is a majority: got %q", got)
	}
	if got := availableReason(60, 120); got != "Available" {
		t.Errorf("60 of 120 is exactly half, not a minority claim: got %q", got)
	}
}

// The verdict must NOT change. These children are unevaluated, not unhealthy — reporting them as
// not-ready would hold a working composition back for as long as its widgets publish no conditions,
// which is indefinitely. That is the #72 wedge and the reason childUnevaluated rolls up as healthy
// in the first place.
func TestResolveReady_PartialCoverageIsStillReady(t *testing.T) {
	v := healthVerdict{ready: true, reason: "Available", total: 121, unevaluated: 118}
	out := resolveReady(projectedHealth{}, "Composition is up-to-date", v)

	if out.reason != ReasonAvailablePartialCoverage {
		t.Errorf("reason = %q, want %q", out.reason, ReasonAvailablePartialCoverage)
	}
	// The message must still carry the numbers: the reason says THAT coverage is partial, the
	// message says how partial, and a human needs the second.
	if !strings.Contains(out.message, "118 of 121") {
		t.Errorf("the message must keep the counts, got %q", out.message)
	}
	// And stampReady keys readiness off the reason vocabulary, so a reason it does not recognise
	// must not silently become not-ready. Asserting the verdict here rather than trusting the name.
	if !strings.HasPrefix(out.reason, "Available") {
		t.Errorf("the reason must remain an Available variant or the verdict changes: %q", out.reason)
	}
}

// THE TEST THAT WAS MISSING, and its absence nearly shipped a wedge.
//
// stampReady switches on the reason with NO default arm — an unrecognised reason returns
// `fmt.Errorf("unknown ready reason: %q")`, deliberately, because the arm used to be
// `default: setAvaibleStatus` and a reason added without a case here reported the composition
// HEALTHY silently.
//
// I added AvailablePartialCoverage in resolveReady and every test passed: they exercise
// availableReason and resolveReady and stop one function short of stampReady. Every composition with
// majority-unevaluated children would have failed its status write and wedged. The guard caught it;
// my test boundary did not.
//
// So this test crosses that boundary on purpose — it asserts the reason is STAMPABLE, which is a
// different claim from "the reason is computed correctly".
func TestStampReady_PartialCoverageIsStampable(t *testing.T) {
	mg := compositionWithInventory([]any{managedEntry("flexes", "w1", "krateo-system")}, "d1")

	err := stampReady(mg, readyOutcome{
		reason:  ReasonAvailablePartialCoverage,
		message: "Composition is up-to-date (118 of 121 managed children not health-evaluated)",
	}, true)
	if err != nil {
		t.Fatalf("stampReady rejected the reason it must handle: %v", err)
	}

	conds, found, _ := unstructured.NestedSlice(mg.Object, "status", "conditions")
	if !found || len(conds) == 0 {
		t.Fatal("no condition was stamped, so this test could not tell a handled reason from a no-op")
	}
	var gotStatus, gotReason string
	for _, c := range conds {
		cm, ok := c.(map[string]any)
		if !ok || cm["type"] != "Ready" {
			continue
		}
		gotStatus, _ = cm["status"].(string)
		gotReason, _ = cm["reason"].(string)
	}
	// Status=True is the whole design decision: unevaluated is not unhealthy.
	if gotStatus != "True" {
		t.Errorf("Ready status = %q, want True — partial coverage must not change the verdict", gotStatus)
	}
	if gotReason != ReasonAvailablePartialCoverage {
		t.Errorf("Ready reason = %q, want %q", gotReason, ReasonAvailablePartialCoverage)
	}
}

// THE CONTROL for the test above: the guard must still reject a genuinely unknown reason. Otherwise
// "stampReady accepts AvailablePartialCoverage" could be true of a stampReady that accepts anything,
// which is the pre-#165 default-arm bug.
func TestStampReady_StillRejectsAnUnknownReason(t *testing.T) {
	mg := compositionWithInventory([]any{managedEntry("flexes", "w1", "krateo-system")}, "d1")
	err := stampReady(mg, readyOutcome{reason: "SomethingNobodyAdded", message: "x"}, true)
	if err == nil {
		t.Fatal("an unrecognised reason must error loudly; a quiet Available is the #165 defect")
	}
	if !strings.Contains(err.Error(), "unknown ready reason") {
		t.Errorf("the error should name the problem, got: %v", err)
	}
}

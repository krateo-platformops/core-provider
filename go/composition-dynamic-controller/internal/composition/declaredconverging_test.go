package composition

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// #234. A blueprint could say "I am not ready" and nothing else. That one bit was read as
// Unavailable — BROKEN — so a composition that is deliberately mid-bump had to either lie green or
// declare an outage. The installer's #230 fix needs the middle answer: converging.
//
// The optional `.status.health.reason` supplies it. Present and "Creating": not ready YET. Absent:
// exactly the pre-#234 behaviour, which every existing blueprint depends on.

// healthObj builds a composition carrying a blueprint's projected health. A reason of "" is left
// unset entirely, so this also covers the absent-field case rather than an empty-string stand-in
// for it — the two are different states and the whole feature is that one of them is inert.
func healthObj(ready bool, reason, message string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{}}
	_ = unstructured.SetNestedField(u.Object, ready, "status", "health", "ready")
	if reason != "" {
		_ = unstructured.SetNestedField(u.Object, reason, "status", "health", "reason")
	}
	if message != "" {
		_ = unstructured.SetNestedField(u.Object, message, "status", "health", "message")
	}
	return u
}

// readyCondition returns (status, reason, message) of the stamped Ready condition.
func readyCondition(t *testing.T, mg *unstructured.Unstructured) (string, string, string) {
	t.Helper()
	conds, found, _ := unstructured.NestedSlice(mg.Object, "status", "conditions")
	if !found {
		t.Fatal("no status.conditions were stamped at all")
	}
	for _, c := range conds {
		cm, ok := c.(map[string]any)
		if !ok || stringField(cm, "type") != "Ready" {
			continue
		}
		return stringField(cm, "status"), stringField(cm, "reason"), stringField(cm, "message")
	}
	t.Fatal("no Ready condition among the stamped conditions")
	return "", "", ""
}

// THE TEST THIS ISSUE IS ABOUT, and it is a differential: the same declaration with and without the
// reason, through the real wiring, ending at what a PARENT concludes.
//
// Both arms run the actual chain — deriveReadyFromHealth -> readProjectedHealth -> resolveReady ->
// stampReady -> setCreatingStatus/setUnavailableStatus — and then feed the stamped object to
// krateoReady, which is how a parent composition reads a nested one. That closes the loop #234
// exists to close: it is not enough for the cdc to *say* Creating, the parent has to stop treating
// it as a fault.
//
// This is deliberately not an AST guard. Five times this session a decision function was covered
// while its call site was not, and deriveReadyFromHealth turns out to be drivable with a nil
// kubeconfig (the rollup is skipped and defaults to Available), so the real path is reachable. A
// guard asserting the call exists would not have caught a reason-string mismatch between what
// resolveReady chooses and what krateoReady matches on — and that mismatch is the only way this
// feature can be wired up correctly end to end and still not work.
func TestDeclaredConverging_RoundTripsToChildConverging(t *testing.T) {
	for _, c := range []struct {
		name       string
		reason     string
		wantReason string
		wantState  childState
		why        string
	}{
		{
			name: "declared converging", reason: "Creating",
			wantReason: "Creating", wantState: childConverging,
			why: "a composition that declared itself mid-bump must hold its parent back WITHOUT " +
				"reporting a fault; childFailed here is the loud wrong answer #234 removes",
		},
		{
			name: "no reason declared (the pre-#234 world)", reason: "",
			wantReason: "Unavailable", wantState: childFailed,
			why: "every blueprint written before #234 projects ready=false with no reason and must " +
				"keep meaning exactly what it meant; this arm is the control",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			mg := healthObj(false, c.reason, "bumping to 2.0.0")
			h := &handler{} // nil kubeconfig: the rollup is skipped, so the projection decides

			if err := h.deriveReadyFromHealth(context.Background(), mg,
				&statusManagerOpts{message: "Composition is up-to-date", force: true}); err != nil {
				t.Fatalf("deriveReadyFromHealth: %v", err)
			}

			status, reason, message := readyCondition(t, mg)
			if reason != c.wantReason {
				t.Errorf("stamped Ready reason = %q, want %q", reason, c.wantReason)
			}
			if status != "False" {
				t.Errorf("stamped Ready status = %q, want False — a declared not-ready composition "+
					"must still block `deps:` edges either way", status)
			}
			if message != "bumping to 2.0.0" {
				t.Errorf("the author's message was lost: %q", message)
			}

			// Now the other end of the trip: what a parent concludes from that condition.
			if got := krateoReady(mg); got != c.wantState {
				t.Errorf("a parent reads this child as %v, want %v: %s", got, c.wantState, c.why)
			}
		})
	}
}

// The recognised set, and the direction an unrecognised value falls.
//
// Unrecognised -> Unavailable is the SAFE direction and the asymmetry is the point: a typo read as
// Creating would silently downgrade a real failure to "still converging" and wait on it forever,
// while a typo read as Unavailable is the same loud answer the author got before #234. Reasons that
// look like synonyms ("Progressing", "Converging", "Pending") are deliberately NOT accepted —
// guessing at a vocabulary is how the chart-side projection ends up built against the wrong one.
func TestNotReadyReason_OnlyCreatingIsRecognised(t *testing.T) {
	for _, c := range []struct {
		reason string
		want   string
	}{
		{"Creating", "Creating"},
		{"creating", "Creating"}, // case-insensitive, like the ready bool's string form
		{"CREATING", "Creating"},

		{"", "Unavailable"},            // absent: the pre-#234 default
		{"Unavailable", "Unavailable"}, // explicit, and the same answer
		{"Progressing", "Unavailable"}, // a plausible synonym, not accepted
		{"Converging", "Unavailable"},
		{"Pending", "Unavailable"},
		{"Creatin", "Unavailable"}, // a typo must NOT become converging
		// Zero evidence is not something a projection can claim: having run a health check at all
		// IS evidence, which is why resolveReady's ZeroEvidence arm honours a projected ready=true.
		{"ZeroEvidence", "Unavailable"},
		{"Available", "Unavailable"}, // contradicts ready=false; the bool wins
	} {
		p := projectedHealth{present: true, ready: false, reason: c.reason}
		if got := p.notReadyReason(); got != c.want {
			t.Errorf("reason %q -> %q, want %q", c.reason, got, c.want)
		}
	}
}

// A positively-observed TERMINAL failure outranks the author's declaration. "I am converging" is a
// claim about intent; a failed Job is an observation, and the observation wins — the same asymmetry
// resolveReady already applies to projected ready=TRUE, which may veto health but may not overrule
// what the rollup can see.
//
// Without this, #234 would hand blueprint authors a way to mask real failures behind a permanent
// "converging", which is the silent-wrong-answer failure mode #165 was about, re-entering through
// the fix for its opposite.
func TestDeclaredConverging_ObservedFailureOutranksIt(t *testing.T) {
	v := healthVerdict{
		ready: false, reason: "Unavailable",
		message: "managed children not healthy: Job/ns/migrate",
		failing: []string{"Job/ns/migrate"},
		total:   3,
	}
	p := projectedHealth{present: true, ready: false, reason: "Creating", message: "bumping to 2.0.0"}

	out := resolveReady(p, "Composition is up-to-date", v)
	if out.reason != "Unavailable" {
		t.Errorf("reason = %q, want Unavailable: a declared transient must not mask a child we "+
			"positively observed has failed and will not recover on its own", out.reason)
	}
	// Both signals survive: the author's verdict leads, the rollup's names stay actionable.
	if !strings.Contains(out.message, "bumping to 2.0.0") {
		t.Errorf("the author's message was dropped: %q", out.message)
	}
	if !strings.Contains(out.message, "Job/ns/migrate") {
		t.Errorf("the failing child was not named, leaving an operator nothing to reach: %q", out.message)
	}
}

// A declared-converging composition whose children are merely CONVERGING (not failed) stays
// Creating. The guard above keys on v.failing specifically, not on "the rollup disagreed" — if it
// keyed on the latter, the ordinary mid-bump case (children restarting, so converging) would be
// reported as an outage, which is precisely the state #234 exists to express.
func TestDeclaredConverging_SurvivesConvergingChildren(t *testing.T) {
	v := healthVerdict{
		ready: false, reason: "Creating",
		message:    "1 of 3 managed children are not ready: Deployment/ns/api",
		converging: []string{"Deployment/ns/api"},
		total:      3,
	}
	p := projectedHealth{present: true, ready: false, reason: "Creating", message: "bumping to 2.0.0"}

	if out := resolveReady(p, "Composition is up-to-date", v); out.reason != "Creating" {
		t.Errorf("reason = %q, want Creating: both signals say 'not yet', and neither says 'broken'",
			out.reason)
	}
}

// An author who declares a reason but no message still gets a usable one.
func TestDeclaredConverging_FallsBackToThePhaseMessage(t *testing.T) {
	p := projectedHealth{present: true, ready: false, reason: "Creating"}
	out := resolveReady(p, "Composition values updated", healthVerdict{ready: true, reason: "Available"})
	if out.reason != "Creating" {
		t.Fatalf("reason = %q, want Creating", out.reason)
	}
	if out.message != "Composition values updated" {
		t.Errorf("message = %q, want the phase message", out.message)
	}
}

// The reason must be IGNORED when the author did not declare not-ready. A reason left behind in the
// projection from an earlier reconcile cannot be allowed to drag a healthy composition off
// Available — the bool is the verdict, the reason only qualifies a negative one.
func TestDeclaredConverging_ReasonIsInertWhenReady(t *testing.T) {
	p := projectedHealth{present: true, ready: true, reason: "Creating", message: "all good"}
	if out := resolveReady(p, "Composition is up-to-date", healthVerdict{ready: true, reason: "Available"}); out.reason != "Available" {
		t.Errorf("reason = %q, want Available: ready=true with a stale reason beside it is still ready",
			out.reason)
	}
}

// The reader picks up the optional field, and reports it absent rather than empty-ish when it is
// not there. Reading it is the only part of the chain a unit test of resolveReady cannot cover.
func TestReadProjectedHealth_ReadsTheOptionalReason(t *testing.T) {
	got := readProjectedHealth(healthObj(false, "Creating", "bumping"))
	if !got.present || got.ready {
		t.Fatalf("present=%v ready=%v, want present and not ready", got.present, got.ready)
	}
	if got.reason != "Creating" {
		t.Errorf("reason = %q, want Creating", got.reason)
	}
	if got.message != "bumping" {
		t.Errorf("message = %q, want bumping", got.message)
	}

	// Absent reason: the field is simply not there, and must read as empty so notReadyReason
	// returns today's answer.
	bare := readProjectedHealth(healthObj(false, "", ""))
	if bare.reason != "" {
		t.Errorf("reason = %q on an object that has no reason field at all", bare.reason)
	}
	if !bare.present || bare.ready {
		t.Errorf("present=%v ready=%v, want present and not ready", bare.present, bare.ready)
	}
}

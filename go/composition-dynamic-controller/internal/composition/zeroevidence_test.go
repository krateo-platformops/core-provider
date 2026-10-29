package composition

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// namedKrateoCR is krateoCR with a name, so several can sit in one fake client.
func namedKrateoCR(name, readyStatus, reason string, hasConds bool) *unstructured.Unstructured {
	u := krateoCR(readyStatus, reason, hasConds)
	_ = unstructured.SetNestedField(u.Object, name, "metadata", "name")
	return u
}

func krateoRef(name string) map[string]any {
	return map[string]any{
		"apiVersion": "composition.krateo.io/v0-1-0", "resource": "portals",
		"name": name, "namespace": "ns",
	}
}

// #165: a composition that evaluated NONE of its children must not report Available.
//
// The caveat already said so in the message — "(N of M managed children not health-evaluated)" —
// while the condition said Available anyway. That combination is the one nobody reads as a warning,
// which is how 0 of 44 blueprints noticed they never attached a RESTAction.
func TestRollup_ZeroEvidenceIsNotAvailable(t *testing.T) {
	h := &handler{}
	// A krateo.io CR with no conditions at all: childUnevaluated, the authn-seed shape.
	child := namedKrateoCR("opaque", "", "", false)
	mg := &unstructured.Unstructured{Object: map[string]any{}}
	setManaged(mg, krateoRef("opaque"))

	v := h.rollupManagedChildren(context.Background(), fakeDynWith(child), mg)

	if v.reason == "Available" {
		t.Fatalf("reported Available over zero evaluated children (%d of %d unevaluated)",
			v.unevaluated, v.total)
	}
	if v.reason != "ZeroEvidence" {
		t.Errorf("reason = %q, want ZeroEvidence", v.reason)
	}
	if v.ready {
		t.Error("ready = true on zero evidence")
	}
	if v.unevaluated != 1 || v.total != 1 {
		t.Errorf("unevaluated/total = %d/%d, want 1/1", v.unevaluated, v.total)
	}
	if !strings.Contains(v.message, "unknown") {
		t.Errorf("message does not say the health is unknown: %q", v.message)
	}
}

// The control arm, and a deliberate non-change. 32 of the 33 compositions carrying the caveat on
// krateo-057 are PARTIAL — the largest at 142 of 145 — and they keep reporting Available.
//
// Evidence from a few children is thin, but it is not nothing, and there is no principled
// threshold between 142/145 and 5/5 that is not simply a number somebody picked. Without this
// test, the fix above could silently widen to every caveat-carrying composition on the fleet.
func TestRollup_PartialEvidenceStaysAvailable(t *testing.T) {
	h := &handler{}
	opaque := namedKrateoCR("opaque", "", "", false)            // unevaluated
	healthy := namedKrateoCR("good", "True", "Available", true) // evaluated, healthy
	mg := &unstructured.Unstructured{Object: map[string]any{}}
	setManaged(mg, krateoRef("opaque"), krateoRef("good"))

	v := h.rollupManagedChildren(context.Background(), fakeDynWith(opaque, healthy), mg)

	if v.reason != "Available" || !v.ready {
		t.Fatalf("one evaluated child out of two must still be Available: got ready=%v reason=%s",
			v.ready, v.reason)
	}
	if v.unevaluated != 1 {
		t.Errorf("unevaluated = %d, want 1", v.unevaluated)
	}
	// The caveat must survive: it is the whole reporting mechanism for the partial case.
	if !strings.Contains(v.message, "not health-evaluated") {
		t.Errorf("partial coverage lost its caveat: %q", v.message)
	}
}

// A composition with NO managed children is not zero evidence — there was nothing to evaluate, and
// it is legitimately up to date. Guards the `len(managed) > 0` half of the condition.
func TestRollup_NoManagedChildrenIsStillAvailable(t *testing.T) {
	h := &handler{}
	mg := &unstructured.Unstructured{Object: map[string]any{}}

	v := h.rollupManagedChildren(context.Background(), fakeDynWith(), mg)
	if !v.ready || v.reason != "Available" {
		t.Fatalf("a composition with no children must be Available: got ready=%v reason=%s", v.ready, v.reason)
	}
}

// The load-bearing case, and the reason this could not ship as a one-line verdict change.
//
// On krateo-057 the Installer composition — the root of the install — lists
// modelconfigs/model-configs among its managed children, and model-configs is one of the two
// compositions at zero evidence. krateoReady maps any present-and-not-True Ready to
// childConverging, so without the ZeroEvidence branch the Installer composition would have gone
// Ready=False permanently the moment this shipped, and eight agent components gate on it.
//
// Ignorance must propagate as ignorance, not as not-ready.
func TestRollup_ParentOfAZeroEvidenceChildStaysAvailable(t *testing.T) {
	h := &handler{}
	zeroEv := namedKrateoCR("model-configs", string(metav1.ConditionUnknown), "ZeroEvidence", true)
	healthy := namedKrateoCR("authn", "True", "Available", true)
	mg := &unstructured.Unstructured{Object: map[string]any{}}
	setManaged(mg, krateoRef("model-configs"), krateoRef("authn"))

	v := h.rollupManagedChildren(context.Background(), fakeDynWith(zeroEv, healthy), mg)

	if !v.ready {
		t.Fatalf("a parent holding a zero-evidence child went not-ready (reason=%s, message=%q). "+
			"On 057 this is the Installer composition, and eight components gate on it",
			v.reason, v.message)
	}
	if len(v.converging) != 0 {
		t.Errorf("the zero-evidence child was counted converging: %v", v.converging)
	}
	if v.unevaluated != 1 {
		t.Errorf("unevaluated = %d, want 1 — it should be counted, never acted on", v.unevaluated)
	}
}

// Keyed on the REASON, never on bare Unknown. A genuinely converging child also reports a non-True
// Ready and must keep holding its parent back; that transient is what childConverging exists for.
// Treating every Unknown as unevaluated would make a real wait invisible.
func TestKrateoReady_UnknownOnlyCountsAsUnevaluatedWithTheZeroEvidenceReason(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status string
		reason string
		want   childState
	}{
		{"Unknown + ZeroEvidence", string(metav1.ConditionUnknown), "ZeroEvidence", childUnevaluated},
		{"Unknown + Creating (a real wait)", string(metav1.ConditionUnknown), "Creating", childConverging},
		{"Unknown with no reason", string(metav1.ConditionUnknown), "", childConverging},
		{"False + Unavailable", "False", "Unavailable", childFailed},
		{"True + Available", "True", "Available", childHealthy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := krateoReady(krateoCR(tc.status, tc.reason, true)); got != tc.want {
				t.Errorf("krateoReady = %d, want %d", got, tc.want)
			}
		})
	}
}

// resolveReady must pass the verdict through rather than fold it into Available — the default arm
// of deriveReadyFromHealth stamps Available, so a reason that falls through reports healthy, which
// is the exact failure #165 is about.
func TestResolveReady_ZeroEvidencePassesThrough(t *testing.T) {
	v := healthVerdict{ready: false, reason: "ZeroEvidence", message: "health is unknown", total: 5, unevaluated: 5}

	out := resolveReady(projectedHealth{}, "Composition is up-to-date", v)
	if out.reason != "ZeroEvidence" {
		t.Fatalf("reason = %q, want ZeroEvidence — anything else is stamped Available downstream", out.reason)
	}
	if out.message != v.message {
		t.Errorf("message = %q, want the rollup's own", out.message)
	}
}

// A blueprint's own health RESTAction IS evidence, and better evidence than the generic rollup's.
// So a projected ready=true turns zero evidence into one positive signal and the composition is
// Available. This is the escape hatch for a blueprint whose children we cannot read but whose
// author can.
func TestResolveReady_ProjectedHealthOverridesZeroEvidence(t *testing.T) {
	v := healthVerdict{ready: false, reason: "ZeroEvidence", message: "health is unknown", total: 3, unevaluated: 3}

	out := resolveReady(projectedHealth{present: true, ready: true, message: "api answered 200"}, "Composition is up-to-date", v)
	if out.reason != "Available" {
		t.Fatalf("reason = %q, want Available: the author's health check is evidence", out.reason)
	}
	if out.message != "api answered 200" {
		t.Errorf("message = %q, want the author's", out.message)
	}

	// Control arm: a projected ready=FALSE must still be honoured as Unavailable, not softened
	// into ZeroEvidence by the new branch.
	if out := resolveReady(projectedHealth{present: true, ready: false, message: "api answered 503"}, "Composition is up-to-date", v); out.reason != "Unavailable" {
		t.Errorf("projected ready=false gave reason %q, want Unavailable", out.reason)
	}
}

// The condition that actually reaches the object: Unknown, with the literal reason the installer
// chart's Ready gate matches on.
func TestSetZeroEvidenceStatus_StampsUnknownWithTheMatchedReason(t *testing.T) {
	mg := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "composition.krateo.io/v0-1-0", "kind": "Portal",
		"metadata": map[string]any{"name": "p", "namespace": "ns"},
	}}
	if err := setZeroEvidenceStatus(mg, "health is unknown", true); err != nil {
		t.Fatalf("setZeroEvidenceStatus: %v", err)
	}

	conds, found, _ := unstructured.NestedSlice(mg.Object, "status", "conditions")
	if !found || len(conds) != 1 {
		t.Fatalf("got %d conditions, want 1", len(conds))
	}
	cm, _ := conds[0].(map[string]any)
	if stringField(cm, "type") != "Ready" {
		t.Errorf("type = %q, want Ready", stringField(cm, "type"))
	}
	if got := stringField(cm, "status"); got != string(metav1.ConditionUnknown) {
		t.Errorf("status = %q, want Unknown — False would block every readiness gate indefinitely", got)
	}
	if got := stringField(cm, "reason"); got != "ZeroEvidence" {
		t.Errorf("reason = %q, want the literal \"ZeroEvidence\": the installer chart's Ready gate "+
			"matches that string, so a rename breaks the gate silently", got)
	}
}

// The routing from a readyOutcome to an actual condition. Every setter was tested individually
// while the SWITCH that chooses between them was not, because deriveReadyFromHealth needs a
// kubeconfig to reach the rollup. Mutation caught it: dropping the ZeroEvidence arm failed nothing.
//
// It matters here more than in a typical switch because the default arm stamps AVAILABLE. A reason
// that falls through does not error and does not stamp nothing — it reports healthy, which is the
// very defect #165 exists to fix.
func TestStampReady_RoutesEveryReasonToTheRightCondition(t *testing.T) {
	for _, tc := range []struct {
		reason     string
		wantStatus string
		wantReason string
	}{
		{"Available", "True", "Available"},
		{"Creating", "False", "Creating"},
		{"Unavailable", "False", "Unavailable"},
		{"ZeroEvidence", string(metav1.ConditionUnknown), "ZeroEvidence"},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			mg := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "composition.krateo.io/v0-1-0", "kind": "Portal",
				"metadata": map[string]any{"name": "p", "namespace": "ns"},
			}}
			if err := stampReady(mg, readyOutcome{reason: tc.reason, message: "m"}, true); err != nil {
				t.Fatalf("stampReady: %v", err)
			}
			conds, _, _ := unstructured.NestedSlice(mg.Object, "status", "conditions")
			if len(conds) != 1 {
				t.Fatalf("got %d conditions, want 1", len(conds))
			}
			cm, _ := conds[0].(map[string]any)
			if got := stringField(cm, "status"); got != tc.wantStatus {
				t.Errorf("status = %q, want %q", got, tc.wantStatus)
			}
			if got := stringField(cm, "reason"); got != tc.wantReason {
				t.Errorf("reason = %q, want %q", got, tc.wantReason)
			}
		})
	}
}

// TestStampReady_UnrecognisedReasonFallsBackToAvailable lived here. It asserted that an unknown
// reason stamps Available, and its own failure message said: "if that changed deliberately, the
// comment on stampReady and this test both need updating."
//
// It changed deliberately. That test documented a HAZARD rather than a wanted behaviour — it
// existed to make the default arm's silence visible — and the hazard is now closed: stampReady
// handles Available explicitly and returns an error for anything it does not recognise.
//
// The property moved to stampready_exhaustive_test.go, which asserts the error, keeps a control
// arm over all four real reasons, and adds the check this one could not: that every reason
// resolveReady can PRODUCE has an arm, so adding a reason is what breaks the build.

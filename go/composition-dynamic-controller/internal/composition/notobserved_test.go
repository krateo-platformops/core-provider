package composition

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// provider-runtime v1.8.0 introduces Ready=Unknown/NotObserved for "the external resource could not
// be observed" — an Observe or Connect failure — leaving the raw error on Synced.
//
// Before v1.8.0 such a provider set ONLY Synced=False/ReconcileError and left Ready at its last
// value, so a parent could read Ready=True over a child nothing had assessed. v1.8.0 fixes that at
// the source; this file is the rollup's half of the contract.
//
// THE CDC MUST TREAT IT EXACTLY LIKE ZeroEvidence: unevaluated. The child is not saying it is
// unhealthy, and it is not saying it is still coming up — it is saying NOTHING ASSESSED IT. Without
// the branch it falls to krateoReady's default and the parent reports Ready=False/Creating, which
// means an apiserver blip on one child (a CompositionDefinition, Alert, RestDefinition, Repo or
// LocalResource) makes installer's depsReady hold back first creation of every dependent and
// verify-roll withhold its verdict. That is the #165 shape, arriving through a new door.

// notObservedChild is the shape provider-runtime v1.8.0 actually writes: Ready=Unknown/NotObserved
// AND Synced=False/ReconcileError on the same object. Both are present deliberately — a fixture
// carrying only the Ready half would not prove the rollup ignores the Synced error, and the Synced
// condition is where the real diagnostic lives.
func notObservedChild(t *testing.T, name string) *unstructured.Unstructured {
	t.Helper()
	u := namedKrateoCR(name, string(metav1.ConditionUnknown), "NotObserved", true)
	conds, found, err := unstructured.NestedSlice(u.Object, "status", "conditions")
	if err != nil || !found {
		t.Fatalf("premise: the fixture must already carry a Ready condition (found=%v err=%v)", found, err)
	}
	conds = append(conds, map[string]any{
		"type":    "Synced",
		"status":  "False",
		"reason":  "ReconcileError",
		"message": "connect failed: Get \"https://10.0.0.1:443/api\": dial tcp: i/o timeout",
	})
	if err := unstructured.SetNestedSlice(u.Object, conds, "status", "conditions"); err != nil {
		t.Fatalf("setting conditions: %v", err)
	}
	return u
}

// THE DIFFERENTIAL, which is what provider-runtime asked for: a NotObserved child must leave the
// parent in EXACTLY the state a ZeroEvidence child does.
//
// Asserted as an equality between two rollups rather than as a list of expected values, because
// the contract is "indistinguishable from ZeroEvidence". If the rollup's treatment of ZeroEvidence
// ever changes, this test follows it rather than pinning a value that silently stops matching.
func TestRollup_NotObservedIsIndistinguishableFromZeroEvidence(t *testing.T) {
	h := &handler{}

	run := func(child *unstructured.Unstructured, childName string) healthVerdict {
		healthy := namedKrateoCR("authn", "True", "Available", true)
		mg := &unstructured.Unstructured{Object: map[string]any{}}
		setManaged(mg, krateoRef(childName), krateoRef("authn"))
		return h.rollupManagedChildren(context.Background(), fakeDynWith(child, healthy), mg)
	}

	zero := run(namedKrateoCR("zero-ev", string(metav1.ConditionUnknown), "ZeroEvidence", true), "zero-ev")
	notObs := run(notObservedChild(t, "not-obs"), "not-obs")

	// CONTROL FIRST: the ZeroEvidence arm must itself be the healthy-parent case, or the equality
	// below could hold with both arms wrong together.
	if !zero.ready || zero.unevaluated != 1 || zero.total != 2 {
		t.Fatalf("CONTROL: a parent of one ZeroEvidence child and one healthy child should be "+
			"ready with 1 of 2 unevaluated; got ready=%v unevaluated=%d total=%d reason=%q. "+
			"The comparison below proves nothing until this holds",
			zero.ready, zero.unevaluated, zero.total, zero.reason)
	}

	if notObs.ready != zero.ready {
		t.Errorf("ready = %v for NotObserved, %v for ZeroEvidence: an unobserved child must not "+
			"hold its parent back any more than an unevaluable one does", notObs.ready, zero.ready)
	}
	if notObs.reason != zero.reason {
		t.Errorf("reason = %q for NotObserved, %q for ZeroEvidence — the two must be "+
			"indistinguishable to every consumer that reads the reason", notObs.reason, zero.reason)
	}
	if notObs.unevaluated != zero.unevaluated {
		t.Errorf("unevaluated = %d for NotObserved, %d for ZeroEvidence: it must be COUNTED, so "+
			"partial coverage stays honest, and never acted on",
			notObs.unevaluated, zero.unevaluated)
	}
	if len(notObs.converging) != 0 {
		t.Errorf("the NotObserved child was counted converging (%v). That is the defect: the "+
			"parent would report Ready=False/Creating and installer's depsReady would hold back "+
			"every dependent on an Observe outage", notObs.converging)
	}
	if len(notObs.failing) != 0 {
		t.Errorf("the NotObserved child was counted failing (%v) — it is unassessed, not unhealthy",
			notObs.failing)
	}
}

// ALL children NotObserved: the parent must land on ZeroEvidence/Unknown, which is the other state
// installer's gate accepts. This is the answer to "what does the parent report", for the
// all-or-nothing case, and it is a different branch of the rollup from the partial case above.
func TestRollup_AllChildrenNotObservedIsZeroEvidence(t *testing.T) {
	h := &handler{}
	a, b := notObservedChild(t, "child-a"), notObservedChild(t, "child-b")
	mg := &unstructured.Unstructured{Object: map[string]any{}}
	setManaged(mg, krateoRef("child-a"), krateoRef("child-b"))

	v := h.rollupManagedChildren(context.Background(), fakeDynWith(a, b), mg)

	if v.reason != "ZeroEvidence" {
		t.Errorf("reason = %q, want ZeroEvidence when NOTHING was evaluated", v.reason)
	}
	if v.unevaluated != 2 || v.total != 2 {
		t.Errorf("unevaluated/total = %d/%d, want 2/2", v.unevaluated, v.total)
	}
	if len(v.converging) != 0 || len(v.failing) != 0 {
		t.Errorf("converging=%v failing=%v, want both empty", v.converging, v.failing)
	}
}

// THE OVER-BREADTH GUARD. Adding NotObserved must not have widened the unevaluated branch to catch
// a genuinely converging child, which would make a real wait invisible. Keyed on the REASON, never
// on bare Unknown — the same discipline the ZeroEvidence branch documents.
//
// The NotObserved row is spelled exactly as provider-runtime defines it (case-sensitive, in
// apis/common/v1). The near-miss rows exist because a casing or spelling difference would fall into
// the converging arm and reintroduce the defect silently, which is the failure this whole change
// exists to prevent.
func TestKrateoReady_NotObservedExactlyAndNothingNear(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status string
		reason string
		want   childState
		why    string
	}{
		{
			name: "Unknown + NotObserved", status: string(metav1.ConditionUnknown),
			reason: "NotObserved", want: childUnevaluated,
			why: "the case provider-runtime v1.8.0 introduces",
		},
		{
			name: "Unknown + ZeroEvidence", status: string(metav1.ConditionUnknown),
			reason: "ZeroEvidence", want: childUnevaluated,
			why: "the existing sibling; if this breaks the new case was bolted on wrongly",
		},
		{
			name: "Unknown + Creating (a real wait)", status: string(metav1.ConditionUnknown),
			reason: "Creating", want: childConverging,
			why: "a genuinely converging child must still hold the parent back",
		},
		{
			name: "Unknown + notobserved (wrong case)", status: string(metav1.ConditionUnknown),
			reason: "notobserved", want: childConverging,
			why: "the reason is case-sensitive; matching loosely would catch reasons nobody vetted",
		},
		{
			name: "Unknown + NotObservedYet (near miss)", status: string(metav1.ConditionUnknown),
			reason: "NotObservedYet", want: childConverging,
			why: "a prefix match would admit any future reason starting with NotObserved",
		},
		{
			name: "False + NotObserved", status: "False",
			reason: "NotObserved", want: childUnevaluated,
			why: "keyed on the reason, not the status: provider-runtime promises Unknown, but a " +
				"consumer that only honoured Unknown would regress if that ever changed",
		},
		{
			name: "False + Unavailable", status: "False",
			reason: "Unavailable", want: childFailed,
			why: "the failing arm must be untouched",
		},
		{
			name: "True + Available", status: "True",
			reason: "Available", want: childHealthy,
			why: "the healthy arm must be untouched",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := krateoReady(krateoCR(tc.status, tc.reason, true)); got != tc.want {
				t.Errorf("krateoReady(status=%q reason=%q) = %d, want %d: %s",
					tc.status, tc.reason, got, tc.want, tc.why)
			}
		})
	}
}

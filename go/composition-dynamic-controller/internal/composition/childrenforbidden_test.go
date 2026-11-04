package composition

import (
	"context"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// #318. A child the controller's ServiceAccount cannot READ used to be counted HEALTHY and
// surfaced nowhere but a Debug log, so one unreadable child and 121 produced byte-identical
// output: Ready=True / Available with the same message.
//
// The population has never been measured, because nothing counted it.

// fakeDynForbidding returns a client that refuses a GET of the named objects with a real 403, the
// way the apiserver does when the SA has no `get` on the kind.
//
// A REACTOR RATHER THAN AN ABSENT OBJECT, because those are the two cases the rollup must keep
// apart: an absent object is childAbsent (NotFound, rolls up converging, #261) and a refused one is
// childUnreadable. A test that simulated "unreadable" by omitting the object would be exercising
// the NotFound branch and would pass against the defect.
func fakeDynForbidding(forbidden []string, objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	c := fakeDynWith(objs...)
	deny := map[string]bool{}
	for _, n := range forbidden {
		deny[n] = true
	}
	c.PrependReactor("get", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		g, ok := a.(k8stesting.GetAction)
		if !ok || !deny[g.GetName()] {
			return false, nil, nil
		}
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Group: "composition.krateo.io", Resource: "portals"},
			g.GetName(),
			//nolint:err113 // the apiserver's own message shape; this is test scaffolding
			errUnauthorizedForTest{},
		)
	})
	return c
}

type errUnauthorizedForTest struct{}

func (errUnauthorizedForTest) Error() string {
	return `portals.composition.krateo.io is forbidden: User "system:serviceaccount:krateo-system:composition-portal" cannot get resource "portals" in API group "composition.krateo.io"`
}

// A 403 must be its own state, not childHealthy. Asserted at the classifier rather than only
// through the rollup, because every verdict arm below depends on this one mapping.
func TestChildHealth_ForbiddenIsUnreadableNotHealthy(t *testing.T) {
	h := &handler{}
	got := h.childHealth(context.Background(), fakeDynForbidding([]string{"secret-child"}), krateoRef("secret-child"))
	if got == childHealthy {
		t.Fatal("a child the SA was REFUSED a read of maps to childHealthy. That is the claim " +
			"#318 exists to remove: it is not an assertion about the child, it is the absence of one")
	}
	if got != childUnreadable {
		t.Errorf("childHealth = %d, want childUnreadable (%d)", got, childUnreadable)
	}
}

// THE SCOPING ASSERTION, and it is the one that keeps the new count meaningful. A missing REST
// mapping or a transient GET failure must stay silently healthy, exactly as before: neither is
// actionable, and reporting them would make unreadableChildrenCount mean "something went wrong
// reading this" rather than "RBAC refused us" — which is the only reading that tells an operator
// what to fix.
func TestChildHealth_ANonForbiddenReadFailureStaysSilentlyHealthy(t *testing.T) {
	h := &handler{}
	c := fakeDynWith()
	c.PrependReactor("get", "*", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("etcd leader election in progress")
	})
	if got := h.childHealth(context.Background(), c, krateoRef("blip")); got != childHealthy {
		t.Errorf("a transient 503 maps to %d, want childHealthy (%d). Folding non-RBAC read "+
			"failures into childUnreadable would turn an apiserver blip into an RBAC report, and "+
			"an operator would go looking for a grant that is not missing", got, childHealthy)
	}
}

// NOT ONE CHILD ASSESSED, and RBAC is why. Diego's rule, and the narrowest verdict change
// available: the composition reports Unknown only when it has no evidence at all.
func TestRollup_EveryChildForbiddenIsUnknownChildrenForbidden(t *testing.T) {
	h := &handler{}
	mg := &unstructured.Unstructured{Object: map[string]any{}}
	setManaged(mg, krateoRef("a"), krateoRef("b"))

	v := h.rollupManagedChildren(context.Background(), fakeDynForbidding([]string{"a", "b"}), mg)

	if v.ready {
		t.Fatal("ready = true over zero assessed children, every one of them refused")
	}
	if v.reason != ReasonChildrenForbidden {
		t.Fatalf("reason = %q, want %q. ZeroEvidence would say the children published nothing, "+
			"which is a claim about THEM; this is a gap in the RBAC we generate, which is ours",
			v.reason, ReasonChildrenForbidden)
	}
	if len(v.unreadable) != 2 {
		t.Errorf("unreadable = %v, want both children", v.unreadable)
	}
	// NAMES in the message, for the #289 reason the converging list already records: a count tells
	// an operator something is wrong and gives them no way to reach it.
	for _, n := range []string{"a", "b"} {
		if !strings.Contains(v.message, n) {
			t.Errorf("message does not name child %q: %s", n, v.message)
		}
	}
	if !strings.Contains(v.message, "refused") {
		t.Errorf("message does not say the read was refused, so it reads as the children's "+
			"problem rather than as a missing grant: %s", v.message)
	}
}

// THE HOLE THE LITERAL RULE WOULD HAVE LEFT. "Unknown when every child is unreadable" keyed on
// len(unreadable) == total would leave this composition — 1 refused, 1 unevaluable, NOTHING
// assessed — reporting Available, which is the same defect in a different mix. The arm is keyed on
// unevaluated+unreadable == total for that reason.
func TestRollup_ForbiddenPlusUnevaluatedWithNothingAssessedIsStillUnknown(t *testing.T) {
	h := &handler{}
	opaque := namedKrateoCR("opaque", "", "", false) // childUnevaluated
	mg := &unstructured.Unstructured{Object: map[string]any{}}
	setManaged(mg, krateoRef("refused"), krateoRef("opaque"))

	v := h.rollupManagedChildren(context.Background(), fakeDynForbidding([]string{"refused"}, opaque), mg)

	if v.ready {
		t.Fatalf("ready = true with 1 refused and 1 unevaluable child: nothing was assessed, so "+
			"Available is a claim about an empty set (reason=%q)", v.reason)
	}
	if v.reason != ReasonChildrenForbidden {
		t.Errorf("reason = %q, want %q: when nothing was assessed AND at least one read was "+
			"refused, the actionable cause is the refusal", v.reason, ReasonChildrenForbidden)
	}
	if v.unevaluated != 1 || len(v.unreadable) != 1 {
		t.Errorf("unevaluated=%d unreadable=%v, want 1 and 1 — the two must stay distinct, or the "+
			"count stops telling an operator whether the fix is theirs or a blueprint author's",
			v.unevaluated, v.unreadable)
	}
}

// CONTROL. The pure-unevaluated case must keep reporting ZeroEvidence, or the new reason has
// hijacked the one installer's gate already accepts and every zero-evidence composition would stop
// passing it. This is the arm that makes the two reasons a distinction rather than a rename.
func TestRollup_AllUnevaluatedKeepsZeroEvidenceNotChildrenForbidden(t *testing.T) {
	h := &handler{}
	opaque := namedKrateoCR("opaque", "", "", false)
	mg := &unstructured.Unstructured{Object: map[string]any{}}
	setManaged(mg, krateoRef("opaque"))

	v := h.rollupManagedChildren(context.Background(), fakeDynWith(opaque), mg)

	if v.reason != "ZeroEvidence" {
		t.Errorf("reason = %q, want ZeroEvidence: no read was refused here, so naming RBAC would "+
			"send an operator after a grant that is not missing", v.reason)
	}
	if len(v.unreadable) != 0 {
		t.Errorf("unreadable = %v, want empty", v.unreadable)
	}
}

// THE VERDICT DOES NOT MOVE for a minority, which is Diego's decision and the narrow one — but the
// gap is now REPORTED, which is the whole issue. Before this, these two cases were byte-identical.
func TestRollup_AForbiddenChildBesideAHealthyOneStaysAvailableAndIsCounted(t *testing.T) {
	h := &handler{}
	healthy := namedKrateoCR("good", "True", "Available", true)
	mg := &unstructured.Unstructured{Object: map[string]any{}}
	setManaged(mg, krateoRef("refused"), krateoRef("good"))

	v := h.rollupManagedChildren(context.Background(), fakeDynForbidding([]string{"refused"}, healthy), mg)

	if !v.ready || v.reason != "Available" {
		t.Fatalf("ready=%v reason=%q: one refused child must NOT regress a composition with an "+
			"assessed healthy child — a single missing grant would otherwise take down every "+
			"composition using that kind at once", v.ready, v.reason)
	}
	if len(v.unreadable) != 1 || v.unreadable[0] == "" {
		t.Errorf("unreadable = %v, want the refused child named. A green verdict that drops the "+
			"count is exactly the state before this change: one unreadable child and 121 "+
			"indistinguishable", v.unreadable)
	}
}

// The unreadable set must survive the OTHER verdict arms too. A composition with a failing child
// and a refused one reports Unavailable — correctly — and an operator looking at it still needs to
// know that part of it was never read, because that is a second thing to fix.
func TestRollup_UnreadableIsCarriedOnTheFailingVerdictToo(t *testing.T) {
	h := &handler{}
	bad := namedKrateoCR("bad", "False", "Unavailable", true)
	mg := &unstructured.Unstructured{Object: map[string]any{}}
	setManaged(mg, krateoRef("refused"), krateoRef("bad"))

	v := h.rollupManagedChildren(context.Background(), fakeDynForbidding([]string{"refused"}, bad), mg)

	if v.reason != "Unavailable" {
		t.Fatalf("reason = %q, want Unavailable: a positively observed sick child still decides "+
			"the verdict", v.reason)
	}
	if len(v.unreadable) != 1 {
		t.Errorf("unreadable = %v on the failing arm, want the refused child. The scalars are "+
			"written from this verdict, so an arm that drops it publishes "+
			"unreadableChildrenCount: 0 for a composition that has one", v.unreadable)
	}
}

// NO READING MUST WRITE NO FIELDS, and this test exists because the mutant that removed the guard
// SURVIVED the whole package. `if rollupRan {` -> `if rollupRan || true {` compiled and broke
// nothing, so the longest comment in setStatus was asserting a property no test covered — the
// shape this repo spent the day hunting.
//
// The two ways of not running the rollup (no kubeconfig, or a dynamic client that fails to build)
// both leave the verdict looking like a rollup over zero children. Publishing
// `managedChildrenCount: 0` from that is a measurement reported as a property, which is the exact
// defect class behind #248, #261, #269 and #273. A consumer cannot tell "this composition has no
// children" from "nobody asked", and the first is actionable while the second is a lie.
func TestSetStatus_ScalarsAreAbsentWhenTheRollupDidNotRun(t *testing.T) {
	// kubeconfig nil, so the rollup is skipped entirely — the shape every unit test in this
	// package runs in, and the shape a handler built before its client is wired would run in.
	h := &handler{}
	mg := compositionWithStatus(map[string]any{})

	if err := h.setStatus(context.Background(), mg, &statusManagerOpts{
		force: true, recordManaged: true, resources: nil,
		message: "Composition values updated", conditionType: ConditionTypeAvailable,
	}); err != nil {
		t.Fatalf("setStatus: %v", err)
	}

	for _, field := range []string{"managedChildrenCount", "healthEvaluatedChildren", "unreadableChildrenCount"} {
		if _, found, err := unstructured.NestedInt64(mg.Object, "status", field); err != nil {
			t.Errorf("reading status.%s: %v", field, err)
		} else if found {
			t.Errorf("status.%s was written although the rollup never ran. A zero here is a fact "+
				"about the instrument published as a fact about the composition, and a consumer "+
				"reading healthEvaluatedChildren == managedChildrenCount would conclude FULL "+
				"COVERAGE from 0 == 0", field)
		}
	}
}

// And the positive arm, because "absent when it did not run" is satisfied by never writing them at
// all. Asserted as the EQUALITY that signals full coverage, which is the thing a consumer actually
// evaluates, rather than as three independent numbers.
func TestSetStatus_ScalarsArePresentAndConsistentWhenTheRollupRan(t *testing.T) {
	healthy := namedKrateoCR("good", "True", "Available", true)
	mg := compositionWithStatus(map[string]any{})
	setManaged(mg, krateoRef("refused"), krateoRef("good"))

	// The rollup is reached through setStatus only when a dynamic client can be built, which needs
	// a kubeconfig. So this asserts coverageScalars — the function setStatus actually writes from —
	// rather than recomputing the arithmetic here. THAT MATTERS: the first version of this test
	// recomputed it, and a mutant that dropped `- len(v.unreadable)` from the real arithmetic
	// SURVIVED, because a test that reconstructs the thing under test can only agree with itself.
	h := &handler{}
	v := h.rollupManagedChildren(context.Background(), fakeDynForbidding([]string{"refused"}, healthy), mg)

	got := coverageScalars(v)
	total, unreadable, evaluated := got["managedChildrenCount"], got["unreadableChildrenCount"], got["healthEvaluatedChildren"]

	if total != 2 || unreadable != 1 || evaluated != 1 {
		t.Fatalf("managedChildrenCount=%d unreadableChildrenCount=%d healthEvaluatedChildren=%d; "+
			"want 2, 1, 1", total, unreadable, evaluated)
	}
	if evaluated == total {
		t.Error("healthEvaluatedChildren == managedChildrenCount with one child refused: that " +
			"equality is what a consumer reads as FULL COVERAGE, so it must not hold here")
	}
	if evaluated+v.unevaluated+unreadable != total {
		t.Errorf("the three buckets do not sum to the denominator (%d + %d + %d != %d); a child "+
			"in no bucket or in two makes every ratio built on these wrong",
			evaluated, v.unevaluated, unreadable, total)
	}
}

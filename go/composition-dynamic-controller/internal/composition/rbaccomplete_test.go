package composition

import (
	"fmt"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/krateo-platformops/composition-dynamic-controller/internal/tools/rbac"
)

// #357. Every outcome of applyDerivedRBAC reaches rbacCompleteCondition, so the condition is rewritten
// on every pass. Each arm is asserted, including True: ChartRenderable latched because no caller could
// ever pass True (#331).
func TestRBACCompleteCondition_EveryOutcome(t *testing.T) {
	skipped := rbac.ApplyReport{SkippedTerminating: []rbac.SkippedObject{
		{Kind: "Role", Namespace: "demo-system", Name: "portal-krateo-rbac"},
	}}
	for _, tc := range []struct {
		name       string
		attempted  bool
		report     rbac.ApplyReport
		err        error
		wantStatus metav1.ConditionStatus
		wantReason string
		wantInMsg  string
	}{
		{"applied", true, rbac.ApplyReport{}, nil, metav1.ConditionTrue, reasonRBACApplied, "applied"},
		{"skipped a terminating namespace", true, skipped, nil, metav1.ConditionFalse, reasonRBACNamespaceTerminating, "demo-system (Role/portal-krateo-rbac)"},
		{"apply failed", true, rbac.ApplyReport{}, fmt.Errorf("failed to Apply Role: boom"), metav1.ConditionFalse, reasonRBACApplyFailed, "boom"},
		// A failure wins over a skip reported beside it: the pass did not complete.
		{"apply failed after a skip", true, skipped, fmt.Errorf("failed to Apply Role: boom"), metav1.ConditionFalse, reasonRBACApplyFailed, "boom"},
		{"not attempted (chart not renderable)", false, rbac.ApplyReport{}, nil, metav1.ConditionUnknown, reasonRBACNotEvaluated, "ChartRenderable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := rbacCompleteCondition(tc.attempted, tc.report, tc.err)
			if c.Type != ConditionTypeRBACComplete || c.Status != tc.wantStatus || c.Reason != tc.wantReason {
				t.Fatalf("got %s=%s/%s, want %s=%s/%s", c.Type, c.Status, c.Reason, ConditionTypeRBACComplete, tc.wantStatus, tc.wantReason)
			}
			if !strings.Contains(c.Message, tc.wantInMsg) {
				t.Errorf("message %q does not name %q", c.Message, tc.wantInMsg)
			}
			if c.LastTransitionTime.IsZero() {
				t.Error("a condition without lastTransitionTime is rejected by metav1.Condition validation")
			}
		})
	}
}

// An unchanged skip must produce an unchanged message whatever order the objects arrive in
// (rbac.Namespaced is a map), or every pass would rewrite status for nothing.
func TestDescribeSkipped_IsOrderIndependentAndNamesNamespacesFirst(t *testing.T) {
	a := []rbac.SkippedObject{
		{Kind: "RoleBinding", Namespace: "zeta", Name: "x"},
		{Kind: "Role", Namespace: "demo-system", Name: "p"},
		{Kind: "RoleBinding", Namespace: "demo-system", Name: "p"},
	}
	b := []rbac.SkippedObject{a[2], a[0], a[1]}
	if describeSkipped(a) != describeSkipped(b) {
		t.Fatalf("message depends on input order:\n%s\n%s", describeSkipped(a), describeSkipped(b))
	}
	msg := describeSkipped(a)
	if !strings.Contains(msg, "demo-system (Role/p, RoleBinding/p); zeta (RoleBinding/x)") {
		t.Errorf("namespaces must be sorted and each list its objects: %q", msg)
	}
}

package composition

import (
	"context"
	"fmt"
	"github.com/krateo-platformops/composition-dynamic-controller/internal/conditionreason"
	"sort"
	"strings"

	xcontext "github.com/krateo-platformops/unstructured-runtime/pkg/context"
	unstructuredtools "github.com/krateo-platformops/unstructured-runtime/pkg/tools/unstructured"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/krateo-platformops/composition-dynamic-controller/internal/tools/rbac"
)

// ConditionTypeRBACComplete reports whether every RBAC object derived from the chart was applied on
// the last reconcile (#357).
//
// It exists because RBAC objects into a TERMINATING namespace are now skipped rather than failing the
// reconcile (rbac.ApplyRBAC). Skipping silently would hide that the chart still targets a namespace
// that is going away; failing would wedge the composition and its parents. This condition is how the
// skip is said out loud.
//
// It is informational and deliberately not Ready. The health rollup reads only a child's Ready
// condition (childhealth.go), so a False here cannot cascade to parents, which was the defect.
const ConditionTypeRBACComplete = conditionreason.TypeRBACComplete

const (
	reasonRBACApplied              = conditionreason.ReasonRBACApplied
	reasonRBACNamespaceTerminating = conditionreason.ReasonRBACNamespaceTerminating
	reasonRBACApplyFailed          = conditionreason.ReasonRBACApplyFailed
	reasonRBACNotEvaluated         = conditionreason.ReasonRBACNotEvaluated
)

// rbacCompleteCondition computes the condition from what this pass did. It is the ONLY place the
// condition's value is decided, and every outcome reaches it, so the condition is rewritten on every
// pass and can never latch. That property failed for ChartRenderable (#331): its callers could only
// ever pass False.
//
//	attempted=false                 Unknown/NotEvaluated   the chart could not be rendered, so no
//	                                                       RBAC was applied this pass
//	applyErr != nil                 False/ApplyFailed
//	skipped terminating objects     False/NamespaceTerminating, naming them
//	otherwise                       True/Applied
func rbacCompleteCondition(attempted bool, report rbac.ApplyReport, applyErr error) metav1.Condition {
	switch {
	case !attempted:
		return conditionreason.RBACNotEvaluated.WithMessage("RBAC was not applied this reconcile: the chart could not be rendered (see ChartRenderable)")
	case applyErr != nil:
		return conditionreason.RBACApplyFailed.WithMessage(applyErr.Error())
	case len(report.SkippedTerminating) > 0:
		return conditionreason.RBACNamespaceTerminating.WithMessage(describeSkipped(report.SkippedTerminating))
	default:
		return conditionreason.RBACApplied.WithMessage("every RBAC object derived from the chart was applied")
	}
}

// describeSkipped names the namespaces first, because that is what an operator acts on, then the
// objects. Sorted, so an unchanged skip produces an unchanged message and no status churn.
func describeSkipped(skipped []rbac.SkippedObject) string {
	byNS := map[string][]string{}
	for _, s := range skipped {
		byNS[s.Namespace] = append(byNS[s.Namespace], s.Kind+"/"+s.Name)
	}
	nss := make([]string, 0, len(byNS))
	for ns := range byNS {
		nss = append(nss, ns)
	}
	sort.Strings(nss)
	parts := make([]string, 0, len(nss))
	for _, ns := range nss {
		objs := byNS[ns]
		sort.Strings(objs)
		parts = append(parts, fmt.Sprintf("%s (%s)", ns, strings.Join(objs, ", ")))
	}
	return "skipped RBAC into namespaces being deleted, so the grant there is moot; the chart still " +
		"targets them: " + strings.Join(parts, "; ")
}

// setRBACCompleteCondition stamps the condition. Best-effort, like ChartRenderable: the condition is
// a report, and failing to write a report must not turn a reconcile into an error.
func setRBACCompleteCondition(ctx context.Context, mg *unstructured.Unstructured, c metav1.Condition) {
	if err := unstructuredtools.SetConditions(mg, c); err != nil {
		xcontext.Logger(ctx).Info("setting RBACComplete condition", "error", err.Error())
	}
}

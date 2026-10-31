package composition

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func appsObj(kind string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       kind,
		"metadata":   map[string]any{"name": "x", "namespace": "krateo-system"},
	}}
}

// THE CASE THE FIX EXISTS FOR (#256). An apps/v1 kind that workloadReady does not model must be
// existence-is-health, not converging.
//
// ControllerRevision is the real one: apps/v1 contains it and it carries NO status at all. Under
// group-only dispatch it reached workloadReady, which found no desiredNumberScheduled, fell to the
// Deployment-shaped branch, defaulted spec.replicas to 1, read status.readyReplicas as 0, and
// returned childConverging -- FOREVER, taking its parent composition's Ready=False with it.
//
// That is the CronJob wedge (#112) with a different kind, and it recurs because the handler infers
// the kind from which status fields happen to be present. Field-sniffing cannot distinguish "this
// kind has no readiness" from "this kind is not ready yet".
func TestClassifyChild_UnmodelledAppsKindIsHealthyNotConverging(t *testing.T) {
	for _, kind := range []string{"ControllerRevision", "SomeFutureAppsKind"} {
		got := classifyChild("apps", appsObj(kind))
		if got == childConverging {
			t.Errorf("apps/%s: got converging -- an unmodelled kind with no readiness must not "+
				"wedge its composition at Ready=False forever (#256)", kind)
		}
		if got != childHealthy {
			t.Errorf("apps/%s: got %d, want childHealthy -- apps is a native group, so an "+
				"unmodelled kind in it is existence-is-health rather than unevaluated", kind, got)
		}
	}
}

// THE CONTROL, and it is what makes the test above mean anything. The four kinds workloadReady DOES
// model must still be judged by their replica counts -- a fix that routed everything in apps to
// healthy-by-existence would pass the test above while silently calling a crashing Deployment
// healthy, which is #96 reintroduced.
func TestClassifyChild_ModelledAppsKindsStillJudged(t *testing.T) {
	for _, kind := range []string{"Deployment", "StatefulSet", "ReplicaSet"} {
		u := appsObj(kind)
		_ = unstructured.SetNestedField(u.Object, int64(1), "spec", "replicas")
		_ = unstructured.SetNestedField(u.Object, int64(0), "status", "readyReplicas")
		if got := classifyChild("apps", u); got != childConverging {
			t.Errorf("apps/%s 0/1: got %d want converging -- this kind IS modelled and a "+
				"0-ready workload is not healthy (#96)", kind, got)
		}
	}

	// DaemonSet is recognised by its own per-node fields rather than by replicas.
	ds := appsObj("DaemonSet")
	_ = unstructured.SetNestedField(ds.Object, int64(3), "status", "desiredNumberScheduled")
	_ = unstructured.SetNestedField(ds.Object, int64(1), "status", "numberReady")
	if got := classifyChild("apps", ds); got != childConverging {
		t.Errorf("apps/DaemonSet 1/3 ready: got %d want converging", got)
	}
	_ = unstructured.SetNestedField(ds.Object, int64(3), "status", "numberReady")
	if got := classifyChild("apps", ds); got != childHealthy {
		t.Errorf("apps/DaemonSet 3/3 ready: got %d want healthy", got)
	}
}

// batch had the same group-only dispatch. CronJob was already special-cased after #112; the Job arm
// is now explicit too, so a third batch kind would fall through rather than reach jobReady -- where,
// exactly as with the CronJob, neither branch need be reachable against its schema.
func TestClassifyChild_UnmodelledBatchKindIsHealthyNotConverging(t *testing.T) {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "SomeFutureBatchKind",
		"metadata":   map[string]any{"name": "x"},
	}}
	if got := classifyChild("batch", u); got != childHealthy {
		t.Errorf("batch/SomeFutureBatchKind: got %d want childHealthy", got)
	}
}

// The control for batch: Job and CronJob must still reach their own handlers.
func TestClassifyChild_ModelledBatchKindsStillJudged(t *testing.T) {
	job := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": map[string]any{"name": "x"},
	}}
	if got := classifyChild("batch", job); got == childHealthy {
		t.Error("batch/Job with no status must not be healthy-by-existence; it has real readiness")
	}

	// A SUSPENDED CronJob is healthy, not converging (#112) -- proves cronJobReady is still reached.
	cj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "batch/v1", "kind": "CronJob",
		"metadata": map[string]any{"name": "x"},
	}}
	if got := classifyChild("batch", cj); got != childHealthy {
		t.Errorf("batch/CronJob: got %d want healthy -- a schedule has nothing to complete", got)
	}
}

// isWorkloadKind is a list rather than a negation on purpose: a new apps kind must arrive as
// unmodelled-and-healthy (recoverable) instead of silently-converging (wedges a composition). This
// pins that direction, so inverting the predicate fails here rather than in production.
func TestIsWorkloadKind_UnknownKindsAreNotClaimed(t *testing.T) {
	for _, kind := range []string{"ControllerRevision", "", "deployment", "Deployments", "Pod"} {
		if isWorkloadKind(kind) {
			t.Errorf("isWorkloadKind(%q) = true; only the four kinds workloadReady actually "+
				"models may be claimed", kind)
		}
	}
	for _, kind := range []string{"Deployment", "StatefulSet", "DaemonSet", "ReplicaSet"} {
		if !isWorkloadKind(kind) {
			t.Errorf("isWorkloadKind(%q) = false; this kind is modelled and must be judged", kind)
		}
	}
}

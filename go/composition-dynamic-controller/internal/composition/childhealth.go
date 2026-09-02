package composition

import (
	"context"
	"fmt"
	"strings"

	xcontext "github.com/krateo-platformops/unstructured-runtime/pkg/context"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// childState is a managed child's rolled-up health.
type childState int

const (
	childHealthy childState = iota
	childConverging
	childFailed
)

// healthVerdict is the composition-level rollup of its managed children.
type healthVerdict struct {
	ready   bool
	reason  string // "Available" | "Creating" | "Unavailable"
	message string
	failing []string
}

// rollupManagedChildren GETs each child listed in status.managed and rolls their health into one
// verdict. It is deliberately FAIL-SAFE: a child it cannot read (Forbidden — the controller SA lacks
// the grant), that does not exist yet (NotFound — just applied / self-heals), or whose kind it does
// not model, is counted HEALTHY. So the rollup can never flip a working composition to Unavailable;
// it can only surface children it can positively observe are sick (see krateo-core-provider#72, #73).
func (h *handler) rollupManagedChildren(ctx context.Context, dyn dynamic.Interface, mg *unstructured.Unstructured) healthVerdict {
	managed, found, _ := unstructured.NestedSlice(mg.Object, "status", "managed")
	if !found || len(managed) == 0 {
		return healthVerdict{ready: true, reason: "Available", message: "Composition is up-to-date"}
	}

	var failing []string
	converging := 0
	for _, m := range managed {
		ref, ok := m.(map[string]any)
		if !ok {
			continue
		}
		switch h.childHealth(ctx, dyn, ref) {
		case childFailed:
			failing = append(failing, childID(ref))
		case childConverging:
			converging++
		}
	}

	switch {
	case len(failing) > 0:
		return healthVerdict{
			ready:   false,
			reason:  "Unavailable",
			message: fmt.Sprintf("managed children not healthy: %s", joinCap(failing, 3)),
			failing: failing,
		}
	case converging > 0:
		return healthVerdict{
			ready:   false,
			reason:  "Creating",
			message: fmt.Sprintf("%d of %d managed children are not ready", converging, len(managed)),
		}
	default:
		return healthVerdict{ready: true, reason: "Available", message: "Composition is up-to-date"}
	}
}

// readyOutcome is the final Ready decision: a reason ("Available" | "Creating" | "Unavailable") and
// the message to stamp.
type readyOutcome struct {
	reason  string
	message string
}

// resolveReady folds a blueprint's projected health (.status.health.ready, when present) with the
// generic managed-child rollup into the final Ready outcome (krateo-core-provider#96):
//
//   - A blueprint that projected ready=false is honored — the author declared it unhealthy.
//   - Otherwise a positively-observed sick managed child (a workload with unready/unavailable
//     replicas, a failed Job, a nested composition reporting Unavailable) keeps the composition
//     Ready=False EVEN WHEN the blueprint projected ready=true. Projected health may VETO but must
//     not override observed workload sickness to Ready: "applied" is not "serving", and a `deps:`
//     edge must wait for the workload to actually be healthy.
//   - When everything observed is healthy, the composition is Available, preferring the author's
//     projected message when they supplied one.
//
// The rollup is itself fail-safe (unreadable / unmodeled children count healthy), so this can only
// surface failures it can positively observe — it never flips a working composition to NotReady.
func resolveReady(projPresent, projReady bool, projMsg, defaultMsg string, v healthVerdict) readyOutcome {
	if projPresent && !projReady {
		msg := projMsg
		if msg == "" {
			msg = defaultMsg
		}
		return readyOutcome{reason: "Unavailable", message: msg}
	}
	switch v.reason {
	case "Unavailable":
		return readyOutcome{reason: "Unavailable", message: v.message}
	case "Creating":
		return readyOutcome{reason: "Creating", message: v.message}
	}
	msg := defaultMsg
	if projPresent && projMsg != "" {
		msg = projMsg
	}
	return readyOutcome{reason: "Available", message: msg}
}

// childHealth GETs one managed child (as the controller SA) and classifies it. Any read it cannot
// complete degrades to healthy — never to a failure — so a missing RBAC grant or a create race
// cannot regress the parent.
func (h *handler) childHealth(ctx context.Context, dyn dynamic.Interface, ref map[string]any) childState {
	apiVersion := stringField(ref, "apiVersion")
	resource := stringField(ref, "resource")
	name := stringField(ref, "name")
	namespace := stringField(ref, "namespace")
	if apiVersion == "" || resource == "" || name == "" {
		return childHealthy
	}
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return childHealthy
	}
	gvr := gv.WithResource(resource)

	var ri dynamic.ResourceInterface = dyn.Resource(gvr)
	if namespace != "" {
		ri = dyn.Resource(gvr).Namespace(namespace)
	}

	obj, err := ri.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return childConverging // just applied / self-heals — never terminal
		}
		// Forbidden (SA lacks read), no REST mapping, transient: degrade to healthy so a missing
		// grant can never regress a working composition.
		xcontext.Logger(ctx).Debug("child health: unreadable, counting healthy", "child", childID(ref), "error", err.Error())
		return childHealthy
	}
	return classifyChild(gv.Group, obj)
}

// classifyChild maps a live child to a health state with conservative, per-class predicates.
// Anything not explicitly modeled is existence-only (present == healthy).
func classifyChild(group string, obj *unstructured.Unstructured) childState {
	switch {
	case strings.HasSuffix(group, "krateo.io"):
		return krateoReady(obj) // Krateo CRs (incl. child Compositions/CompositionDefinitions)
	case group == "apps":
		return workloadReady(obj)
	case group == "batch":
		return jobReady(obj)
	default:
		return childHealthy // ConfigMap/Secret/Service/RBAC/CRD/plain CRs: existence is health
	}
}

// krateoReady reads a Krateo child's own Ready condition. Only a Ready condition that is PRESENT
// and not-True is evidence the child is unhealthy. The absence of a Ready condition is NOT such
// evidence: many *.krateo.io CRs are leaf declarative resources that never carry conditions (e.g.
// authn's serviceaccount.authn.krateo.io ServiceAccount, a seed identity), and they are healthy by
// existence like any other non-composition child. Treating "no Ready condition" as converging
// wedged the parent Ready=False forever — snowplow never went Ready because its managed
// snowplow-seed ServiceAccount CR has no conditions. Composition/CompositionDefinition children
// always stamp a Ready condition (reason Creating/Available/Unavailable) the moment they first
// reconcile, so genuinely-not-ready nested compositions are still caught by the not-True branch.
func krateoReady(obj *unstructured.Unstructured) childState {
	conds, found, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if !found {
		return childHealthy
	}
	for _, c := range conds {
		cm, ok := c.(map[string]any)
		if !ok || stringField(cm, "type") != "Ready" {
			continue
		}
		switch {
		case stringField(cm, "status") == "True":
			return childHealthy
		case stringField(cm, "reason") == "Unavailable":
			return childFailed
		default:
			return childConverging
		}
	}
	// Has conditions but no Ready type: not a Ready-bearing resource; existence is health.
	return childHealthy
}

// workloadReady handles Deployment/StatefulSet/ReplicaSet (spec.replicas) and DaemonSet
// (desiredNumberScheduled). A workload with unready replicas — including a CrashLoopBackOff pod that
// holds readyReplicas at 0 — is CONVERGING, never failed (it self-progresses), so the composition
// stays Ready=False (Creating) until the workload actually serves rather than merely being applied
// (krateo-core-provider#96).
func workloadReady(obj *unstructured.Unstructured) childState {
	// DaemonSet: readiness is per-node (desiredNumberScheduled vs numberReady); numberUnavailable>0
	// means some node's pod is not ready (e.g. crashing).
	if desired, found, _ := unstructured.NestedInt64(obj.Object, "status", "desiredNumberScheduled"); found {
		ready, _, _ := unstructured.NestedInt64(obj.Object, "status", "numberReady")
		unavailable, _, _ := unstructured.NestedInt64(obj.Object, "status", "numberUnavailable")
		return boolState(ready >= desired && unavailable == 0)
	}
	// Deployment/StatefulSet/ReplicaSet: spec.replicas defaults to 1 when unset (Kubernetes' own
	// default), so an omitted replicas must NOT read as desired=0 — which would call a 0-ready
	// crashing workload "healthy". status.unavailableReplicas>0 is the direct "not serving" signal.
	desired := int64(1)
	if d, found, _ := unstructured.NestedInt64(obj.Object, "spec", "replicas"); found {
		desired = d
	}
	ready, _, _ := unstructured.NestedInt64(obj.Object, "status", "readyReplicas")
	unavailable, _, _ := unstructured.NestedInt64(obj.Object, "status", "unavailableReplicas")
	return boolState(ready >= desired && unavailable == 0)
}

// jobReady: succeeded -> healthy; failed beyond backoffLimit -> failed; else converging.
func jobReady(obj *unstructured.Unstructured) childState {
	if succeeded, _, _ := unstructured.NestedInt64(obj.Object, "status", "succeeded"); succeeded > 0 {
		return childHealthy
	}
	failed, _, _ := unstructured.NestedInt64(obj.Object, "status", "failed")
	if backoff, found, _ := unstructured.NestedInt64(obj.Object, "spec", "backoffLimit"); found && failed > backoff {
		return childFailed
	}
	return childConverging
}

func boolState(ok bool) childState {
	if ok {
		return childHealthy
	}
	return childConverging
}

func childID(ref map[string]any) string {
	kind, name, ns := stringField(ref, "resource"), stringField(ref, "name"), stringField(ref, "namespace")
	if ns != "" {
		return fmt.Sprintf("%s/%s/%s", kind, ns, name)
	}
	return fmt.Sprintf("%s/%s", kind, name)
}

func stringField(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func joinCap(items []string, cap int) string {
	if len(items) <= cap {
		return strings.Join(items, ", ")
	}
	return fmt.Sprintf("%s (+%d more)", strings.Join(items[:cap], ", "), len(items)-cap)
}

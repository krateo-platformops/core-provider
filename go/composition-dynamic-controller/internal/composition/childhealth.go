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
	case group == "batch" && obj.GetKind() == "CronJob":
		return cronJobReady(obj)
	case group == "batch":
		return jobReady(obj)
	// core/v1 (group "") — only the two kinds that actually carry readiness. Everything else in the
	// core group (ConfigMap, Secret, Service, ServiceAccount, ...) is config: it has no notion of
	// being unready, so it falls through to existence-is-health below (krateo-core-provider#121).
	case group == "" && obj.GetKind() == "Pod":
		return podReady(obj)
	case group == "" && obj.GetKind() == "PersistentVolumeClaim":
		return pvcReady(obj)
	default:
		return childHealthy // ConfigMap/Secret/Service/RBAC/CRD/plain CRs: existence is health
	}
}

// podReady classifies a bare Pod. Most Pods in a composition belong to a Deployment/StatefulSet and
// are judged through workloadReady instead — this is for charts that render a Pod directly.
//
// Phase is authoritative for the terminal outcomes; Running is not, because a Running Pod may still
// be failing its readiness probe and serving nothing. Note the asymmetry that keeps this safe: an
// ABSENT Ready condition is NOT treated as unhealth. That is the mistake krateoReady made (wedged
// snowplow) and jobReady made for CronJobs (wedged the portal, #112) — a field a resource does not
// carry is evidence of nothing.
func podReady(obj *unstructured.Unstructured) childState {
	phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
	switch phase {
	case "Succeeded":
		return childHealthy // ran to completion; a finished Pod is not an unhealthy one
	case "Failed":
		return childFailed // terminal: it will not recover on its own
	case "Running":
		if ready, found := podReadyCondition(obj); found {
			return boolState(ready)
		}
		return childHealthy // Running with no Ready condition: do not invent unhealth
	case "":
		return childHealthy // status not written yet, or unreadable — fail-safe, as everywhere here
	default:
		return childConverging // Pending, Unknown: not yet, but it can still get there
	}
}

// podReadyCondition returns (isTrue, found) for the Pod's Ready condition.
func podReadyCondition(obj *unstructured.Unstructured) (bool, bool) {
	conds, found, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if !found {
		return false, false
	}
	for _, c := range conds {
		cm, ok := c.(map[string]any)
		if !ok || stringField(cm, "type") != "Ready" {
			continue
		}
		return stringField(cm, "status") == "True", true
	}
	return false, false
}

// pvcReady classifies a PersistentVolumeClaim by its bind phase.
//
// Pending is CONVERGING rather than failed on purpose: with the common WaitForFirstConsumer binding
// mode a claim stays Pending until a consumer Pod is scheduled, which is normal and self-resolving.
// A claim that genuinely never binds does hold the parent at Creating — correct, because a workload
// with no storage is not ready. Lost is terminal: the bound volume is gone and nothing in-cluster
// brings it back.
func pvcReady(obj *unstructured.Unstructured) childState {
	phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
	switch phase {
	case "Bound":
		return childHealthy
	case "Lost":
		return childFailed
	case "":
		return childHealthy // not yet written / unreadable — fail-safe
	default:
		return childConverging // Pending
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

// cronJobReady: a CronJob is a SCHEDULE, not a unit of work, so it has nothing to complete and
// "converging" never ends. It is healthy once it exists and is not suspended.
//
// It must not be judged by jobReady. A CronJob is batch/v1, so dispatching on group alone sent it
// there, where BOTH branches are unreachable against the CronJob schema: CronJobStatus carries only
// {active, lastScheduleTime, lastSuccessfulTime} so status.succeeded never appears and the healthy
// branch cannot be taken, and backoffLimit lives at spec.jobTemplate.spec.backoffLimit rather than
// spec.backoffLimit so the failed branch cannot be taken either. Every CronJob therefore returned
// childConverging forever, and its parent composition sat Ready=False forever with it.
//
// Observed: krateo-057's Frontend composition went Ready=False "1 of 13 managed children are not
// ready" the moment frontend 1.6.18 added a preview-sandbox janitor CronJob, and stayed there
// across two successful firings — lastScheduleTime and lastSuccessfulTime are fields jobReady never
// reads. The blast radius was not cosmetic: the installer gates rendering on inst.depsReady, so a
// permanently-not-Ready Frontend stopped the Portal composition from ever being re-created after a
// pin flip removed it, which froze the portal Helm release, which froze every chart that release
// registers. One unreachable predicate, four layers of consequence.
//
// This is the same shape as the krateoReady note below — a predicate treating "the field I look for
// is absent" as evidence of unhealth, when it is only evidence that the field does not exist on
// that kind. Suspension is the one genuine not-ready state a CronJob has, so it is the only thing
// worth testing.
// A SUSPENDED CronJob is healthy, not converging. childConverging means "not ready YET, keep
// waiting" — and a suspended CronJob never stops being suspended on its own, so returning it would
// pin the parent composition Ready=False forever. That is the same permanent-wait trap this function
// exists to fix, just narrower: a chart that ships `suspend: true` (a janitor you enable later is a
// normal pattern) would wedge its composition exactly the way the unreachable jobReady branches did.
//
// Suspension is also not unhealth: the object matches its declared spec, which is what Ready means
// for a child. "Nobody asked it to run" is a statement about intent, not about failure. If a
// suspended schedule ever needs surfacing it belongs in a status projection, not in a health
// predicate that blocks a dependency gate.
// The parameter is deliberately unused: existence IS the verdict. It is kept for signature parity
// with the other predicates so the classifyChild dispatch stays uniform.
func cronJobReady(_ *unstructured.Unstructured) childState {
	return childHealthy
}

// jobReady: succeeded -> healthy; failed beyond backoffLimit -> failed; else converging.
// Jobs ONLY — see cronJobReady for why a CronJob must never reach this.
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

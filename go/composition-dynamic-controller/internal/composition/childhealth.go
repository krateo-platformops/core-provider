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
// (desiredNumberScheduled). Not-yet-met rollouts are converging, never failed — they self-progress.
func workloadReady(obj *unstructured.Unstructured) childState {
	if desired, found, _ := unstructured.NestedInt64(obj.Object, "status", "desiredNumberScheduled"); found {
		ready, _, _ := unstructured.NestedInt64(obj.Object, "status", "numberReady")
		return boolState(ready >= desired)
	}
	desired, _, _ := unstructured.NestedInt64(obj.Object, "spec", "replicas")
	ready, _, _ := unstructured.NestedInt64(obj.Object, "status", "readyReplicas")
	return boolState(ready >= desired)
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

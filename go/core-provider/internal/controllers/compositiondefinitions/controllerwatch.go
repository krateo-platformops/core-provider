package compositiondefinitions

import (
	"context"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlevent "sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// A definition's Ready is Available only once its composition controller has rolled out
// (controllerReadiness, Diego 2026-11-16). A rollout finishes, gives up or loses availability on the
// Deployment's own schedule, so the definition is woken by the Deployment's events rather than left
// to its poll interval (3m by default): a gate waiting on "apiVersion == X and Ready=True" sees the
// transition when it happens.

// The labels every composition controller Deployment carries (helm/core-provider/assets/cdc/
// deployment.yaml): instance is the generated resource, version its API version, each truncated to 63.
const (
	labelComponent = "app.kubernetes.io/component"
	labelManagedBy = "app.kubernetes.io/managed-by"
	labelInstance  = "app.kubernetes.io/instance"
	labelVersion   = "app.kubernetes.io/version"
)

func isCompositionControllerDeployment(obj client.Object) bool {
	l := obj.GetLabels()
	return l[labelComponent] == "controller" && l[labelManagedBy] == "krateo" && l[labelInstance] != "" && l[labelVersion] != ""
}

func trunc63(s string) string {
	if len(s) > 63 {
		return s[:63]
	}
	return s
}

// definitionsOfController maps a controller Deployment to the definitions it runs: those whose status
// names its resource and version and whose controller lives in its namespace, followers included.
func definitionsOfController(dep client.Object, cds []compositiondefinitionsv1alpha1.CompositionDefinition) []reconcile.Request {
	if !isCompositionControllerDeployment(dep) {
		return nil
	}
	l := dep.GetLabels()
	var reqs []reconcile.Request
	for i := range cds {
		cd := &cds[i]
		gvk := cd.Status.CurrentGVK()
		if gvk.Kind == "" || trunc63(cd.Status.Resource) != l[labelInstance] || trunc63(gvk.Version) != l[labelVersion] {
			continue
		}
		if controllerNamespace(cd) != dep.GetNamespace() {
			continue
		}
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(cd)})
	}
	return reqs
}

func enqueueForControllerDeployment(cli client.Client) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		if !isCompositionControllerDeployment(obj) {
			return nil
		}
		var list compositiondefinitionsv1alpha1.CompositionDefinitionList
		if err := cli.List(ctx, &list); err != nil {
			return nil // the definition's own poll still observes it
		}
		return definitionsOfController(obj, list.Items)
	}
}

// controllerDeploymentPredicate passes a composition controller Deployment's creation, deletion, and
// any update that can change controllerReadiness's verdict: the spec generation, the replica counts,
// or the Available and Progressing conditions. Other updates (annotations, unrelated status) do not
// wake the definition.
func controllerDeploymentPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(ev ctrlevent.CreateEvent) bool { return isCompositionControllerDeployment(ev.Object) },
		DeleteFunc:  func(ev ctrlevent.DeleteEvent) bool { return isCompositionControllerDeployment(ev.Object) },
		GenericFunc: func(ctrlevent.GenericEvent) bool { return false },
		UpdateFunc: func(ev ctrlevent.UpdateEvent) bool {
			o, ok1 := ev.ObjectOld.(*appsv1.Deployment)
			n, ok2 := ev.ObjectNew.(*appsv1.Deployment)
			if !ok1 || !ok2 || !isCompositionControllerDeployment(n) {
				return false
			}
			return rolloutSignature(o) != rolloutSignature(n)
		},
	}
}

type rolloutSig struct {
	generation, observed                         int64
	replicas, updated, available, statusReplicas int32
	availableCond, progressingCond               string
}

func rolloutSignature(d *appsv1.Deployment) rolloutSig {
	s := rolloutSig{
		generation: d.Generation, observed: d.Status.ObservedGeneration,
		updated: d.Status.UpdatedReplicas, available: d.Status.AvailableReplicas, statusReplicas: d.Status.Replicas,
		replicas: -1,
	}
	if d.Spec.Replicas != nil {
		s.replicas = *d.Spec.Replicas
	}
	for _, c := range d.Status.Conditions {
		switch c.Type {
		case appsv1.DeploymentAvailable:
			s.availableCond = string(c.Status) + "/" + c.Reason
		case appsv1.DeploymentProgressing:
			s.progressingCond = string(c.Status) + "/" + c.Reason
		}
	}
	return s
}

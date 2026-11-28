package compositiondefinitions

import (
	"context"
	"fmt"
	"sync"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/controllers/compositiondefinitions/helpers/getters"
	"github.com/krateo-platformops/core-provider/internal/tools/clusterkube"
	"github.com/krateo-platformops/core-provider/internal/tools/deploy"
	"github.com/krateo-platformops/core-provider/internal/tools/restactionrbac"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

// apiRefRolesReconciler keeps, for each local definition that declares an apiRef, one Role and
// RoleBinding per namespace its compositions' reads land in (core-provider#691).
//
// A SEPARATE CONTROLLER, as the scale-to-zero reconciler is, and for the same reason: Observe is a
// read. It owns the Roles and RoleBindings (name and label: deploy.ApiRefRBACName,
// deploy.ApiRefRBACLabel) and nothing else. The ClusterRole stays with Deploy, which takes the same
// plan from the same planner, and the condition and status.apiRefRBAC stay with the definition's
// reconciler, which reports what it planned.
//
// WHAT TRIGGERS IT, none of it a poll: the definition's spec changes (apiRef, its extras), the creation
// and deletion of an instance of the Kind (the composition watch, one metadata-only watch per Kind and
// version, as the scale-to-zero reconciler's wake watch), and a kick from the definition's own pass
// when what it just planned differs from what was last applied, which is how a RESTAction changing, a
// thing nothing watches, reaches it.
type apiRefRolesReconciler struct {
	kube    client.Client
	dyn     dynamic.Interface
	cache   cache.Cache
	log     logging.Logger
	planner *apiRefPlanner

	ctrl    controller.Controller
	watched sync.Map // schema.GroupVersionKind -> struct{}
}

func (r *apiRefRolesReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	var cd compositiondefinitionsv1alpha1.CompositionDefinition
	if err := r.kube.Get(ctx, req.NamespacedName, &cd); err != nil {
		// Gone: its Undeploy removed the Roles by label.
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if cd.GetDeletionTimestamp() != nil || clusterkube.IsRemote(cd.Spec.Deploy) {
		return reconcile.Result{}, nil
	}
	gvr, ok := statusGVR(&cd)
	if !ok {
		// Not deployed yet: the definition's reconciler records the GVR when it deploys, and a status
		// write does not bump generation, so nothing else wakes this.
		return reconcile.Result{RequeueAfter: notDeployedRequeue}, nil
	}
	gvk := cd.Status.CurrentGVK()
	siblings, err := getters.GetCompositionDefinitionsWithVersion(ctx, r.kube, gvk)
	if err != nil {
		return reconcile.Result{}, err
	}
	if owner := electControllerOwner(&cd, siblings); owner != nil {
		// A follower: the owner writes the Roles of the shared version.
		return reconcile.Result{}, nil
	}
	name := deploy.ApiRefRBACName(gvr)
	if apiRefName(&cd) == "" {
		for i := range siblings {
			if apiRefName(&siblings[i]) != "" && siblings[i].GetDeletionTimestamp() == nil {
				return reconcile.Result{}, nil // a sibling still declares one
			}
		}
		// The apiRef is gone: so are its Roles.
		return reconcile.Result{}, deploy.DeleteApiRefRoles(ctx, r.dyn, name)
	}
	if err := r.ensureCompositionWatch(gvk); err != nil {
		return reconcile.Result{}, err
	}
	key := client.ObjectKeyFromObject(&cd)
	group, found, err := deploy.ApiRefRBACGroup(ctx, r.dyn, gvr)
	if err != nil {
		return reconcile.Result{}, err
	}
	if !found {
		// Deploy has not written the binding the Roles bind the same group as.
		return reconcile.Result{RequeueAfter: notDeployedRequeue}, nil
	}

	// A plan that cannot be determined changes nothing: no Role is written from half a plan, and none
	// is deleted because the answer is missing. The error requeues it with backoff; the condition is
	// the definition's reconciler's to record.
	plan, err := r.planner.plan(ctx, &cd, gvr)
	if err != nil {
		r.log.Info("apiRef Roles left unchanged: the compositions' reads could not be resolved",
			"definition", key.String(), "error", err.Error())
		return reconcile.Result{}, err
	}
	composed := restactionrbac.Compose(nil, plan.Compositions, group, name)
	if err := deploy.ApplyApiRefRoles(ctx, r.dyn, gvr, key, composed); err != nil {
		return reconcile.Result{}, err
	}
	r.planner.markApplied(key, plan.digest)
	r.log.Debug("apiRef Roles applied", "definition", key.String(), "namespaces", composed.Namespaces(), "compositions", len(plan.Compositions))
	return reconcile.Result{}, nil
}

// ensureCompositionWatch starts, once per Kind and version, a metadata-only watch on its instances
// that enqueues every definition of that Kind and version when one is created or deleted. An instance's
// name, namespace and UID never change, so an update changes no extras and wakes nothing.
func (r *apiRefRolesReconciler) ensureCompositionWatch(gvk schema.GroupVersionKind) error {
	if _, loaded := r.watched.LoadOrStore(gvk, struct{}{}); loaded {
		return nil
	}
	obj := &metav1.PartialObjectMetadata{}
	obj.SetGroupVersionKind(gvk)
	src := source.Kind[client.Object](r.cache, obj,
		handler.EnqueueRequestsFromMapFunc(r.definitionsOf(gvk)),
		predicate.Funcs{
			CreateFunc:  func(event.CreateEvent) bool { return true },
			DeleteFunc:  func(event.DeleteEvent) bool { return true },
			UpdateFunc:  func(event.UpdateEvent) bool { return false },
			GenericFunc: func(event.GenericEvent) bool { return false },
		})
	if err := r.ctrl.Watch(src); err != nil {
		r.watched.Delete(gvk) // let the next pass try again
		return fmt.Errorf("starting the apiRef Roles composition watch on %s: %w", gvk.String(), err)
	}
	return nil
}

func (r *apiRefRolesReconciler) definitionsOf(gvk schema.GroupVersionKind) handler.MapFunc {
	return func(ctx context.Context, _ client.Object) []reconcile.Request {
		cds, err := getters.GetCompositionDefinitionsWithVersion(ctx, r.kube, gvk)
		if err != nil {
			r.log.Info("apiRef Roles composition watch could not list definitions; the next definition event retries",
				"gvk", gvk.String(), "error", err.Error())
			return nil
		}
		out := make([]reconcile.Request, 0, len(cds))
		for i := range cds {
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: cds[i].Namespace, Name: cds[i].Name}})
		}
		return out
	}
}

var (
	sharedPlannerOnce sync.Once
	sharedPlanner     *apiRefPlanner
	sharedKicks       = make(chan event.GenericEvent, 256)
)

// plannerFor is the process-wide planner, shared by the definition's reconciler and the Roles
// reconciler so one composition's resolution is made once.
func plannerFor(dyn dynamic.Interface) *apiRefPlanner {
	sharedPlannerOnce.Do(func() {
		sharedPlanner = newAPIRefPlanner(dyn, inspector)
		sharedPlanner.kick = kickRoles
	})
	return sharedPlanner
}

// kickRoles asks the Roles reconciler to look at def. Dropping a kick when the channel is full is safe:
// the definition's next pass kicks again while the plan still differs from what was applied.
func kickRoles(def types.NamespacedName) {
	cd := &compositiondefinitionsv1alpha1.CompositionDefinition{ObjectMeta: metav1.ObjectMeta{Namespace: def.Namespace, Name: def.Name}}
	select {
	case sharedKicks <- event.GenericEvent{Object: cd}:
	default:
	}
}

// SetupApiRefRoles registers the apiRef Roles reconciler (#691).
func SetupApiRefRoles(mgr ctrl.Manager, dyn dynamic.Interface, log logging.Logger) error {
	return setupApiRefRoles(mgr, dyn, plannerFor(dyn), sharedKicks, log)
}

func setupApiRefRoles(mgr ctrl.Manager, dyn dynamic.Interface, planner *apiRefPlanner, kicks <-chan event.GenericEvent, log logging.Logger) error {
	r := &apiRefRolesReconciler{
		kube:    mgr.GetClient(),
		dyn:     dyn,
		cache:   mgr.GetCache(),
		log:     log.WithValues("controller", "apirefroles"),
		planner: planner,
	}
	c, err := ctrl.NewControllerManagedBy(mgr).
		Named("compositiondefinition-apirefroles").
		For(&compositiondefinitionsv1alpha1.CompositionDefinition{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		WatchesRawSource(source.Channel(kicks, &handler.EnqueueRequestForObject{})).
		Build(r)
	if err != nil {
		return err
	}
	r.ctrl = c
	return nil
}

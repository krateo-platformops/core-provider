package compositiondefinitions

import (
	"context"
	"fmt"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/controllers/compositiondefinitions/helpers/getters"
	"github.com/krateo-platformops/core-provider/internal/controllers/compositiondefinitions/helpers/status"
	"github.com/krateo-platformops/core-provider/internal/tools/clusterkube"
	contexttools "github.com/krateo-platformops/core-provider/internal/tools/context"
	"github.com/krateo-platformops/core-provider/internal/tools/deploy"
	rtv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	"github.com/krateo-platformops/provider-runtime/pkg/reconciler"
	appsv1 "k8s.io/api/apps/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery/cached/memory"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlevent "sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// One composition-dynamic-controller per generated Kind+version, however many definitions point
// at it.
//
// The controller is unscoped: it is started with -group/-version/-resource only and watches every
// instance carrying that version's label, cluster-wide. Its cluster-scoped RBAC
// ({resource}-{apiVersion} ClusterRole/ClusterRoleBinding, and the apiRef RBAC) carries no
// namespace in its name. So when two LOCAL definitions in different namespaces target the same
// Kind+version — which is what moving a live definition to another namespace looks like halfway
// through — each used to deploy its own controller, both reconciled the same instances, the
// shared ClusterRoleBinding named whichever ServiceAccount deployed last (so each definition's
// Lookup digest disagreed with the other's and they redeployed in turn every poll), and deleting
// either one deleted the shared RBAC out from under the other.
//
// The rule now: the OLDEST live local definition on a Kind+version owns the controller (ties
// broken by namespace, then name, so every reconcile of every definition elects the same one).
// The others are followers: they deploy nothing, retire any controller they deployed before this
// rule existed, and keep their status current so the delete path's reference counting sees them.
// They still migrate their OWN instances between versions; only the CRD and the controller are
// the owner's.
// A definition being deleted never owns, so the next-oldest takes over the moment deletion starts.
//
// creationTimestamp is the sort key on purpose: it is immutable. generation/resourceVersion are
// not ownership signals (core-provider's own migrations are metadata writes and do not move
// generation).
//
// Remote definitions are out of this: a remote target gets a controller dedicated to that
// definition on the spoke, so they neither own nor follow.

// olderDefinition reports whether a sorts before b in the ownership order.
func olderDefinition(a, b *compositiondefinitionsv1alpha1.CompositionDefinition) bool {
	ta, tb := a.GetCreationTimestamp(), b.GetCreationTimestamp()
	if !ta.Equal(&tb) {
		return ta.Before(&tb)
	}
	if a.GetNamespace() != b.GetNamespace() {
		return a.GetNamespace() < b.GetNamespace()
	}
	return a.GetName() < b.GetName()
}

// oldestLiveLocalSibling returns the oldest definition in cds that is not self, not being deleted
// and not remote, or nil when there is none.
func oldestLiveLocalSibling(self *compositiondefinitionsv1alpha1.CompositionDefinition, cds []compositiondefinitionsv1alpha1.CompositionDefinition) *compositiondefinitionsv1alpha1.CompositionDefinition {
	var oldest *compositiondefinitionsv1alpha1.CompositionDefinition
	for i := range cds {
		cd := &cds[i]
		if cd.GetName() == self.GetName() && cd.GetNamespace() == self.GetNamespace() {
			continue
		}
		if cd.GetDeletionTimestamp() != nil || clusterkube.IsRemote(cd.Spec.Deploy) {
			continue
		}
		if oldest == nil || olderDefinition(cd, oldest) {
			oldest = cd
		}
	}
	return oldest
}

// electControllerOwner returns the definition that owns the controller self would otherwise
// deploy, or nil when self owns it. cds are the definitions currently targeting that Kind+version.
func electControllerOwner(self *compositiondefinitionsv1alpha1.CompositionDefinition, cds []compositiondefinitionsv1alpha1.CompositionDefinition) *compositiondefinitionsv1alpha1.CompositionDefinition {
	if clusterkube.IsRemote(self.Spec.Deploy) {
		return nil
	}
	if o := oldestLiveLocalSibling(self, cds); o != nil && olderDefinition(o, self) {
		return o
	}
	return nil
}

// controllerOwner returns the definition that runs the shared controller for gvk when that is not
// cr; nil means cr owns it (or is remote, and has its own).
func (e *external) controllerOwner(ctx context.Context, cr *compositiondefinitionsv1alpha1.CompositionDefinition, gvk schema.GroupVersionKind) (*compositiondefinitionsv1alpha1.CompositionDefinition, error) {
	if clusterkube.IsRemote(cr.Spec.Deploy) {
		return nil, nil
	}
	cds, err := getters.GetCompositionDefinitionsWithVersion(ctx, e.mgmt, gvk)
	if err != nil {
		return nil, fmt.Errorf("error listing CompositionDefinitions on %s: %w", gvk.String(), err)
	}
	return electControllerOwner(cr, cds), nil
}

// controllerSuccessor returns the live local definition that takes over gvk's controller once cr
// stops running it, or nil when no other definition still needs it.
func (e *external) controllerSuccessor(ctx context.Context, cr *compositiondefinitionsv1alpha1.CompositionDefinition, gvk schema.GroupVersionKind) (*compositiondefinitionsv1alpha1.CompositionDefinition, error) {
	cds, err := getters.GetCompositionDefinitionsWithVersion(ctx, e.mgmt, gvk)
	if err != nil {
		return nil, fmt.Errorf("error listing CompositionDefinitions on %s: %w", gvk.String(), err)
	}
	return oldestLiveLocalSibling(cr, cds), nil
}

// controllerDeployedIn reports whether namespace holds a controller Deployment for gvr.
func (e *external) controllerDeployedIn(ctx context.Context, namespace string, gvr schema.GroupVersionResource) (bool, error) {
	var dep appsv1.Deployment
	err := e.kube.Get(ctx, deploy.ControllerDeploymentKey(namespace, gvr), &dep)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("error looking up controller deployment for %s in %s: %w", gvr.String(), namespace, err)
	}
	return true, nil
}

// handOverController removes the controller cr deployed for gvr in its own namespace while
// another definition (successor) keeps the Kind+version alive: only the objects in cr's namespace
// go, the shared cluster-scoped RBAC stays for the successor's controller, and every derived
// binding naming cr's controller ServiceAccount is repointed at the successor's, which has the
// same name in the successor's namespace.
//
// Instances are never touched, and the CRD is never deleted. Two definitions in the SAME namespace
// share every controller object by name, so there is nothing of cr's own to remove and nothing to
// rebind: the successor's next deploy rewrites them.
func (e *external) handOverController(ctx context.Context, cr *compositiondefinitionsv1alpha1.CompositionDefinition, chartInfo *compositiondefinitionsv1alpha1.ChartInfo, gvr schema.GroupVersionResource, successor *compositiondefinitionsv1alpha1.CompositionDefinition, log logging.Logger) error {
	if successor.Namespace == cr.Namespace {
		log.Debug("Composition controller stays: the next owner shares this namespace",
			"gvr", gvr.String(), "to", successor.Namespace+"/"+successor.Name)
		return nil
	}
	if err := deploy.Undeploy(ctx, e.kube, deploy.UndeployOptions{
		DiscoveryClient:        memory.NewMemCacheClient(e.client.Discovery()),
		Spec:                   chartInfo,
		KubeClient:             e.kube,
		GVR:                    gvr,
		Namespace:              cr.Namespace,
		SkipCRD:                true,
		KeepShared:             true,
		DynamicClient:          e.dynamic,
		RBACFolderPath:         CDCrbacConfigFolder,
		DeploymentTemplatePath: CDCtemplateDeploymentPath,
		ServiceTemplatePath:    ServiceTemplatePath,
		ConfigmapTemplatePath:  CDCtemplateConfigmapPath,
		JsonSchemaTemplatePath: JSONSchemaTemplateConfigmapPath,
		AuthnNamespace:         AuthnNamespace,
	}); err != nil {
		return fmt.Errorf("error handing the %s controller over to %s/%s: %w", gvr.String(), successor.Namespace, successor.Name, err)
	}

	repaired, err := deploy.ResubjectDerivedRBAC(ctx, deploy.ResubjectOptions{
		DynamicClient: e.dynamic,
		Namespace:     cr.Namespace,
		NewNamespace:  successor.Namespace,
		OldGVR:        gvr,
		NewGVR:        gvr,
	})
	if err != nil {
		return fmt.Errorf("error re-subjecting derived RBAC from %s to %s: %w", cr.Namespace, successor.Namespace, err)
	}
	log.Debug("Handed the composition controller over to another definition",
		"gvr", gvr.String(), "to", successor.Namespace+"/"+successor.Name, "rebound", repaired)
	return nil
}

// handedOver reports whether a definition being deleted has finished handing its Kind+version
// over: a live local successor exists and no controller of this definition's own is left in its
// namespace.
//
// Ordering: it can only become true AFTER Delete's handOverController has removed this
// definition's controller (the first Observe of a deletion still finds it, returns "exists", and
// Delete runs), so it never short-circuits the hand-over it depends on.
//
// Same namespace: true at once, with no Delete, by construction. Every controller object is named
// {resource}-{apiVersion} in the definition's namespace, so two definitions there on one version
// share ONE ServiceAccount, Deployment and ConfigMap, and the shared ClusterRoleBinding's subject is
// that same ServiceAccount whichever of them deployed last. Nothing belongs to the leaving
// definition alone, and nothing it could remove would leave the successor's controller without a
// live identity. The successor redeploys its own configuration on its next reconcile (its status
// digest is empty while it follows, so its Observe reports not up to date).
func (e *external) handedOver(ctx context.Context, cr *compositiondefinitionsv1alpha1.CompositionDefinition, gvk schema.GroupVersionKind, gvr schema.GroupVersionResource) (bool, error) {
	if clusterkube.IsRemote(cr.Spec.Deploy) {
		return false, nil
	}
	successor, err := e.controllerSuccessor(ctx, cr, gvk)
	if err != nil || successor == nil {
		return false, err
	}
	if successor.Namespace == cr.Namespace {
		return true, nil
	}
	own, err := e.controllerDeployedIn(ctx, cr.Namespace, gvr)
	if err != nil {
		return false, err
	}
	return !own, nil
}

func followerCondition(owner *compositiondefinitionsv1alpha1.CompositionDefinition, gvr schema.GroupVersionResource) rtv1.Condition {
	return rtv1.Available().WithMessage(fmt.Sprintf(
		"composition controller for %s is run by CompositionDefinition %s/%s", gvr.String(), owner.Namespace, owner.Name))
}

// observeFollower is Observe for a definition that does not own its Kind+version's controller.
// It is up to date once Update has moved its status onto this version, no controller of its own is
// left in its namespace (one deployed before ownership was elected), and none of ITS instances
// still wait for a version migration: a follower still migrates its own instances, because no other
// definition does that for it, and an unmigrated instance would hold every served-version prune of
// the Kind back for good. The CRD's status shape is the owner's, so it is not compared here.
func (e *external) observeFollower(ctx context.Context, cr *compositiondefinitionsv1alpha1.CompositionDefinition, crd *apiextensionsv1.CustomResourceDefinition, gvr schema.GroupVersionResource, gvk schema.GroupVersionKind, packageURL string, owner *compositiondefinitionsv1alpha1.CompositionDefinition) (reconciler.ExternalObservation, error) {
	log := contexttools.LoggerFromCtx(ctx, logging.NewNopLogger())
	notUpToDate := reconciler.ExternalObservation{ResourceExists: true, ResourceUpToDate: false}

	if cr.Status.CurrentGVK() != gvk {
		log.Debug("Follower status not yet on this version", "status", cr.Status.CurrentGVK().String(), "chart", gvk.String())
		return notUpToDate, nil
	}
	if owner.Namespace != cr.Namespace {
		own, err := e.controllerDeployedIn(ctx, cr.Namespace, gvr)
		if err != nil {
			return reconciler.ExternalObservation{}, err
		}
		if own {
			log.Debug("Follower still runs its own composition controller; handing it over",
				"gvr", gvr.String(), "owner", owner.Namespace+"/"+owner.Name)
			return notUpToDate, nil
		}
	}
	if cr.MigrationApproved(gvr.Version) {
		self := getters.DefinitionRef{Name: cr.Name, Namespace: cr.Namespace}
		for _, vi := range cr.Status.Managed.VersionInfo {
			if vi.Version == gvr.Version {
				continue
			}
			stale, err := getters.GetOwnedCompositionsByVersionLabel(ctx, e.dynamic, gvr, vi.Version, self)
			if err != nil {
				return reconciler.ExternalObservation{}, fmt.Errorf("error checking compositions on version %s: %w", vi.Version, err)
			}
			if len(stale.Items) > 0 {
				log.Debug("Follower's compositions pending version migration", "fromVersion", vi.Version, "toVersion", gvr.Version, "count", len(stale.Items))
				return notUpToDate, nil
			}
		}
	}

	if err := status.RefreshCompositionDefinitionStatus(cr, crd, gvr, gvk, packageURL); err != nil {
		return reconciler.ExternalObservation{}, fmt.Errorf("error refreshing CompositionDefinition status: %w", err)
	}
	cr.Status.Digest = ""
	cr.SetConditions(followerCondition(owner, gvr))
	return reconciler.ExternalObservation{ResourceExists: true, ResourceUpToDate: true}, nil
}

// yieldController is Update's step for a definition that does not own its Kind+version's
// controller: instead of applying the CRD and deploying a controller, it hands over any controller
// it still runs in its own namespace. The owner established the CRD version this definition is on,
// so the pluralizer resolves it.
func (e *external) yieldController(ctx context.Context, cr *compositiondefinitionsv1alpha1.CompositionDefinition, gvk schema.GroupVersionKind, owner *compositiondefinitionsv1alpha1.CompositionDefinition, log logging.Logger) (schema.GroupVersionResource, error) {
	gvr, err := e.pluralizer.GVKtoGVR(gvk)
	if err != nil {
		return gvr, fmt.Errorf("waiting for CompositionDefinition %s/%s to establish %s: %w", owner.Namespace, owner.Name, gvk.String(), err)
	}
	if err := e.handOverController(ctx, cr, cr.Spec.Chart.DeepCopy(), gvr, owner, log); err != nil {
		return gvr, err
	}
	cr.Status.Digest = ""
	log.Debug("Composition controller run by another definition", "gvr", gvr.String(), "owner", owner.Namespace+"/"+owner.Name)
	return gvr, nil
}

// siblingHandoverPredicate passes the CompositionDefinition events after which another definition
// on the same Kind may have to take a controller over: a deletion starting or completing, and a
// definition moving to another version. Everything else is left to the definition's own watch, so
// sibling status writes do not ping-pong.
func siblingHandoverPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(ctrlevent.CreateEvent) bool { return false },
		GenericFunc: func(ctrlevent.GenericEvent) bool { return false },
		DeleteFunc:  func(ctrlevent.DeleteEvent) bool { return true },
		UpdateFunc: func(ev ctrlevent.UpdateEvent) bool {
			oldCD, ok1 := ev.ObjectOld.(*compositiondefinitionsv1alpha1.CompositionDefinition)
			newCD, ok2 := ev.ObjectNew.(*compositiondefinitionsv1alpha1.CompositionDefinition)
			if !ok1 || !ok2 {
				return false
			}
			if oldCD.GetDeletionTimestamp() == nil && newCD.GetDeletionTimestamp() != nil {
				return true
			}
			return oldCD.Status.ApiVersion != newCD.Status.ApiVersion
		},
	}
}

// enqueueSiblingDefinitions maps a CompositionDefinition event to reconcile requests for every
// OTHER definition on the same generated group+kind, so a successor takes the controller over as
// soon as its owner goes, instead of at its next poll.
func enqueueSiblingDefinitions(cli client.Client) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		cd, ok := obj.(*compositiondefinitionsv1alpha1.CompositionDefinition)
		if !ok {
			return nil
		}
		gvk := cd.Status.CurrentGVK()
		if gvk.Kind == "" {
			return nil
		}
		siblings, err := getters.GetCompositionDefinitions(ctx, cli, gvk.GroupKind())
		if err != nil {
			return nil
		}
		var reqs []reconcile.Request
		for i := range siblings {
			s := &siblings[i]
			if s.Name == cd.Name && s.Namespace == cd.Namespace {
				continue
			}
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(s)})
		}
		return reqs
	}
}

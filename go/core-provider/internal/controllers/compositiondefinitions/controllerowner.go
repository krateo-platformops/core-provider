package compositiondefinitions

import (
	"context"
	"fmt"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/controllers/compositiondefinitions/helpers/getters"
	"github.com/krateo-platformops/core-provider/internal/controllers/compositiondefinitions/helpers/status"
	"github.com/krateo-platformops/core-provider/internal/tools/chart/chartfs"
	"github.com/krateo-platformops/core-provider/internal/tools/clusterkube"
	contexttools "github.com/krateo-platformops/core-provider/internal/tools/context"
	crdclient "github.com/krateo-platformops/core-provider/internal/tools/crd"
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

func followerCondition(owner *compositiondefinitionsv1alpha1.CompositionDefinition, gvr schema.GroupVersionResource) rtv1.Condition {
	return rtv1.Available().WithMessage(fmt.Sprintf(
		"composition controller for %s is run by CompositionDefinition %s/%s", gvr.String(), owner.Namespace, owner.Name))
}

// observeFollower is Observe for a definition that does not own its Kind+version's controller.
// It is up to date once no controller of its own is left in its namespace (one deployed before
// ownership was elected is retired by settleFollower); its status is refreshed every time, because
// the delete path counts definitions by the GVK on their status.
func (e *external) observeFollower(ctx context.Context, cr *compositiondefinitionsv1alpha1.CompositionDefinition, crd *apiextensionsv1.CustomResourceDefinition, gvr schema.GroupVersionResource, gvk schema.GroupVersionKind, packageURL string, owner *compositiondefinitionsv1alpha1.CompositionDefinition) (reconciler.ExternalObservation, error) {
	log := contexttools.LoggerFromCtx(ctx, logging.NewNopLogger())
	if owner.Namespace != cr.Namespace {
		own, err := e.controllerDeployedIn(ctx, cr.Namespace, gvr)
		if err != nil {
			return reconciler.ExternalObservation{}, err
		}
		if own {
			log.Debug("Follower still runs its own composition controller; retiring it",
				"gvr", gvr.String(), "owner", owner.Namespace+"/"+owner.Name)
			return reconciler.ExternalObservation{ResourceExists: true, ResourceUpToDate: false}, nil
		}
	}
	if err := status.RefreshCompositionDefinitionStatus(cr, crd, gvr, gvk, packageURL); err != nil {
		return reconciler.ExternalObservation{}, fmt.Errorf("error refreshing CompositionDefinition status: %w", err)
	}
	cr.Status.Digest = ""
	cr.SetConditions(followerCondition(owner, gvr))
	return reconciler.ExternalObservation{ResourceExists: true, ResourceUpToDate: true}, nil
}

// settleFollower is Create/Update for a definition that does not own its Kind+version's
// controller: it hands over any controller it still runs in its own namespace and records the
// status the delete path counts definitions by. It never applies the CRD, deploys a controller or
// migrates instances; the owner does.
func (e *external) settleFollower(ctx context.Context, cr *compositiondefinitionsv1alpha1.CompositionDefinition, gvk schema.GroupVersionKind, owner *compositiondefinitionsv1alpha1.CompositionDefinition) error {
	log := contexttools.LoggerFromCtx(ctx, logging.NewNopLogger())

	gvr, err := e.pluralizer.GVKtoGVR(gvk)
	if err != nil {
		return fmt.Errorf("waiting for CompositionDefinition %s/%s to establish %s: %w", owner.Namespace, owner.Name, gvk.String(), err)
	}
	if err := e.handOverController(ctx, cr, cr.Spec.Chart.DeepCopy(), gvr, owner, log); err != nil {
		return err
	}

	crd, err := crdclient.Get(ctx, e.kube, gvr.GroupResource())
	if err != nil {
		return fmt.Errorf("error getting CRD: %w", err)
	}
	if crd == nil {
		return fmt.Errorf("waiting for CompositionDefinition %s/%s to establish the %s CRD", owner.Namespace, owner.Name, gvr.GroupResource().String())
	}
	pkgFS, err := chartfs.ForSpec(ctx, e.mgmtDynamic, cr.Spec.Chart)
	if err != nil {
		return err
	}
	if err := status.RefreshCompositionDefinitionStatus(cr, crd, gvr, gvk, pkgFS.PackageURL()); err != nil {
		return fmt.Errorf("error refreshing CompositionDefinition status: %w", err)
	}
	cr.Status.Digest = ""
	cr.SetConditions(followerCondition(owner, gvr))
	log.Debug("Follower settled", "gvr", gvr.String(), "owner", owner.Namespace+"/"+owner.Name)
	return nil
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

// Package compositionmirror reflects hub Composition instances of a remote-targeted
// CompositionDefinition onto its spoke, and reads the spoke's status back. It is the hub-side half of
// the "remote composition mirror" model (docs/design/remote-composition-mirror.md): the desired
// Composition is authored on the hub; core-provider mirrors its spec down to the spoke, where the
// projected composition-dynamic-controller renders the release, and mirrors the spoke status up.
//
// v1 is resync-driven. The controller reconciles a remote CompositionDefinition — whose generated
// Kind is created at runtime, so a dynamic per-Kind watch is deferred (design §7) — and, on each
// reconcile and every resync, reconciles the *set* of hub Composition instances of that Kind (in the
// CompositionDefinition's namespace) against their spokes:
//   - down: create-or-update each spoke instance's spec from its hub counterpart (hub wins on drift),
//   - back: copy the spoke instance's status onto the hub instance (best-effort),
//   - gc:   delete spoke instances this reflector created that no longer have a hub counterpart.
//
// An instance goes to the spoke named by its krateo.io/target annotation (design §5.1 fan-out), or to
// the CD's spec.deploy.targetRef when unannotated. Instances are grouped by resolved target and each
// spoke is reconciled with exactly the instances bound to it, so the same Kind can fan out per tenant.
//
// Set-difference GC over a management label (rather than a per-instance finalizer) gives
// cross-cluster teardown without a dynamic watch; the finalizer/watch upgrade is a fast-follow.
package compositionmirror

import (
	"context"
	"fmt"
	"github.com/krateo-platformops/core-provider/internal/tools/auditrec"
	"github.com/krateo-platformops/plumbing/kubeutil/objectclient"
	"reflect"
	"time"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/tools/clusterkube"
	"github.com/krateo-platformops/plumbing/kubeutil/dynamicwatch"
	"github.com/krateo-platformops/provider-runtime/pkg/controller"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	// managedByLabel marks spoke Composition instances this reflector created, so garbage collection
	// only ever deletes mirrors it owns — never instances authored directly on the spoke.
	managedByLabel = "compositionmirror.krateo.io/managed-by"
	managedByValue = "core-provider"

	// cdFinalizer holds a remote CompositionDefinition in Terminating until the reflector has deleted
	// its hub Composition instances, so instances never outlive their type (mirroring how a local CD
	// delete cascades instances via CRD removal). Hub-only cleanup, so it never blocks on the spoke.
	cdFinalizer = "compositionmirror.krateo.io/hub-teardown"

	// targetAnnotation lets a single hub Composition override the CompositionDefinition's default
	// deploy target, so instances of the same Kind can fan out to different spokes per tenant. Its
	// value is the name of a KubernetesTarget in the CompositionDefinition's namespace; absent, the
	// instance follows the CD's spec.deploy.targetRef (backward compatible — the pre-fan-out behavior).
	// It lives in metadata, not spec, so Composition.spec stays pure chart values (design §5.1).
	targetAnnotation = "krateo.io/target"

	// requeueInterval is the steady-state resync: because there is no dynamic watch on the generated
	// hub Kind (v1), drift on a hub Composition is healed at most this long after it happens.
	requeueInterval = 30 * time.Second
	// requeuePending is the shorter backoff used while the CompositionDefinition has not yet recorded
	// its generated GVK (the CRD is still being generated).
	requeuePending = 10 * time.Second
	// reconcileTimeout bounds the spoke-reaching work of a single reconcile so a slow or unreachable
	// target cannot pin a worker indefinitely and starve reflection for other CompositionDefinitions.
	reconcileTimeout = 2 * time.Minute
)

// Options configures the compositionmirror reconciler.
type Options struct {
	ControllerOptions controller.Options
}

// Setup wires the reflector into the manager. It watches CompositionDefinitions statically; each
// generated composition Kind is watched dynamically the first time its CD reconciles (the Kind is
// created at runtime), so a hub Composition edit reflects immediately instead of at the next resync.
func Setup(mgr ctrl.Manager, o Options) error {
	r := &Reconciler{
		hub:        mgr.GetClient(),
		hubDynamic: dynamic.NewForConfigOrDie(mgr.GetConfig()),
		cfgWatch:   dynamicwatch.NewRegistry(mgr.GetCache()),
		log:        o.ControllerOptions.Logger.WithValues("controller", "compositionmirror"),
	}
	c, err := ctrl.NewControllerManagedBy(mgr).
		Named("compositionmirror").
		WithOptions(o.ControllerOptions.ForControllerRuntime()).
		For(&compositiondefinitionsv1alpha1.CompositionDefinition{}).
		Build(r)
	if err != nil {
		return err
	}
	r.ctrl = c
	return nil
}

// Reconciler mirrors the hub Composition instances of a remote CompositionDefinition onto its spokes.
type Reconciler struct {
	hub        client.Client
	hubDynamic dynamic.Interface
	// newSpoke builds a spoke's dynamic client from a target name. It is the injection seam for fan-out:
	// production leaves it nil and spokeResolverFor defaults to clusterkube.Remote, while unit tests set
	// it to return per-target fakes so the engine can be exercised across several spokes without a real
	// cluster.
	newSpoke func(ctx context.Context, mgmt client.Client, ns, target string) (dynamic.Interface, error)
	// cfgWatch and ctrl back the dynamic per-Kind watches. Because composition Kinds are generated at
	// runtime, the reflector registers a watch for each Kind the first time it reconciles that Kind's
	// CompositionDefinition (see ensureWatch); cfgWatch (plumbing/kubeutil/dynamicwatch.Registry) dedupes
	// registration across reconciles of many different CompositionDefinitions.
	cfgWatch *dynamicwatch.Registry
	ctrl     crcontroller.Controller
	log      logging.Logger
}

// Reconcile enumerates and reflects the hub Composition instances of one remote CompositionDefinition.
func (r *Reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	cd := &compositiondefinitionsv1alpha1.CompositionDefinition{}
	if err := r.hub.Get(ctx, req.NamespacedName, cd); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	// Teardown first: on delete, remove the hub Composition instances this CD's type owns, then release
	// our finalizer. Handled before the remote check so the finalizer can never get stuck (even if the
	// CD stopped being remote). It is hub-only, so it never blocks on an unreachable spoke.
	if cd.DeletionTimestamp != nil {
		return r.reconcileDelete(ctx, cd)
	}

	// Only remote CompositionDefinitions have a hub-desired / spoke-realized split. A local CD's
	// Composition instances are rendered in place by the local cdc — there is nothing to mirror. Drop
	// a stale finalizer if this CD is (no longer) remote.
	if !clusterkube.IsRemote(cd.Spec.Deploy) {
		if controllerutil.RemoveFinalizer(cd, cdFinalizer) {
			if err := r.hub.Update(ctx, cd); err != nil {
				return reconcile.Result{}, err
			}
		}
		return reconcile.Result{}, nil
	}

	// Hold the CD until the reflector has torn its hub Compositions down on delete (reconcileDelete).
	if controllerutil.AddFinalizer(cd, cdFinalizer) {
		if err := r.hub.Update(ctx, cd); err != nil {
			return reconcile.Result{}, err
		}
	}

	// The generated GVK is recorded on the CD status once its CRD exists (on both hub and spoke, see
	// increment 1). Until then there is nothing to author against — retry shortly.
	if cd.Status.ApiVersion == "" || cd.Status.Kind == "" || cd.Status.Resource == "" {
		return reconcile.Result{RequeueAfter: requeuePending}, nil
	}
	gv, err := schema.ParseGroupVersion(cd.Status.ApiVersion)
	if err != nil {
		r.log.Debug("compositionmirror: invalid generated apiVersion", "cd", req.String(), "apiVersion", cd.Status.ApiVersion, "error", err)
		return reconcile.Result{RequeueAfter: requeueInterval}, nil
	}
	gvr := gv.WithResource(cd.Status.Resource)

	// React to hub Composition changes immediately by watching this generated Kind. Best-effort: if
	// the Kind is not discoverable yet (its CRD was just created), the periodic resync still drives
	// reflection and the next reconcile retries the registration.
	r.ensureWatch(gv.WithKind(cd.Status.Kind))

	// Bound all spoke-reaching work: an unreachable target fails this reconcile instead of blocking a
	// worker until the client's own (possibly absent) timeouts fire — isolating one bad spoke from the
	// rest. Spoke failures are returned as errors so controller-runtime applies rate-limited backoff
	// (a persistently down spoke stops being retried at the flat resync interval and is surfaced),
	// while the happy path keeps the fixed resync cadence that drives drift healing.
	ctx, cancel := context.WithTimeout(ctx, reconcileTimeout)
	defer cancel()

	// Every KubernetesTarget in the CD's namespace is also swept for orphaned mirrors of this Kind, so a
	// spoke an instance was retargeted AWAY from (its annotation now names another target) still gets its
	// stale mirror collected — the current annotation set alone would miss it. Best-effort: a failure to
	// list targets just skips the orphan sweep this pass (retried next resync); it never blocks the
	// reflection of the CD's live targets, and the GC is Kind+label scoped so it can only ever touch this
	// reflector's own mirrors of THIS Kind, never another CD's.
	sweepTargets, err := r.namespaceTargets(ctx, cd.Namespace)
	if err != nil {
		r.log.Debug("compositionmirror: listing namespace targets for orphan sweep failed", "cd", req.String(), "error", err)
	}

	// Each hub instance is mirrored to the spoke named by its krateo.io/target annotation, or to the
	// CD's default deploy target when unannotated. Spoke clients are built per resolved target and
	// memoized for this reconcile (spokeResolverFor), so many instances bound to one spoke share a
	// client. KubernetesTargets are namespaced and resolved in the CD's own namespace.
	if err := reflectInstances(ctx, reflectParams{
		hub:           r.hubDynamic,
		resolveSpoke:  r.spokeResolverFor(cd.Namespace),
		defaultTarget: cd.Spec.Deploy.TargetRef.Name,
		sweepTargets:  sweepTargets,
		gvr:           gvr,
		apiVersion:    cd.Status.ApiVersion,
		kind:          cd.Status.Kind,
		namespace:     cd.Namespace,
		log:           r.log,
	}); err != nil {
		return reconcile.Result{}, fmt.Errorf("compositionmirror: reflecting %s: %w", req, err)
	}

	return reconcile.Result{RequeueAfter: requeueInterval}, nil
}

// reconcileDelete tears down a deleted CompositionDefinition: it clears the managed spoke mirrors on
// every spoke its instances fanned out to, deletes the hub Composition instances (so they never
// outlive their type), then releases the reflector's finalizer. The finalizer is held until every
// target's spoke cleanup succeeds, so a mirror is never orphaned by a premature release.
//
// Ordering is load-bearing for fan-out: the spokes an instance was mirrored to are recorded only by
// its krateo.io/target annotation, so the target set is read from the live hub Compositions and their
// mirrors are collected BEFORE the hub Compositions are deleted. Were the hub side cleared first, a
// spoke GC that failed and retried would have lost the annotated targets and orphaned their mirrors.
//
// Hub deletes are issued but not awaited: a hub Composition held by a user's own finalizer finishes
// deleting on its own, and the CD must not block on it.
func (r *Reconciler) reconcileDelete(ctx context.Context, cd *compositiondefinitionsv1alpha1.CompositionDefinition) (reconcile.Result, error) {
	if !controllerutil.ContainsFinalizer(cd, cdFinalizer) {
		return reconcile.Result{}, nil
	}
	if cd.Status.ApiVersion != "" && cd.Status.Kind != "" && cd.Status.Resource != "" {
		gv, err := schema.ParseGroupVersion(cd.Status.ApiVersion)
		if err != nil {
			return reconcile.Result{}, fmt.Errorf("compositionmirror: parsing generated apiVersion %q: %w", cd.Status.ApiVersion, err)
		}
		gvr := gv.WithResource(cd.Status.Resource)

		// For a remote CD, clear the managed spoke mirrors on every target the instances fanned out to.
		// This is required, not just tidy: the spoke cdc finalizes each mirror's Helm release on delete,
		// and the CompositionDefinition's own teardown waits for the spoke composition instances to be
		// gone before it removes the cdc/CRD — so leaving the mirrors would deadlock that teardown.
		// Bounded + spoke-dependent: an unreachable spoke keeps the CD Terminating and retries, the same
		// failure mode as that teardown. We do NOT release the finalizer until every live target succeeds.
		if clusterkube.IsRemote(cd.Spec.Deploy) {
			tctx, cancel := context.WithTimeout(ctx, reconcileTimeout)
			defer cancel()

			// This CD's live targets: the current-annotation targets + the default, read from the live hub
			// Compositions BEFORE they are deleted (below). These MUST be torn down — they are the spokes
			// this CD is actively mirroring to — so a failure here keeps the CD Terminating and retries.
			liveTargets, err := mirrorTargets(tctx, r.hubDynamic, gvr, cd.Namespace, cd.Spec.Deploy.TargetRef.Name)
			if err != nil {
				return reconcile.Result{}, fmt.Errorf("compositionmirror: enumerating spoke targets for %s/%s: %w", cd.Namespace, cd.Name, err)
			}

			resolve := r.spokeResolverFor(cd.Namespace)
			live := make(map[string]struct{}, len(liveTargets))
			for _, target := range liveTargets {
				live[target] = struct{}{}
				spoke, err := resolve(tctx, target)
				if err != nil {
					return reconcile.Result{}, fmt.Errorf("compositionmirror: reaching spoke %q to tear down %s/%s: %w", target, cd.Namespace, cd.Name, err)
				}
				// nil desired set: no mirror is wanted now, so garbageCollect removes every managed one
				// (and only those — never an instance authored directly on the spoke).
				if err := garbageCollect(tctx, spoke.Resource(gvr).Namespace(cd.Namespace), gvr, nil, r.log); err != nil {
					return reconcile.Result{}, fmt.Errorf("compositionmirror: tearing down spoke %q mirrors for %s/%s: %w", target, cd.Namespace, cd.Name, err)
				}
			}

			// Also sweep every OTHER KubernetesTarget in the namespace, to collect a mirror left on a spoke
			// an instance was retargeted away from (no annotation names it anymore, so mirrorTargets can't
			// see it). Best-effort: these targets are not this CD's active spokes, so an unreachable or
			// unrelated one must NOT block this CD's teardown — log and continue. The GC is Kind+label
			// scoped, so this can only ever remove this reflector's own mirrors of THIS Kind.
			nsTargets, err := r.namespaceTargets(tctx, cd.Namespace)
			if err != nil {
				r.log.Debug("compositionmirror: listing namespace targets for teardown sweep failed", "cd", cd.Name, "error", err)
			}
			for _, target := range nsTargets {
				if _, done := live[target]; done {
					continue
				}
				spoke, err := resolve(tctx, target)
				if err != nil {
					r.log.Debug("compositionmirror: sweep-only spoke unreachable at teardown, skipping", "cd", cd.Name, "target", target, "error", err)
					continue
				}
				if err := garbageCollect(tctx, spoke.Resource(gvr).Namespace(cd.Namespace), gvr, nil, r.log); err != nil {
					r.log.Debug("compositionmirror: sweep-only teardown GC failed, skipping", "cd", cd.Name, "target", target, "error", err)
				}
			}
		}

		// Live spokes are clean; now clear the hub Compositions (hub-only, always reachable).
		if err := deleteAllHubCompositions(ctx, r.hubDynamic, gvr, cd.Namespace); err != nil {
			return reconcile.Result{}, fmt.Errorf("compositionmirror: cleaning hub compositions for %s/%s: %w", cd.Namespace, cd.Name, err)
		}
	}
	controllerutil.RemoveFinalizer(cd, cdFinalizer)
	if err := r.hub.Update(ctx, cd); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{}, nil
}

// mirrorTargets returns the distinct KubernetesTargets the hub Composition instances of a Kind fan out
// to: each instance's krateo.io/target annotation, or defaultTarget when it has none. defaultTarget is
// always included (first) so its spoke is torn down even when no instance currently targets it — e.g.
// none exist, or every one was retargeted away. It must be called before the hub Compositions are
// deleted, since the annotations are the only record of which spokes hold mirrors.
func mirrorTargets(ctx context.Context, hubDyn dynamic.Interface, gvr schema.GroupVersionResource, ns, defaultTarget string) ([]string, error) {
	res := hubDyn.Resource(gvr).Namespace(ns)
	list, err := res.List(ctx, metav1.ListOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return []string{defaultTarget}, nil // CRD already gone: only the default spoke can hold mirrors
		}
		return nil, fmt.Errorf("listing hub compositions: %w", err)
	}
	seen := map[string]struct{}{defaultTarget: {}}
	targets := []string{defaultTarget}
	for i := range list.Items {
		t := targetOf(&list.Items[i], defaultTarget)
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		targets = append(targets, t)
	}
	return targets, nil
}

// namespaceTargets lists the names of every KubernetesTarget in namespace ns. These are the candidate
// spokes a CD's instances can fan out to; the reflector sweeps them for orphaned mirrors so a spoke an
// instance was retargeted away from — one no current annotation names — still gets its stale mirror
// collected. It reads from the hub, so it is always reachable; callers treat a failure as best-effort.
func (r *Reconciler) namespaceTargets(ctx context.Context, ns string) ([]string, error) {
	var list compositiondefinitionsv1alpha1.KubernetesTargetList
	if err := r.hub.List(ctx, &list, client.InNamespace(ns)); err != nil {
		return nil, fmt.Errorf("listing KubernetesTargets in %s: %w", ns, err)
	}
	names := make([]string, 0, len(list.Items))
	for i := range list.Items {
		names = append(names, list.Items[i].GetName())
	}
	return names, nil
}

// deleteAllHubCompositions issues a delete for every hub Composition of the given Kind in the
// namespace. A missing resource (its CRD already gone) and already-absent instances are treated as
// success.
func deleteAllHubCompositions(ctx context.Context, hubDyn dynamic.Interface, gvr schema.GroupVersionResource, ns string) error {
	res := hubDyn.Resource(gvr).Namespace(ns)
	list, err := res.List(ctx, metav1.ListOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("listing hub compositions: %w", err)
	}
	for i := range list.Items {
		name := list.Items[i].GetName()
		err := res.Delete(ctx, name, metav1.DeleteOptions{})
		auditrec.RecordDeleteSent(ctx, auditrec.ActionCompositionHubDelete,
			auditrec.Target{GVR: gvr, Kind: list.Items[i].GetKind(), Namespace: ns, Name: name}, string(list.Items[i].GetUID()), err)
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting hub composition %q: %w", name, err)
		}
	}
	return nil
}

// ensureWatch registers a dynamic informer-backed watch on the given generated composition Kind the
// first time it is seen, so hub Composition edits enqueue their CompositionDefinition and reflect at
// once rather than at resync latency. Registration is best-effort and deduped per GVK: a Kind that is
// not yet discoverable simply isn't watched this pass and is retried on the next reconcile.
//
// Note (v1): watches are not torn down when a CD is deleted — controller-runtime has no clean
// per-source stop. A lingering watch on a since-removed Kind is harmless (its informer errors and idles),
// and the hub composition CRD outlives the CD today anyway. Watch teardown is tracked in the design's
// open items.
func (r *Reconciler) ensureWatch(gvk schema.GroupVersionKind) {
	if r.ctrl == nil {
		return // not wired for dynamic watches (e.g. under unit tests); resync still reflects
	}
	// GenerationChangedPredicate reconciles on spec changes, creates and deletes, but NOT on
	// status-only / metadata updates. That is essential: the reflector writes the spoke status back
	// onto the hub Composition, and reconciling on that write would loop. (Status updates leave
	// metadata.generation unchanged because the composition CRD has a status subresource.)
	if err := r.cfgWatch.EnsureWatch(r.ctrl, gvk, enqueueCDForComposition(r.hub), predicate.GenerationChangedPredicate{}); err != nil {
		r.log.Debug("compositionmirror: dynamic watch registration deferred", "gvk", gvk.String(), "error", err)
		return
	}
	r.log.Debug("compositionmirror: registered dynamic watch on hub Kind", "gvk", gvk.String())
}

// enqueueCDForComposition maps a hub Composition event to the reconcile of its owning
// CompositionDefinition — the remote-targeted CD in the same namespace whose generated Kind matches
// the Composition's. (Local CDs do not reflect, so they are skipped.)
func enqueueCDForComposition(hub client.Client) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		var cds compositiondefinitionsv1alpha1.CompositionDefinitionList
		if err := hub.List(ctx, &cds, client.InNamespace(obj.GetNamespace())); err != nil {
			return nil
		}
		kind := obj.GetObjectKind().GroupVersionKind().Kind
		var reqs []reconcile.Request
		for i := range cds.Items {
			cd := &cds.Items[i]
			if clusterkube.IsRemote(cd.Spec.Deploy) && cd.Status.Kind == kind {
				reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(cd)})
			}
		}
		return reqs
	}
}

// spokeResolver returns the dynamic client for the spoke addressed by a resolved target name. The
// engine calls it once per distinct target in a reconcile; the production implementation
// (spokeResolverFor) memoizes and wraps clusterkube.Remote, while unit tests supply per-target fakes.
type spokeResolver func(ctx context.Context, target string) (dynamic.Interface, error)

// reflectParams carries everything reflectInstances needs. The hub is a dynamic client and spokes are
// obtained through resolveSpoke, so the engine is unit-testable with fakes, independent of controller
// wiring.
type reflectParams struct {
	hub           dynamic.Interface
	resolveSpoke  spokeResolver
	defaultTarget string // CD spec.deploy.targetRef.Name — the spoke for instances without an annotation
	// sweepTargets are additional spokes (every KubernetesTarget in the namespace) to garbage-collect
	// for orphaned mirrors, even when no current instance targets them — so a spoke an instance was
	// retargeted away from still gets its stale mirror collected. Best-effort: a sweep-only target that
	// won't resolve or GC does not fail the reconcile (it isn't one this CD is actively mirroring to).
	sweepTargets []string
	gvr          schema.GroupVersionResource
	apiVersion   string
	kind         string
	namespace    string
	log          logging.Logger
}

// reflectInstances mirrors the hub Composition instances of one Kind (in a single namespace) onto their
// spokes and reads their status back, then garbage-collects spoke mirrors with no hub counterpart.
//
// Instances are grouped by resolved target (krateo.io/target annotation, else defaultTarget) so each
// spoke is reconciled with exactly the instances bound to it. The set of spokes visited is the union of
// the group targets, the default, and sweepTargets (every namespace KubernetesTarget) — the last so a
// spoke an instance was RETARGETED AWAY from, which no current annotation names, still has its orphaned
// mirror collected. Targets this CD actively mirrors to (any group target, and the default) are
// authoritative: a resolve or GC failure there is returned so the reconcile backs off and retries.
// Sweep-only targets (no instances of this CD) are best-effort: an unreachable or unrelated namespace
// target is logged and skipped, never breaking this CD's reflection. The GC is Kind+label scoped, so
// sweeping unrelated targets can only ever touch this reflector's own mirrors of THIS Kind.
//
// Residual limit: an orphan on a spoke that is BOTH retargeted-away-from AND permanently unreachable is
// not collected (its GC keeps being skipped best-effort); every reachable spoke is swept at resync.
func reflectInstances(ctx context.Context, p reflectParams) error {
	hubRes := p.hub.Resource(p.gvr).Namespace(p.namespace)

	hubList, err := hubRes.List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing hub compositions: %w", err)
	}

	// Group instances by their resolved target; seed the default so its spoke is always reconciled.
	groups := map[string][]*unstructured.Unstructured{p.defaultTarget: nil}
	for i := range hubList.Items {
		inst := &hubList.Items[i]
		target := targetOf(inst, p.defaultTarget)
		groups[target] = append(groups[target], inst)
	}

	// The full set of spokes to visit: group targets (already includes the default) plus the sweep set.
	sweepSet := make(map[string]struct{}, len(groups)+len(p.sweepTargets))
	for target := range groups {
		sweepSet[target] = struct{}{}
	}
	for _, target := range p.sweepTargets {
		if target != "" {
			sweepSet[target] = struct{}{}
		}
	}

	for target := range sweepSet {
		insts := groups[target] // nil for a sweep-only target
		// Authoritative = this CD is actively mirroring to the target (it has instances, or it is the
		// default). Only these fail the reconcile; sweep-only targets are best-effort.
		authoritative := len(insts) > 0 || target == p.defaultTarget

		spoke, err := p.resolveSpoke(ctx, target)
		if err != nil {
			if authoritative {
				return fmt.Errorf("building spoke for target %q: %w", target, err)
			}
			p.log.Debug("compositionmirror: sweep-only spoke unresolved, skipping", "target", target, "error", err)
			continue
		}
		spokeRes := spoke.Resource(p.gvr).Namespace(p.namespace)

		desired := make(map[string]struct{}, len(insts))
		for _, inst := range insts {
			desired[inst.GetName()] = struct{}{}

			if _, err := mirrorDown(ctx, spokeRes, p.gvr, inst, p.apiVersion, p.kind, p.namespace); err != nil {
				return fmt.Errorf("mirroring %s/%s to spoke %q: %w", p.namespace, inst.GetName(), target, err)
			}

			if err := mirrorStatusUp(ctx, hubRes, spokeRes, inst.GetName()); err != nil {
				// Status readback is best-effort: a spoke instance not yet rendered has no status, and a
				// composition CRD without a status subresource cannot accept one. Neither is fatal to the
				// desired-state mirror, which is the reflector's primary job.
				p.log.Debug("compositionmirror: status readback skipped", "name", inst.GetName(), "target", target, "error", err)
			}
		}

		// desired may be empty (sweep-only target, or the default with no instances): garbageCollect then
		// removes every managed mirror on that spoke — exactly the orphan we are sweeping for.
		if err := garbageCollect(ctx, spokeRes, p.gvr, desired, p.log); err != nil {
			if authoritative {
				return fmt.Errorf("garbage-collecting spoke %q: %w", target, err)
			}
			p.log.Debug("compositionmirror: sweep-only GC failed, skipping", "target", target, "error", err)
		}
	}

	return nil
}

// targetOf reports the KubernetesTarget a hub Composition instance is mirrored to: its
// krateo.io/target annotation when set, otherwise the CompositionDefinition's default deploy target.
func targetOf(inst *unstructured.Unstructured, defaultTarget string) string {
	if t := inst.GetAnnotations()[targetAnnotation]; t != "" {
		return t
	}
	return defaultTarget
}

// spokeResolverFor returns a spokeResolver bound to namespace ns for a single reconcile: it builds a
// spoke dynamic client per target name (via r.newSpoke, defaulting to clusterkube.Remote) and memoizes
// the result, so many instances bound to the same target build the client — which re-reads the
// KubernetesTarget and its kubeconfig Secret — only once. Errors are not cached: a transient
// target-read failure should be retried on the next reconcile.
func (r *Reconciler) spokeResolverFor(ns string) spokeResolver {
	build := r.newSpoke
	if build == nil {
		build = remoteSpokeDynamic
	}
	cache := map[string]dynamic.Interface{}
	return func(ctx context.Context, target string) (dynamic.Interface, error) {
		if dyn, ok := cache[target]; ok {
			return dyn, nil
		}
		dyn, err := build(ctx, r.hub, ns, target)
		if err != nil {
			return nil, err
		}
		cache[target] = dyn
		return dyn, nil
	}
}

// remoteSpokeDynamic builds a spoke's dynamic client for a target name via clusterkube.Remote — the
// same client path the single-target reflector used. It is the production spokeResolverFor backend;
// unit tests substitute a fake via Reconciler.newSpoke.
func remoteSpokeDynamic(ctx context.Context, mgmt client.Client, ns, target string) (dynamic.Interface, error) {
	clients, err := clusterkube.Remote(ctx, mgmt, ns, &compositiondefinitionsv1alpha1.DeploymentTarget{
		TargetRef: &compositiondefinitionsv1alpha1.TargetReference{Name: target},
	})
	if err != nil {
		return nil, err
	}
	return clients.Dynamic, nil
}

// mirrorLabels are the labels a spoke mirror should carry: the hub Composition's own labels — which
// include the krateo.io/composition-definition-* and composition-version labels the spoke cdc uses to
// resolve this instance's CompositionDefinition (see composition-dynamic-controller archive/getter) —
// plus the reflector's management marker so garbage collection can recognise the mirror.
func mirrorLabels(hubInst *unstructured.Unstructured) map[string]string {
	labels := map[string]string{}
	for k, v := range hubInst.GetLabels() {
		labels[k] = v
	}
	labels[managedByLabel] = managedByValue
	return labels
}

// mirrorWrite is the write mirrorDown sent to the spoke, if any.
type mirrorWrite string

const (
	mirrorCreated   mirrorWrite = "created"
	mirrorUpdated   mirrorWrite = "updated"
	mirrorUnchanged mirrorWrite = "unchanged" // the mirror already matched: no request was sent
)

// mirrorDown creates-or-updates the spoke Composition from its hub counterpart, and reports which
// write it sent. The hub is the source of truth for spec (hub wins on drift); the mirror is stamped
// with the management label so GC can recognise it.
//
// A mirror that already carries the hub's labels, the management marker and the hub's spec is left
// alone: no Update is sent (core-provider#423). It used to Update on every resync, so "the mirror
// changed" meant nothing and every resync was a write, which an audit record per write would have
// turned into one row per instance per resync.
func mirrorDown(ctx context.Context, spokeRes dynamic.ResourceInterface, gvr schema.GroupVersionResource, hubInst *unstructured.Unstructured, apiVersion, kind, namespace string) (mirrorWrite, error) {
	t := auditrec.Target{GVR: gvr, Kind: kind, Namespace: namespace, Name: hubInst.GetName()} // core-provider#423: composition.mirror
	spec, _, err := unstructured.NestedMap(hubInst.Object, "spec")
	if err != nil {
		return "", fmt.Errorf("reading hub spec: %w", err)
	}

	live, err := spokeRes.Get(ctx, hubInst.GetName(), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		mirror := &unstructured.Unstructured{}
		mirror.SetAPIVersion(apiVersion)
		mirror.SetKind(kind)
		mirror.SetNamespace(namespace)
		mirror.SetName(hubInst.GetName())
		mirror.SetLabels(mirrorLabels(hubInst))
		if spec != nil {
			if err := unstructured.SetNestedMap(mirror.Object, spec, "spec"); err != nil {
				return "", fmt.Errorf("setting mirror spec: %w", err)
			}
		}
		created, err := spokeRes.Create(ctx, mirror, metav1.CreateOptions{})
		auditrec.RecordApply(ctx, auditrec.ActionCompositionMirror, t, objectclient.ApplyResult{Op: objectclient.ApplyCreated, After: created}, err)
		if err != nil {
			return "", err
		}
		return mirrorCreated, nil
	} else if err != nil {
		return "", fmt.Errorf("reading spoke mirror: %w", err)
	}

	// Sync the hub's labels onto the mirror (hub wins for keys it sets) and ensure the management
	// marker, preserving any other labels the spoke added. The hub's labels include the
	// krateo.io/composition-definition-* and composition-version labels the spoke cdc resolves this
	// instance's CompositionDefinition against, so the mirror carries them directly rather than
	// relying on the projected admission policy or the cdc's unique-kind fallback. Then push the
	// desired spec (hub wins on drift).
	liveLabels := live.GetLabels()
	labels := make(map[string]string, len(liveLabels)+len(hubInst.GetLabels())+1)
	for k, v := range liveLabels {
		labels[k] = v
	}
	for k, v := range hubInst.GetLabels() {
		labels[k] = v
	}
	labels[managedByLabel] = managedByValue

	liveSpec, liveHasSpec, err := unstructured.NestedMap(live.Object, "spec")
	if err != nil {
		return "", fmt.Errorf("reading spoke mirror spec: %w", err)
	}
	specSame := (spec == nil && !liveHasSpec) || (spec != nil && liveHasSpec && reflect.DeepEqual(spec, liveSpec))
	if specSame && labelsEqual(labels, liveLabels) {
		return mirrorUnchanged, nil
	}

	before := live.DeepCopy()
	live.SetLabels(labels)
	if spec != nil {
		if err := unstructured.SetNestedMap(live.Object, spec, "spec"); err != nil {
			return "", fmt.Errorf("updating mirror spec: %w", err)
		}
	} else {
		unstructured.RemoveNestedField(live.Object, "spec")
	}
	updated, err := spokeRes.Update(ctx, live, metav1.UpdateOptions{})
	auditrec.RecordUpdate(ctx, auditrec.ActionCompositionMirror, t, before, updated, err, false)
	if err != nil {
		return "", err
	}
	return mirrorUpdated, nil
}

// labelsEqual is map equality where a nil and an empty label set are the same.
func labelsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

// mirrorStatusUp copies the spoke Composition's status onto the hub Composition. The hub instance is
// re-read so the status write carries a current resourceVersion.
func mirrorStatusUp(ctx context.Context, hubRes, spokeRes dynamic.ResourceInterface, name string) error {
	live, err := spokeRes.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("reading spoke status: %w", err)
	}
	status, found, err := unstructured.NestedMap(live.Object, "status")
	if err != nil {
		return fmt.Errorf("reading spoke status field: %w", err)
	}
	if !found {
		return nil // nothing rendered on the spoke yet
	}

	cur, err := hubRes.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("re-reading hub composition: %w", err)
	}
	// Skip the write when the hub status already equals the spoke's. Besides avoiding a needless write
	// every resync, this is a second guard against a watch loop: an unconditional UpdateStatus bumps
	// the hub Composition's resourceVersion, which would re-fire the watch even if nothing changed.
	if curStatus, _, _ := unstructured.NestedMap(cur.Object, "status"); reflect.DeepEqual(curStatus, status) {
		return nil
	}
	if err := unstructured.SetNestedMap(cur.Object, status, "status"); err != nil {
		return fmt.Errorf("setting hub status: %w", err)
	}
	if _, err := hubRes.UpdateStatus(ctx, cur, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("writing hub status: %w", err)
	}
	return nil
}

// garbageCollect deletes spoke mirrors this reflector owns (management label) that are no longer
// present on the hub. The label is re-checked client-side so the set-difference is correct even if
// the API server does not apply the label selector.
func garbageCollect(ctx context.Context, spokeRes dynamic.ResourceInterface, gvr schema.GroupVersionResource, desired map[string]struct{}, log logging.Logger) error {
	managed, err := spokeRes.List(ctx, metav1.ListOptions{LabelSelector: managedByLabel + "=" + managedByValue})
	if err != nil {
		return fmt.Errorf("listing managed spoke mirrors: %w", err)
	}
	for i := range managed.Items {
		item := &managed.Items[i]
		if item.GetLabels()[managedByLabel] != managedByValue {
			continue // defensive: only ever delete mirrors we own
		}
		name := item.GetName()
		if _, keep := desired[name]; keep {
			continue
		}
		err := spokeRes.Delete(ctx, name, metav1.DeleteOptions{})
		// core-provider#423: composition.mirror, verb delete.
		auditrec.RecordDeleteSent(ctx, auditrec.ActionCompositionMirror,
			auditrec.Target{GVR: gvr, Kind: item.GetKind(), Namespace: item.GetNamespace(), Name: name}, string(item.GetUID()), err)
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting orphaned spoke mirror %q: %w", name, err)
		}
		log.Debug("compositionmirror: deleted orphaned spoke mirror", "name", name)
	}
	return nil
}

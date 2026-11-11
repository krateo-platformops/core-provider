package compositiondefinitions

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"time"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/controllers/compositiondefinitions/helpers/getters"
	"github.com/krateo-platformops/core-provider/internal/tools/clusterkube"
	"github.com/krateo-platformops/core-provider/internal/tools/deploy"
	"github.com/krateo-platformops/plumbing/labels"
	rtv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	"github.com/krateo-platformops/provider-runtime/pkg/scaletozero"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8slabels "k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
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

// defaultScaleToZeroHoldDown is spec.controller.scaleToZeroHoldDown when unset (#393).
const defaultScaleToZeroHoldDown = 10 * time.Minute

// notDeployedRequeue is how soon a definition that opted in but has no generated GVK yet is looked
// at again. The main reconciler records the GVK when it deploys, and a status write does not bump
// generation, so nothing else would requeue it.
const notDeployedRequeue = 30 * time.Second

// scaleToZeroReconciler scales an opted-in definition's composition controller between zero and the
// chart's replica count with provider-runtime's scaletozero helper (#393).
//
// A SEPARATE CONTROLLER, NOT OBSERVE. Scaling is a write, and the definition's Observe is a pure
// read by rule. It owns exactly two things: the controller Deployment's spec.replicas (through the
// helper's scale patch; the definition's own re-apply keeps the live value, see
// DeployOptions.KeepLiveReplicas) and status.scaleToZero (a merge patch on that field only). The
// definition's main reconciler reads status.scaleToZero and the live Deployment to report Ready and
// ControllerActive.
//
// WHAT TRIGGERS IT. The helper watches nothing. This reconciler runs on a definition's spec changes
// (generation), on a sibling definition's spec changes (one sharing the controller can opt in or
// out), on the creation and deletion of an INSTANCE of an opted-in Kind (the wake watch, one
// metadata-only watch per opted-in Kind and version, which only grows: controller-runtime has no
// unwatch), and on the helper's own RequeueAfter during the hold-down.
type scaleToZeroReconciler struct {
	kube  client.Client // the manager's client: definitions
	live  client.Client // built with client.New, as the helper requires: live reads and the scale patch
	cache cache.Cache
	log   logging.Logger
	// active is the replica count a woken controller runs (the chart's cdc.replicaCount).
	active func(gvr schema.GroupVersionResource) (int32, error)
	now    func() time.Time

	ctrl    controller.Controller
	watched sync.Map // schema.GroupVersionKind -> struct{}
}

func (r *scaleToZeroReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	var cd compositiondefinitionsv1alpha1.CompositionDefinition
	if err := r.kube.Get(ctx, req.NamespacedName, &cd); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if cd.GetDeletionTimestamp() != nil {
		return reconcile.Result{}, nil
	}
	if !scaleToZeroOptedIn(&cd) {
		// Opted out (or never in): the definition's own re-apply renders the chart's replicas, which
		// wakes a controller left at 0. All that is left here is to drop the state.
		return reconcile.Result{}, r.persist(ctx, &cd, nil)
	}
	gvr, ok := statusGVR(&cd)
	if !ok || cd.GetCondition(rtv1.TypeReady).Status != metav1.ConditionTrue {
		// Not deployed, or its CRD not yet served: there is no controller to scale and no Kind to
		// watch. Ready=True implies the CRD is Established (#269).
		return reconcile.Result{RequeueAfter: notDeployedRequeue}, nil
	}
	gvk := cd.Status.CurrentGVK()

	siblings, err := getters.GetCompositionDefinitionsWithVersion(ctx, r.kube, gvk)
	if err != nil {
		return reconcile.Result{}, err
	}
	if owner := electControllerOwner(&cd, siblings); owner != nil {
		// A follower: the owner runs the shared controller and decides for it. A follower keeps no
		// scale-to-zero state of its own, so a stale block from when it owned the controller goes.
		return reconcile.Result{}, r.persist(ctx, &cd, nil)
	}
	if err := r.ensureWakeWatch(gvk); err != nil {
		return reconcile.Result{}, err
	}

	decision, err := r.decide(ctx, &cd, gvr, gvk, siblings)
	if perr := r.persist(ctx, &cd, decision.status); perr != nil && err == nil {
		err = perr
	}
	if err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{RequeueAfter: decision.requeueAfter}, nil
}

type scaleToZeroDecision struct {
	status       *compositiondefinitionsv1alpha1.ScaleToZeroStatus
	requeueAfter time.Duration
}

// decide calls the helper for an owning, opted-in, deployed definition and says what to persist.
func (r *scaleToZeroReconciler) decide(ctx context.Context, cd *compositiondefinitionsv1alpha1.CompositionDefinition,
	gvr schema.GroupVersionResource, gvk schema.GroupVersionKind,
	siblings []compositiondefinitionsv1alpha1.CompositionDefinition) (scaleToZeroDecision, error) {

	holdDown, err := scaleToZeroHoldDown(cd)
	if err != nil {
		return scaleToZeroDecision{status: unknownStatus(cd)}, err
	}
	active, err := r.active(gvr)
	if err != nil {
		return scaleToZeroDecision{status: unknownStatus(cd)}, err
	}
	heldBy := notOptedInSiblings(cd, siblings)
	lastSeen := lastSeenOf(cd)
	if len(heldBy) > 0 {
		// Every definition sharing the controller must opt in (Diego, 2026-11-11). Passing "seen
		// now" makes the helper decide running, so it never scales the shared controller down, and
		// wakes it if it is at 0 (a sibling that never opted in was added after the scale-down).
		// The helper stays the only writer of replicas.
		lastSeen = r.now()
	}
	// The selector is EXACTLY the controller's own (composition-dynamic-controller main.go: one
	// requirement, the composition-version label equal to its version). A narrower one would miss
	// instances and scale a controller to zero while it still has work.
	sel := k8slabels.SelectorFromSet(k8slabels.Set{labels.CompositionVersionLabel: gvk.Version})

	res, err := scaletozero.Reconcile(ctx, r.live, deploy.ControllerDeploymentKey(controllerNamespace(cd), gvr),
		gvk, sel, lastSeen, holdDown, active)
	st := &compositiondefinitionsv1alpha1.ScaleToZeroStatus{
		Replicas: &res.Replicas,
		Reason:   string(res.Reason),
		HeldBy:   heldBy,
	}
	// LASTSEEN IS PERSISTED ONLY WHILE THE HOLD-DOWN IS RUNNING OR THE KIND IS IDLE. While instances
	// exist the helper hands back "now" on every pass, and this reconciler runs on events, not on a
	// poll, so a stored "now" would age: a controller active for days with no events would carry a
	// lastSeen days old, and deleting its last instance would scale it to zero AT ONCE, skipping the
	// hold-down. Stored as absent instead, the first pass that sees the Kind empty (the deletion's
	// own event) starts the full hold-down there: the helper starts it at "now" for a zero lastSeen.
	// A held controller stores none either: it is decided as "seen now" every pass.
	if err == nil && len(heldBy) == 0 && (res.Reason == scaletozero.ReasonIdleHoldDown || res.Reason == scaletozero.ReasonIdle) {
		t := metav1.NewTime(res.LastSeen.UTC().Truncate(time.Second))
		st.LastSeen = &t
	}
	if err != nil {
		// The helper changed nothing and reports replicas that never read as 0. Keep the last
		// lastSeen so a transient failure does not restart or skip the hold-down.
		st.LastSeen = storedLastSeen(cd)
		return scaleToZeroDecision{status: st}, err
	}
	r.log.Debug("Scale-to-zero decision", "definition", cd.Namespace+"/"+cd.Name, "gvk", gvk.String(),
		"replicas", res.Replicas, "reason", string(res.Reason), "patched", res.Patched, "heldBy", heldBy,
		"requeueAfter", res.RequeueAfter.String())
	if res.Patched {
		r.log.Info("Scaled the composition controller", "definition", cd.Namespace+"/"+cd.Name,
			"gvk", gvk.String(), "replicas", res.Replicas, "reason", string(res.Reason))
	}
	return scaleToZeroDecision{status: st, requeueAfter: res.RequeueAfter}, nil
}

// unknownStatus keeps what was known when nothing could be decided.
func unknownStatus(cd *compositiondefinitionsv1alpha1.CompositionDefinition) *compositiondefinitionsv1alpha1.ScaleToZeroStatus {
	st := cd.Status.ScaleToZero.DeepCopy()
	if st == nil {
		st = &compositiondefinitionsv1alpha1.ScaleToZeroStatus{}
	}
	st.Reason = string(scaletozero.ReasonUnknown)
	return st
}

func storedLastSeen(cd *compositiondefinitionsv1alpha1.CompositionDefinition) *metav1.Time {
	if cd.Status.ScaleToZero == nil || cd.Status.ScaleToZero.LastSeen == nil {
		return nil
	}
	t := *cd.Status.ScaleToZero.LastSeen
	return &t
}

func lastSeenOf(cd *compositiondefinitionsv1alpha1.CompositionDefinition) time.Time {
	if t := storedLastSeen(cd); t != nil {
		return t.Time
	}
	return time.Time{}
}

// scaleToZeroHoldDown is spec.controller.scaleToZeroHoldDown, or the default. Admission already
// rejects a non-positive or malformed duration; an error here means a definition stored before that
// rule, and it is reported rather than defaulted, so a typo cannot silently become 10m.
func scaleToZeroHoldDown(cd *compositiondefinitionsv1alpha1.CompositionDefinition) (time.Duration, error) {
	c := cd.Spec.Controller
	if c == nil || c.ScaleToZeroHoldDown == nil {
		return defaultScaleToZeroHoldDown, nil
	}
	d, err := time.ParseDuration(*c.ScaleToZeroHoldDown)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("spec.controller.scaleToZeroHoldDown %q is not a positive Go duration", *c.ScaleToZeroHoldDown)
	}
	return d, nil
}

// notOptedInSiblings names, sorted, the live local definitions that share self's controller and
// have not opted in. self is excluded (it is opted in, or this is not called).
func notOptedInSiblings(self *compositiondefinitionsv1alpha1.CompositionDefinition,
	siblings []compositiondefinitionsv1alpha1.CompositionDefinition) []string {
	var out []string
	for i := range siblings {
		s := &siblings[i]
		if s.GetName() == self.GetName() && s.GetNamespace() == self.GetNamespace() {
			continue
		}
		if s.GetDeletionTimestamp() != nil || scaleToZeroOptedIn(s) || clusterkube.IsRemote(s.Spec.Deploy) {
			continue
		}
		out = append(out, s.GetNamespace()+"/"+s.GetName())
	}
	sort.Strings(out)
	return out
}

// scaleToZeroPatch is st as a JSON merge patch that REPLACES the stored block: every field is named,
// with null for an absent one. Marshalling st directly would omit its empty fields (omitempty),
// and in a merge patch an omitted key means "unchanged", so a lastSeen cleared here would survive in
// the stored object, which is exactly the stale lastSeen the hold-down rule exists to prevent
// (caught by the envtest suite: Active and held passes kept a 20-minute-old lastSeen). nil removes
// the whole block.
func scaleToZeroPatch(st *compositiondefinitionsv1alpha1.ScaleToZeroStatus) any {
	if st == nil {
		return nil
	}
	m := map[string]any{"lastSeen": nil, "replicas": nil, "reason": nil, "heldBy": nil}
	if st.LastSeen != nil {
		m["lastSeen"] = st.LastSeen
	}
	if st.Replicas != nil {
		m["replicas"] = *st.Replicas
	}
	if st.Reason != "" {
		m["reason"] = st.Reason
	}
	if len(st.HeldBy) > 0 {
		m["heldBy"] = st.HeldBy
	}
	return m
}

// persist writes status.scaleToZero with a merge patch on that field only, and only when it
// changed, so the reconciler adds no write while nothing changes. nil removes it.
func (r *scaleToZeroReconciler) persist(ctx context.Context, cd *compositiondefinitionsv1alpha1.CompositionDefinition,
	st *compositiondefinitionsv1alpha1.ScaleToZeroStatus) error {
	if reflect.DeepEqual(cd.Status.ScaleToZero, st) {
		return nil
	}
	body, err := json.Marshal(map[string]any{"status": map[string]any{"scaleToZero": scaleToZeroPatch(st)}})
	if err != nil {
		return err
	}
	if err := r.kube.Status().Patch(ctx, cd, client.RawPatch(types.MergePatchType, body)); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("writing status.scaleToZero: %w", err)
	}
	return nil
}

// ensureWakeWatch starts, once per Kind and version, a metadata-only watch on its instances that
// enqueues every definition of that Kind and version when one is created or deleted. Creation is
// the wake-up; deletion starts the hold-down without waiting for any poll.
func (r *scaleToZeroReconciler) ensureWakeWatch(gvk schema.GroupVersionKind) error {
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
		return fmt.Errorf("starting the scale-to-zero wake watch on %s: %w", gvk.String(), err)
	}
	return nil
}

// definitionsOf maps an instance event to every definition currently at that Kind and version.
func (r *scaleToZeroReconciler) definitionsOf(gvk schema.GroupVersionKind) handler.MapFunc {
	return func(ctx context.Context, _ client.Object) []reconcile.Request {
		cds, err := getters.GetCompositionDefinitionsWithVersion(ctx, r.kube, gvk)
		if err != nil {
			r.log.Info("Scale-to-zero wake watch could not list definitions; the next definition event retries",
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

// SetupScaleToZero registers the scale-to-zero reconciler (#393). live must be built with
// client.New (the helper reads live and paginates); templatePath is the shipped controller
// Deployment template, the source of the replica count a controller is woken to.
func SetupScaleToZero(mgr ctrl.Manager, live client.Client, log logging.Logger, templatePath string) error {
	return setupScaleToZero(mgr, live, log, func(gvr schema.GroupVersionResource) (int32, error) {
		return deploy.ChartControllerReplicas(templatePath, gvr)
	})
}

// setupScaleToZero is SetupScaleToZero with the replica source injected, so a test runs the real
// controller without the mounted chart template.
func setupScaleToZero(mgr ctrl.Manager, live client.Client, log logging.Logger, active func(schema.GroupVersionResource) (int32, error)) error {
	r := &scaleToZeroReconciler{
		kube:   mgr.GetClient(),
		live:   live,
		cache:  mgr.GetCache(),
		log:    log.WithValues("controller", "scaletozero"),
		active: active,
		now:    time.Now,
	}
	c, err := ctrl.NewControllerManagedBy(mgr).
		Named("compositiondefinition-scaletozero").
		For(&compositiondefinitionsv1alpha1.CompositionDefinition{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&compositiondefinitionsv1alpha1.CompositionDefinition{},
			handler.EnqueueRequestsFromMapFunc(enqueueSiblingDefinitions(mgr.GetClient())),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Build(r)
	if err != nil {
		return err
	}
	r.ctrl = c
	return nil
}

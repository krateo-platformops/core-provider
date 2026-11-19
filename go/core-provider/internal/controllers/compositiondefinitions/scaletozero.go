package compositiondefinitions

import (
	"context"
	"fmt"
	"github.com/krateo-platformops/core-provider/internal/cdreason"
	"sort"
	"strings"
	"time"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/tools/clusterkube"
	rtv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// scaleToZeroOptedIn reports whether cr opted its controller into scale-to-zero (#393). It is the
// ONE place the opt-in is read, so Observe's dry-run, Create and Update cannot disagree about who
// writes the controller's replicas: if they did, Observe would see the helper's 0 as drift and
// Update would apply the chart value over it.
//
// Remote definitions never count: admission rejects the combination (core-provider#442), and a
// definition stored before that rule existed must not be treated as opted in either.
func scaleToZeroOptedIn(cr *compositiondefinitionsv1alpha1.CompositionDefinition) bool {
	if cr == nil || clusterkube.IsRemote(cr.Spec.Deploy) {
		return false
	}
	c := cr.Spec.Controller
	return c != nil && c.ScaleToZero != nil && *c.ScaleToZero
}

// controllerReadiness is what a deployed definition reports about its composition controller (#393):
// its Ready condition (owners only; a follower keeps followerCondition) and ControllerActive, the
// shared condition (plumbing conditions) for "is anything reconciling this Kind right now".
//
// The cases, from Diego's decisions of 2026-11-10/11:
//
//	replicas > 0, rolled out           Ready Available           ControllerActive True  Running
//	replicas > 0, rolling out          Ready False Creating       ControllerActive True  Running
//	replicas > 0, given up or lost     Ready False Unavailable    ControllerActive True  Running
//	0, opted in (declared)             Ready True ScaledToZero    ControllerActive False ScaledToZero
//	0, NOT opted in (undeclared)       Ready False Unavailable    ControllerActive False Unavailable
//	Deployment absent                  Ready False Creating       ControllerActive Unknown NotObserved
//	Deployment unreadable              (Observe errors: Ready NotObserved) ControllerActive Unknown NotObserved
//
// AVAILABLE MEANS THE CONTROLLER IS UP (Diego, 2026-11-16, on openstack's gate): its Deployment has
// rolled out, for its current generation, and is available. Before, Available needed only declared
// replicas, so a definition read Available while its controller was still starting, or crash-looping.
// Not available splits on the Deployment's OWN verdict, not a timer of ours (frontend's catch, same
// day): a rollout in progress is Creating (converging); a rollout the Deployment gave up on
// (Progressing=False/ProgressDeadlineExceeded, from its progressDeadlineSeconds) or availability lost
// after a completed rollout is Unavailable (failed), naming the Deployment's reason. A node drain of a
// one-replica controller therefore reads Unavailable until its pod is back, which is true.
// ControllerActive keeps its meaning, "replicas are declared", which scale-to-zero reports on.
//
// The undeclared zero used to read Ready=True only because 0 == 0 ready replicas: nothing reconciles
// the Kind and nobody declared that, so a gate must not pass on it.
//
// Every message is stable while the state is: SetConditions rewrites a condition whose message
// changes, so no counts and no "now" go into them (Diego, 2026-11-11). The times quoted are
// lastSeen, which changes only on a transition.
// pods lists the controller Deployment's pods. It is called only when availability was lost after a
// completed rollout (#559), so the common path reads no pods; nil means no pod information.
func controllerReadiness(cr *compositiondefinitionsv1alpha1.CompositionDefinition, kind string, dep *appsv1.Deployment, depErr error, pods func() []corev1.Pod) (ready, active rtv1.Condition) {
	if depErr != nil || dep == nil {
		why := "the controller Deployment was not found"
		if depErr != nil {
			// The caller returns depErr from Observe, so the runtime reports Ready=Unknown/NotObserved:
			// an unreadable Deployment cannot vouch for Available. The Ready returned here is not used.
			why = "the controller Deployment could not be read: " + depErr.Error()
		}
		return cdreason.Creating.WithMessage(why), cdreason.ControllerNotObserved.WithMessage(why)
	}
	replicas := int32(1) // the apiserver defaults a nil to 1
	if dep.Spec.Replicas != nil {
		replicas = *dep.Spec.Replicas
	}
	st := cr.Status.ScaleToZero
	optedIn := scaleToZeroOptedIn(cr)

	if replicas == 0 {
		if optedIn {
			msg := "idle: no instances of " + kind + "; wakes on the first"
			if st != nil && st.LastSeen != nil {
				msg = "idle: no instances of " + kind + " since " + st.LastSeen.UTC().Format(time.RFC3339) + "; wakes on the first"
			}
			return cdreason.ScaledToZero.WithMessage(msg), cdreason.ControllerScaledToZero.WithMessage(msg)
		}
		msg := "the controller has 0 replicas, but this definition has not opted into scale-to-zero: nothing reconciles " + kind
		return cdreason.Unavailable.WithMessage(msg), cdreason.ControllerUnavailable.WithMessage(msg)
	}

	msg := "controller running"
	if optedIn {
		hd, err := scaleToZeroHoldDown(cr)
		switch {
		case err != nil:
			msg = "controller running; scale-to-zero is not in effect: " + err.Error()
		case st != nil && len(st.HeldBy) > 0:
			msg = "controller running; kept running: " + strings.Join(st.HeldBy, ", ") + " not opted into scale-to-zero"
		case st != nil && st.Reason == "IdleHoldDown" && st.LastSeen != nil:
			msg = "controller running; no instances of " + kind + " since " + st.LastSeen.UTC().Format(time.RFC3339) +
				"; scales to zero at " + st.LastSeen.Add(hd).UTC().Format(time.RFC3339)
		default:
			msg = "controller running; scales to zero after " + hd.String() + " without instances"
		}
	}
	running := cdreason.ControllerRunning.WithMessage(msg)
	name := dep.Namespace + "/" + dep.Name
	switch state, why := controllerRollout(dep, replicas); state {
	case rolloutInProgress:
		return cdreason.Creating.WithMessage("the controller Deployment " + name + " is rolling out"), running
	case rolloutGaveUp:
		return cdreason.Unavailable.WithMessage("the controller Deployment " + name + " gave up its rollout: " + why), running
	case rolloutLost:
		msg := "the controller Deployment " + name + " lost availability after a completed rollout: " + why
		if pods != nil {
			if cause := podCause(pods()); cause != "" {
				msg += "; " + cause
			}
		}
		return cdreason.Unavailable.WithMessage(msg), running
	}
	return cdreason.Available.Condition(), running
}

type rolloutState int

const (
	rolloutDone rolloutState = iota
	rolloutInProgress
	// rolloutGaveUp: Progressing=False/ProgressDeadlineExceeded, the rollout did not complete.
	rolloutGaveUp
	// rolloutLost: the rollout completed and availability was lost since (an eviction, a pod that
	// started failing). The two read differently because they mean different things to an operator
	// (installer, 2026-11-16): the message names which, and the Deployment's own reason.
	rolloutLost
)

// controllerRollout is the Deployment's own verdict on its rollout, the one `kubectl rollout status`
// gives, plus the two ways it reports giving up. why is the Deployment's reason and message, for a
// failure; both are stable while the state is (the controller writes them on transitions).
//
//   - observedGeneration behind generation: the controller has not seen the spec yet. In progress.
//   - Progressing=False/ProgressDeadlineExceeded: the rollout gave up (a crash loop, an image that
//     does not pull) after progressDeadlineSeconds. Failed.
//   - old pods remain, or fewer than replicas are updated or available: in progress, UNLESS the
//     rollout already completed (Progressing reason NewReplicaSetAvailable) and availability was lost
//     since. Failed.
//   - otherwise the Available condition decides; absent is in progress (status not reported yet).
func controllerRollout(dep *appsv1.Deployment, replicas int32) (rolloutState, string) {
	if dep.Status.ObservedGeneration < dep.Generation {
		return rolloutInProgress, ""
	}
	var progressing, available *appsv1.DeploymentCondition
	for i := range dep.Status.Conditions {
		switch c := &dep.Status.Conditions[i]; c.Type {
		case appsv1.DeploymentProgressing:
			progressing = c
		case appsv1.DeploymentAvailable:
			available = c
		}
	}
	if progressing != nil && progressing.Status == corev1.ConditionFalse && progressing.Reason == deploymentProgressDeadlineExceeded {
		return rolloutGaveUp, progressing.Reason + ": " + progressing.Message
	}
	st := dep.Status
	if st.UpdatedReplicas < replicas || st.Replicas > st.UpdatedReplicas || st.AvailableReplicas < replicas {
		if progressing != nil && progressing.Reason == deploymentNewReplicaSetAvailable && st.UpdatedReplicas >= replicas && st.Replicas <= st.UpdatedReplicas {
			return rolloutLost, lostAvailability(available)
		}
		return rolloutInProgress, ""
	}
	switch {
	case available == nil:
		return rolloutInProgress, ""
	case available.Status != corev1.ConditionTrue:
		return rolloutLost, lostAvailability(available)
	}
	return rolloutDone, ""
}

// The Deployment controller's reasons (k8s.io/kubernetes pkg/controller/deployment/util), which
// k8s.io/api does not export.
const (
	deploymentProgressDeadlineExceeded = "ProgressDeadlineExceeded"
	deploymentNewReplicaSetAvailable   = "NewReplicaSetAvailable"
)

func lostAvailability(available *appsv1.DeploymentCondition) string {
	if available == nil {
		return "fewer replicas available than declared"
	}
	return available.Reason + ": " + available.Message
}

// controllerConditions reads the definition's controller Deployment and returns controllerReadiness.
// ok is false for a remote definition: ControllerActive is reported for local definitions only
// (Diego, 2026-11-11), and a remote one keeps the Ready it had. readErr is the Deployment read's error
// other than NotFound: the owner's Observe returns it, because an unreadable controller cannot vouch
// for Available (active still says why).
func (e *external) controllerConditions(ctx context.Context, cr *compositiondefinitionsv1alpha1.CompositionDefinition,
	placement compositiondefinitionsv1alpha1.ControllerStatus) (ready, active rtv1.Condition, ok bool, readErr error) {
	if clusterkube.IsRemote(cr.Spec.Deploy) {
		return rtv1.Condition{}, rtv1.Condition{}, false, nil
	}
	// The Deployment that RUNS the controller (#580): for a pool member that is the pool's
	// cdc-pool-<pool>, not <resource>-<version>-controller, which cutover retires. Reading the
	// dedicated name for a member reported Ready=False/Creating "the controller Deployment was not
	// found" for as long as it stayed in the pool.
	var dep appsv1.Deployment
	err := e.kube.Get(ctx, types.NamespacedName{Namespace: placement.Namespace, Name: placement.Deployment}, &dep)
	switch {
	case apierrors.IsNotFound(err):
		ready, active = controllerReadiness(cr, cr.Status.Kind, nil, nil, nil)
	case err != nil:
		ready, active = controllerReadiness(cr, cr.Status.Kind, nil, err, nil)
		return ready, active, true, fmt.Errorf("reading the controller Deployment: %w", err)
	default:
		ready, active = controllerReadiness(cr, cr.Status.Kind, &dep, nil, e.controllerPods(ctx, &dep))
	}
	return ready, active, true, nil
}

// applyControllerConditions writes what controllerConditions found, for an owner (asReady is the
// identity) or a follower (followerReady). A remote definition (ok false) gets fallback. An unreadable
// Deployment writes only ControllerActive and returns the error: Observe returns it, so the runtime
// reports Ready=Unknown/NotObserved instead of a Ready this pass could not establish.
func applyControllerConditions(cr *compositiondefinitionsv1alpha1.CompositionDefinition, ready, active rtv1.Condition, ok bool, readErr error,
	asReady func(rtv1.Condition) rtv1.Condition, fallback rtv1.Condition) error {
	switch {
	case !ok:
		cr.SetConditions(fallback)
	case readErr != nil:
		cr.SetConditions(active)
		return readErr
	default:
		cr.SetConditions(asReady(ready), active)
	}
	return nil
}

// controllerPods lists dep's pods by its own selector, for podCause. A failed list is no information,
// not an error: the verdict (Unavailable) is the Deployment's and stands without it.
func (e *external) controllerPods(ctx context.Context, dep *appsv1.Deployment) func() []corev1.Pod {
	return func() []corev1.Pod {
		if dep.Spec.Selector == nil {
			return nil
		}
		sel, err := metav1.LabelSelectorAsSelector(dep.Spec.Selector)
		if err != nil {
			return nil
		}
		var list corev1.PodList
		if err := e.kube.List(ctx, &list, client.InNamespace(dep.Namespace), client.MatchingLabelsSelector{Selector: sel}); err != nil {
			return nil
		}
		return list.Items
	}
}

// podCause says why a controller that rolled out is no longer available, from its pods, so a reader of
// the definition alone can tell an eviction (a replacement starting, or unschedulable) from a
// controller that came up and then failed (restarting, image pull) (#559; Diego, 2026-11-17). An
// eviction and a crash loop are identical at the Deployment level.
//
// The text must be STABLE while the state is: SetConditions rewrites a condition whose message changes,
// moving its lastTransitionTime, which installer bounds windows with. So it names the first pod that is
// not ready (by name, sorted) and a cause that does not flicker across a crash loop's cycle: a
// container that has restarted reads "restarting (last exit: <reason>)" whether it is momentarily
// running, terminated or waiting in CrashLoopBackOff, and ErrImagePull and ImagePullBackOff both read
// "image pull failing". No counts.
func podCause(pods []corev1.Pod) string {
	if pods == nil {
		return ""
	}
	if len(pods) == 0 {
		return "no pods"
	}
	sorted := append([]corev1.Pod(nil), pods...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	for i := range sorted {
		p := &sorted[i]
		if podReady(p) {
			continue
		}
		return "pod " + p.Name + ": " + notReadyCause(p)
	}
	return ""
}

func podReady(p *corev1.Pod) bool {
	if p.DeletionTimestamp != nil {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func notReadyCause(p *corev1.Pod) string {
	if p.DeletionTimestamp != nil {
		return "terminating"
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
			return "unschedulable"
		}
	}
	statuses := append(append([]corev1.ContainerStatus(nil), p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...)
	for _, cs := range statuses {
		if w := cs.State.Waiting; w != nil && (w.Reason == "ErrImagePull" || w.Reason == "ImagePullBackOff") {
			return "image pull failing"
		}
	}
	for _, cs := range statuses {
		if cs.RestartCount > 0 && !cs.Ready {
			last := "unknown"
			if t := cs.LastTerminationState.Terminated; t != nil && t.Reason != "" {
				last = t.Reason
			}
			return "restarting (last exit: " + last + ")"
		}
	}
	for _, cs := range statuses {
		if w := cs.State.Waiting; w != nil {
			switch w.Reason {
			case "CreateContainerConfigError", "CreateContainerError", "InvalidImageName", "RunContainerError":
				return w.Reason
			}
		}
	}
	if p.Status.Phase == corev1.PodPending {
		return "starting"
	}
	return "running, not ready"
}

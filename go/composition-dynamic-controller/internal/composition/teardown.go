package composition

import (
	"context"
	"fmt"
	"time"

	xcontext "github.com/krateo-platformops/unstructured-runtime/pkg/context"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// pendingChild is a managed child that has been asked to delete but is still holding its finalizer.
type pendingChild struct {
	id       string
	since    time.Time // the child's own deletionTimestamp
	hasSince bool
}

// drainFinalizerBoundChildren deletes the composition's finalizer-bearing managed children FIRST and
// reports the ones still holding their finalizer (krateo-core-provider#108).
//
// Why this must happen before `helm uninstall`:
//
// helm sorts a release's manifests by kind, and kinds it does not know go LAST — from helm's own
// kind_sorter.go, "unknown kind is last". `Secret` is a known kind at position 32 of UninstallOrder,
// while a composition's custom resources (git.krateo.io LocalResource, github.krateo.io Repository,
// ...) are unknown kinds. So a plain uninstall deletes the credential Secret BEFORE the resources
// that need those credentials to tear their own remote state down. The child's provider then cannot
// Connect, so it can never finalize:
//
//	connect failed: retrieving .toRepo username: cannot get <name>-git-username secret:
//	Secret "<name>-git-username" not found
//
// On krateo-057 that stranded 11 children of four deleted BuilderPublish claims — finalizers held for
// six days, reconcile-erroring every few seconds, with no parent left to fix them.
//
// The selector is "has finalizers", deliberately, rather than a kind list. A finalizer IS the
// resource declaring "I have work to do before I can go", so it is exactly the set that can strand;
// everything else deletes instantly and needs no ordering. It also derives the risk set from the
// live cluster instead of a hardcoded list that would rot as new providers appear — and it
// self-evidently never touches the credential Secret, which carries no finalizer and must survive
// until the children that need it are gone.
//
// FAIL-SAFE: every uncertainty degrades to today's behaviour. An unreadable status.managed, a failed
// GET, a Forbidden, a missing REST mapping — all are skipped, so the caller falls through to a plain
// uninstall. This can only ever ADD successful cleanups, never block a deletion that works today.
func (h *handler) drainFinalizerBoundChildren(ctx context.Context, dyn dynamic.Interface, mg *unstructured.Unstructured) []pendingChild {
	log := xcontext.Logger(ctx)

	managed, found, _ := unstructured.NestedSlice(mg.Object, "status", "managed")
	if !found || len(managed) == 0 {
		return nil
	}

	var pending []pendingChild
	for _, m := range managed {
		ref, ok := m.(map[string]any)
		if !ok {
			continue
		}
		apiVersion := stringField(ref, "apiVersion")
		resource := stringField(ref, "resource")
		name := stringField(ref, "name")
		namespace := stringField(ref, "namespace")
		if apiVersion == "" || resource == "" || name == "" {
			continue
		}
		gv, err := schema.ParseGroupVersion(apiVersion)
		if err != nil {
			continue
		}

		var ri dynamic.ResourceInterface = dyn.Resource(gv.WithResource(resource))
		if namespace != "" {
			ri = dyn.Resource(gv.WithResource(resource)).Namespace(namespace)
		}

		obj, err := ri.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			// Already gone (NotFound) or unreadable (Forbidden / no mapping): nothing to drain.
			continue
		}
		if len(obj.GetFinalizers()) == 0 {
			// No finalizer => deletes instantly, helm can handle it in its normal sweep. Crucially
			// this is the branch the credential Secret takes, so we never pull it out early.
			continue
		}

		p := pendingChild{id: childID(ref)}
		if ts := obj.GetDeletionTimestamp(); ts != nil {
			p.since, p.hasSince = ts.Time, true
		} else {
			// Not yet asked to delete — ask now, while its dependencies still exist.
			if err := ri.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				log.Debug("child teardown: delete failed, leaving it to helm", "child", p.id, "error", err.Error())
				continue
			}
			p.since, p.hasSince = time.Now(), true
		}
		pending = append(pending, p)
	}
	return pending
}

// drainedFor returns how long the longest-waiting pending child has been deleting. Elapsed time
// comes from the children's own deletionTimestamps — stamped by the apiserver — so the wait is
// stateless and survives a controller restart mid-teardown.
func drainedFor(pending []pendingChild, now time.Time) time.Duration {
	var longest time.Duration
	for _, p := range pending {
		if !p.hasSince {
			continue
		}
		if d := now.Sub(p.since); d > longest {
			longest = d
		}
	}
	return longest
}

// pendingIDs renders the pending children for a message, capped so a large composition does not
// produce an unreadable error.
func pendingIDs(pending []pendingChild) string {
	ids := make([]string, 0, len(pending))
	for _, p := range pending {
		ids = append(ids, p.id)
	}
	return joinCap(ids, 3)
}

// waitingForChildrenErr is returned while children are still finalizing, to requeue the composition.
func waitingForChildrenErr(pending []pendingChild, waited time.Duration) error {
	return fmt.Errorf("waiting for %d managed child(ren) to finalize before uninstalling (%s; waiting %s)",
		len(pending), pendingIDs(pending), waited.Truncate(time.Second))
}

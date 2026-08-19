package composition

import (
	"context"

	"github.com/krateo-platformops/unstructured-runtime/pkg/tools"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/util/retry"
)

// updateStatusWithRetry writes mg's status subresource, retrying on optimistic-concurrency
// conflicts. On a 409 it re-reads the object solely to pick up the current resourceVersion, then
// re-submits the status this reconcile already computed. It is a drop-in replacement for
// tools.UpdateStatus.
//
// Why this exists: tools.UpdateStatus submits mg with whatever resourceVersion it was read at and
// performs no retry, so a benign "the object has been modified" 409 aborts the whole reconcile.
// During a GVK-migration handover the retiring per-version controller and this one briefly contend
// on the same composition CR's status, making that 409 routine; without a retry the healthy
// Ready/Synced condition never lands and the composition stays wedged Ready=False until someone
// manually restarts the controller (krateo-platformops/core-provider#57). Re-submitting our status
// is safe because this controller owns the status subresource (last-writer-wins).
//
// Note: we carry mg's status as-is and refresh only its resourceVersion — we deliberately do NOT
// deep-copy the status subtree. mg.Object["status"] can hold typed values that
// runtime.DeepCopyJSONValue (used by unstructured.NestedFieldCopy) panics on.
func updateStatusWithRetry(ctx context.Context, mg *unstructured.Unstructured, opts tools.UpdateOptions) (*unstructured.Unstructured, error) {
	var result *unstructured.Unstructured
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		updated, uErr := tools.UpdateStatus(ctx, mg, opts)
		if uErr == nil {
			result = updated
			return nil
		}
		if !apierrors.IsConflict(uErr) {
			return uErr
		}
		// Conflict: re-read for a fresh resourceVersion and let RetryOnConflict re-invoke.
		// Pluralizer/DynamicClient are non-nil here — tools.UpdateStatus validates them and would
		// have returned a non-conflict error otherwise.
		gvr, gErr := opts.Pluralizer.GVKtoGVR(mg.GroupVersionKind())
		if gErr != nil {
			return gErr
		}
		latest, gErr := opts.DynamicClient.Resource(gvr).
			Namespace(mg.GetNamespace()).
			Get(ctx, mg.GetName(), metav1.GetOptions{})
		if gErr != nil {
			return gErr
		}
		mg.SetResourceVersion(latest.GetResourceVersion())
		return uErr
	})
	return result, err
}

package composition

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/krateo-platformops/unstructured-runtime/pkg/tools"
)

// isBeingDeleted re-reads the composition and reports whether it NOW carries a deletionTimestamp.
//
// #276 defect 1. Observe already refuses to self-heal a composition that is being deleted, at
// composition.go:237 — but that guard is 252 lines upstream of the self-heal apply, with a dynamic
// client build, a package fetch, two helm client builds, an HTTP call to chart-inspector and value
// injection in between. A reconcile that passed the guard before a delete was issued reaches the
// apply while the uninstall is running, and the self-heal's documented job is "creates children
// deleted out-of-band" — which is exactly what a concurrent uninstall looks like from inside it.
//
// Observed on the a0 GKE cluster: six keystone Jobs re-created 16 SECONDS AFTER the composition
// was deleted, carrying the OLD release's version labels and no helm annotations, so nothing would
// ever clean them up. Their creating field manager was "controller", not "helm", at the creation
// instant, which is what attributed them to this path rather than to a helm install.
//
// A metadata read would be cheaper than a full Get, but the dynamic client's typed Get is what
// every other re-read in this package uses (status_update.go), and introducing a second mechanism
// for one call is not worth the divergence. One Get per reconcile against a cached client is not
// the cost that matters here; the 252-line window is.
func isBeingDeleted(ctx context.Context, mg *unstructured.Unstructured, opts tools.UpdateOptions) (bool, error) {
	if opts.Pluralizer == nil || opts.DynamicClient == nil {
		return false, fmt.Errorf("no client to re-read the composition")
	}
	gvr, err := opts.Pluralizer.GVKtoGVR(mg.GroupVersionKind())
	if err != nil {
		return false, fmt.Errorf("resolving GVR: %w", err)
	}
	latest, err := opts.DynamicClient.Resource(gvr).
		Namespace(mg.GetNamespace()).
		Get(ctx, mg.GetName(), metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("re-reading composition: %w", err)
	}
	return latest.GetDeletionTimestamp() != nil, nil
}

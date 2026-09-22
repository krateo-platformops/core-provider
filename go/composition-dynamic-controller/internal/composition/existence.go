package composition

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
	"github.com/krateo-platformops/plumbing/helm/v3"
	xcontext "github.com/krateo-platformops/unstructured-runtime/pkg/context"
	"github.com/krateo-platformops/unstructured-runtime/pkg/controller"
	"github.com/krateo-platformops/unstructured-runtime/pkg/meta"

	compositionMeta "github.com/krateo-platformops/composition-dynamic-controller/pkg/meta"
)

// Exists implements controller.ExistenceChecker.
//
// Incomplete-create recovery needs one fact: did the composition's helm release get created? It
// used to obtain that from Observe, which is a poor source here — Observe also computes drift and
// runs a self-heal apply, so it returns an error whenever the chart cannot converge. Recovery read
// that as "cannot determine the creation result" and refused permanently, which wedged the
// composition AND stopped the reconcile that would have repaired the convergence failure. A
// composition missing one RBAC rule could therefore never recover, because the code that
// regenerates RBAC lives past the refusal (#110, #130).
//
// Existence is cheap and cannot fail for convergence reasons: a release either is in helm's storage
// or is not. Deliberately does no drift computation, no upgrade, no apply — a chart that cannot
// converge still has an answerable existence question, and conflating the two is the bug.
//
// A release in ANY status counts as existing, including a failed or pending one. The question is
// whether the create landed, not whether it succeeded: reporting false for a failed release would
// tell recovery to clear the pending marker and create again, on top of a release that is already
// there.
func (h *handler) Exists(ctx context.Context, mg *unstructured.Unstructured) (bool, error) {
	log := xcontext.Logger(ctx).
		WithValues("op", "Exists").
		WithValues("name", mg.GetName()).
		WithValues("namespace", mg.GetNamespace())

	// Match Observe: the release-name label may not be set yet on a composition whose first create
	// was interrupted, which is exactly the case recovery runs in.
	compositionMeta.SetReleaseName(mg, compositionMeta.CalculateReleaseName(mg, h.safeReleaseName))
	releaseName := compositionMeta.GetReleaseName(mg)

	hc, err := helm.NewClient(h.kubeconfig,
		helm.WithNamespace(mg.GetNamespace()),
		helm.WithLogger(h.getHelmLogger(meta.IsVerbose(mg))),
		helm.WithCache(),
	)
	if err != nil {
		return false, fmt.Errorf("creating helm client: %w", err)
	}
	// Value-captured, as in Observe: a helm client owns a DiskCache cleanup goroutine whose only
	// exit is Close(). Recovery runs on a requeue cadence, so leaking one per call would reproduce
	// the growth in #99.
	defer hc.Close()

	rel, err := hc.GetRelease(ctx, releaseName, &helmconfig.GetConfig{})
	if err != nil {
		// A genuine inability to answer. Propagate it: recovery keeps its conservative refusal
		// rather than clearing the pending marker on a guess and risking a duplicate release.
		return false, fmt.Errorf("finding helm release: %w", err)
	}

	log.Debug("Resolved incomplete-create existence from helm storage", "release", releaseName, "exists", rel != nil)
	return rel != nil, nil
}

// Compile-time proof that the handler still satisfies the optional interface. Without this the
// recovery path silently falls back to Observe — the exact behaviour this restores — and nothing
// would fail to build or test.
var _ controller.ExistenceChecker = (*handler)(nil)

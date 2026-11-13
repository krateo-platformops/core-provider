package compositiondefinitions

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	crdclient "github.com/krateo-platformops/core-provider/internal/tools/crd"
	"github.com/krateo-platformops/core-provider/internal/tools/crd/generation"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// #484. Adding a served version to a composition CRD is ONE write that also removes the versions
// whose prune is due, and it WAITS while a vacated version is still inside its grace window.
//
// The symptom it removes, on krateo-057 (core-provider 2.13.82, installers):
//
//	update failed: error applying or updating CRD: error applying CRD update: etcdserver: request is too large
//
// A CRD carries every served version's full schema (installers: ~600 KB each), and Update added the
// new version first and pruned stale ones only at its end. One failed step in between (there, the
// schema ConfigMap, #485) left the stale version served, and the next bump needed three versions in
// one object. With #454's 5-minute grace the same wedge needs no failure at all: a bump less than 5
// minutes after the previous one would add N+1 while N-1 is still waiting out its grace.
//
// Diego, 2026-11-13: the add waits for the grace. So the CRD never carries more than the current
// version plus one transitional version, and the grace keeps its promise to clients of a vacated
// endpoint. The cost is that a bump landing inside the grace waits for it (up to the grace plus one
// poll interval).

// vacatedBeforeAdd is the subset of the prune-eligible versions that the add may act on: every one
// EXCEPT the definition's own current version. Before the add, the definition still serves its
// instances (if any) through that version and its status still names it, so it is not vacated even
// when it has no instances; it is vacated only once the definition moves to the new version, and the
// standalone prune at the end of Update takes it from there. Counting it here would make every bump
// of a definition with no instances wait out a full grace for nothing.
func vacatedBeforeAdd(prunable []string, ownCurrent string) []string {
	out := make([]string, 0, len(prunable))
	for _, v := range prunable {
		if v != ownCurrent {
			out = append(out, v)
		}
	}
	return out
}

// servedVersionAddWaits is returned (as an error, so it cannot be ignored on the way up) when the
// add must wait for a vacated version's grace. Update and Create treat it as a successful no-op: the
// wait is the intended outcome, not a failure, and Observe states it in the Ready message.
type servedVersionAddWaits struct {
	gvr     schema.GroupVersionResource
	waiting []string
	dueAt   time.Time
}

func (w *servedVersionAddWaits) Error() string {
	return fmt.Sprintf("crd for '%s' does not serve this version yet: waiting for served version(s) %s "+
		"to finish their %s prune grace (due %s) before adding it, so the CRD never carries more than the "+
		"current version plus one transitional version",
		w.gvr.String(), strings.Join(w.waiting, ", "), servedVersionPruneGrace, w.dueAt.UTC().Format(time.RFC3339))
}

// asServedVersionAddWaits reports whether err is (or wraps) the add-waits outcome.
func asServedVersionAddWaits(err error) (*servedVersionAddWaits, bool) {
	var w *servedVersionAddWaits
	if errors.As(err, &w) {
		return w, true
	}
	return nil, false
}

// servedVersionAddPlan is what the write that adds gvk's version must do: drop the due versions, or
// not happen yet (wait != nil). The zero value is "no add, or nothing to drop".
type servedVersionAddPlan struct {
	drop map[string]bool
	wait *servedVersionAddWaits
}

// planServedVersionAdd decides the add for gvk's version on the CRD with the given plural. It records
// the grace clocks in cr's status (applyPruneGrace, the same decision Observe and the standalone prune
// take), so the clock a wait is measured against is the one that later makes the version due.
func (e *external) planServedVersionAdd(ctx context.Context, cr *compositiondefinitionsv1alpha1.CompositionDefinition, gvk schema.GroupVersionKind, plural string) (servedVersionAddPlan, error) {
	gr := schema.GroupResource{Group: gvk.Group, Resource: plural}
	live, err := crdclient.Get(ctx, e.kube, gr)
	if err != nil {
		return servedVersionAddPlan{}, fmt.Errorf("fetching CRD to plan the served-version add: %w", err)
	}
	if live == nil || generation.GVKExists(live, gvk) {
		return servedVersionAddPlan{}, nil // a create, or no version to add
	}

	own := ""
	if cur := cr.Status.CurrentGVK(); cur.Group == gvk.Group && cur.Kind == gvk.Kind {
		own = cur.Version
	}
	// prunableServedVersions lists instances through the gvr it is given, and the new version is not
	// served yet: list through a version the CRD serves, the definition's own when it is served.
	listVersion := servedListVersion(live, own)
	if listVersion == "" {
		return servedVersionAddPlan{}, nil // nothing served but vacuum: nothing to drop or wait for
	}
	prunable, _, err := e.prunableServedVersions(ctx, gvk, gr.WithVersion(listVersion), cr.Name, cr.Namespace)
	if err != nil {
		return servedVersionAddPlan{}, fmt.Errorf("evaluating served versions before adding %s: %w", gvk.Version, err)
	}
	vacated := vacatedBeforeAdd(prunable, own)
	due := applyPruneGrace(cr, vacated, time.Now())

	dueSet := map[string]bool{}
	for _, v := range due {
		dueSet[v] = true
	}
	var waiting []string
	var dueAt time.Time
	for _, p := range cr.Status.ServedVersionsPendingPrune {
		if dueSet[p.Version] {
			continue
		}
		waiting = append(waiting, p.Version)
		if at := p.EligibleSince.Add(servedVersionPruneGrace); dueAt.IsZero() || at.Before(dueAt) {
			dueAt = at
		}
	}
	if len(waiting) > 0 {
		sort.Strings(waiting)
		return servedVersionAddPlan{wait: &servedVersionAddWaits{
			gvr: gr.WithVersion(gvk.Version), waiting: waiting, dueAt: dueAt,
		}}, nil
	}
	if len(dueSet) == 0 {
		return servedVersionAddPlan{}, nil
	}
	return servedVersionAddPlan{drop: dueSet}, nil
}

// servedListVersion picks a served, non-vacuum version of live to list instances through: own when
// it is served, else the first served one, else "".
func servedListVersion(live *apiextensionsv1.CustomResourceDefinition, own string) string {
	first := ""
	for _, v := range live.Spec.Versions {
		if !v.Served || v.Name == "vacuum" {
			continue
		}
		if v.Name == own {
			return own
		}
		if first == "" {
			first = v.Name
		}
	}
	return first
}

// applyCompositionCRD applies the generated CRD for gvk on the target and, for a remote definition,
// on the hub, under the add plan: the due versions leave in the same write on both, and while a
// vacated version is inside its grace nothing is written and the add-waits outcome is returned.
//
// The hub gets the SAME drop set. syncHubCompositionCRDVersions converges the hub onto the target
// afterwards, but the hub apply runs first, and appending to a hub copy that still carries the dropped
// version would make the hub write the three-version object this exists to avoid.
func (e *external) applyCompositionCRD(ctx context.Context, cr *compositiondefinitionsv1alpha1.CompositionDefinition, gvk schema.GroupVersionKind, crd *apiextensionsv1.CustomResourceDefinition) (schema.GroupVersionResource, error) {
	plan, err := e.planServedVersionAdd(ctx, cr, gvk, crd.Spec.Names.Plural)
	if err != nil {
		return schema.GroupVersionResource{}, err
	}
	if plan.wait != nil {
		return plan.wait.gvr, plan.wait
	}
	gvr, err := crdclient.ApplyOrUpdateCRDDropping(ctx, e.kube, e.dynamic, crd, plan.drop)
	if err != nil {
		return gvr, fmt.Errorf("error applying or updating CRD: %w", err)
	}
	// For a remote CD the composition CRD must also exist on the hub, so the desired Composition can
	// be authored/validated there (it lives only on the spoke otherwise). No-op for local CDs.
	if err := e.applyHubCompositionCRD(ctx, crd, plan.drop); err != nil {
		return gvr, err
	}
	return gvr, nil
}

// settleAddWait is how Update and Create end on the CRD write's error: the add-waits outcome stops the
// operation as a SUCCESS (stop=true, nil), anything else is returned as it is (stop=false, err). One
// function so the two callers cannot handle the wait differently.
func settleAddWait(log logging.Logger, err error) (stop bool, out error) {
	if w, ok := asServedVersionAddWaits(err); ok {
		log.Info("Served-version add waits for a prune grace", "detail", w.Error())
		return true, nil
	}
	return false, err
}

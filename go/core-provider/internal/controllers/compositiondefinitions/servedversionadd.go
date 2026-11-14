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
// Diego, 2026-11-13: the add waits for the grace. So the CRD carries no more than the current
// version plus one transitional version, and the grace keeps its promise to clients of a vacated
// endpoint. The cost is that a bump landing inside the grace waits for it (up to the grace plus one
// poll interval).
//
// EXCEPT a held version (#494): a version something no grace releases still uses (a helm release
// revision helm reads, instances, another definition) is neither dropped nor waited on, so the add
// would carry it too. For a small CRD that is harmless and necessary (a parent's release holds the
// child's old version until the parent re-renders). When the write would exceed
// servedVersionAddBudget, the add is refused before anything is sent and the refusal names the holder
// (Diego, 2026-11-14).

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
	// %q, not hand-written quotes: the resource and versions come from objects read off the cluster.
	return fmt.Sprintf("crd for %q does not serve this version yet: waiting for served version(s) %q "+
		"to finish their %s prune grace (due %s) before adding it, so the CRD never carries more than the "+
		"current version plus one transitional version",
		w.gvr.String(), w.waiting, servedVersionPruneGrace, w.dueAt.UTC().Format(time.RFC3339))
}

// asServedVersionAddWaits reports whether err is (or wraps) the add-waits outcome.
func asServedVersionAddWaits(err error) (*servedVersionAddWaits, bool) {
	var w *servedVersionAddWaits
	if errors.As(err, &w) {
		return w, true
	}
	return nil, false
}

// servedVersionAddBudget is the most a CRD write that adds a version may carry, in JSON bytes, while a
// HELD version has to stay beside it (#494, Diego 2026-11-14). etcd refuses a request over 1.5 MiB
// (1,572,864 B: "etcdserver: request is too large"). The budget sits below it with margin, and is
// compared with the request's JSON size, which over-states etcd's protobuf encoding of a CRD: both
// err toward refusing. Measured on a real apiserver: two ~560 KB versions (1.12 MB) are accepted and a
// third is refused (crd.TestApplyOrUpdateCRDDroppingKeepsTheWriteUnderTheEtcdLimitEnvtest).
const servedVersionAddBudget = 1_400_000

// servedVersionAddTooLarge is returned (as an error, like the wait) when the add would keep a held,
// non-current served version and the write would exceed servedVersionAddBudget (#494). Nothing is
// written: the write would only be refused by etcd. It names each held version and what holds it (a
// helm release revision, instances, another definition), because the add can proceed only once that
// holder lets go, which no prune grace will do. Update and Create treat it like the wait: a
// successful no-op, retried on the next poll; Observe states it in the Ready message.
type servedVersionAddTooLarge struct {
	gvr    schema.GroupVersionResource
	bytes  int
	budget int
	held   []compositiondefinitionsv1alpha1.HeldVersion
}

func (e *servedVersionAddTooLarge) Error() string {
	holders := make([]string, 0, len(e.held))
	for _, h := range e.held {
		holders = append(holders, fmt.Sprintf("%s held by %s", h.Version, h.Reason))
	}
	return fmt.Sprintf("crd for %q does not serve this version yet: adding it would make the CRD write %d bytes, "+
		"over the %d-byte budget below etcd's request limit, because served version(s) that must stay are held: %s. "+
		"The add proceeds once they are released",
		e.gvr.String(), e.bytes, e.budget, strings.Join(holders, "; "))
}

// asServedVersionAddTooLarge reports whether err is (or wraps) the too-large refusal.
func asServedVersionAddTooLarge(err error) (*servedVersionAddTooLarge, bool) {
	var r *servedVersionAddTooLarge
	if errors.As(err, &r) {
		return r, true
	}
	return nil, false
}

// servedVersionAddPlan is what the write that adds gvk's version must do: drop the due versions, or
// not happen yet (wait != nil: a vacated version's grace; tooLarge != nil: a held version and no room,
// #494). The zero value is "no add, or nothing to drop".
type servedVersionAddPlan struct {
	drop     map[string]bool
	wait     *servedVersionAddWaits
	tooLarge *servedVersionAddTooLarge
}

// refusal is the plan's no-write outcome, nil when the write may go ahead.
func (p servedVersionAddPlan) refusal() error {
	switch {
	case p.wait != nil:
		return p.wait
	case p.tooLarge != nil:
		return p.tooLarge
	}
	return nil
}

// planServedVersionAdd decides the add for gvk's version on the CRD with the given plural. It records
// the grace clocks in cr's status (applyPruneGrace, the same decision Observe and the standalone prune
// take), so the clock a wait is measured against is the one that later makes the version due.
//
// crd is the generated CRD for gvk, the object the write would add: with a held version that has to
// stay, the write it would make is measured against servedVersionAddBudget (#494).
func (e *external) planServedVersionAdd(ctx context.Context, cr *compositiondefinitionsv1alpha1.CompositionDefinition, gvk schema.GroupVersionKind, crd *apiextensionsv1.CustomResourceDefinition) (servedVersionAddPlan, error) {
	gr := schema.GroupResource{Group: gvk.Group, Resource: crd.Spec.Names.Plural}
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
	prunable, kept, err := e.prunableServedVersions(ctx, gvk, gr.WithVersion(listVersion), cr.Name, cr.Namespace)
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
	// #494: a version held by something no grace will release (a helm release revision helm reads,
	// instances, another definition) stays beside the new one. When that write would exceed the
	// budget, refuse it and name the holders instead of sending a write etcd would reject. A small CRD
	// (a parent/child bump) is unaffected: there three versions are harmless and the holder lets go
	// once the parent re-renders.
	if held := heldBeside(live, own, gvk.Version, dueSet, heldVersions(kept)); len(held) > 0 {
		size, err := crdclient.ProjectedWriteBytes(live, crd, dueSet)
		if err != nil {
			return servedVersionAddPlan{}, fmt.Errorf("measuring the write that adds %s: %w", gvk.Version, err)
		}
		if size > servedVersionAddBudget {
			return servedVersionAddPlan{tooLarge: &servedVersionAddTooLarge{
				gr.WithVersion(gvk.Version), size, servedVersionAddBudget, held}}, nil
		}
	}
	if len(dueSet) == 0 {
		return servedVersionAddPlan{}, nil
	}
	return servedVersionAddPlan{drop: dueSet}, nil
}

// heldBeside is the held versions that would stay served beside the new version: served, not vacuum,
// not the definition's own current version, not the version being added, not dropped by this write,
// and held (a version the prune keeps for a reason; "*" when unaccounted instances hold every
// version). Each keeps the reason it is held for.
func heldBeside(live *apiextensionsv1.CustomResourceDefinition, own, adding string, drop map[string]bool, held []compositiondefinitionsv1alpha1.HeldVersion) []compositiondefinitionsv1alpha1.HeldVersion {
	reason := map[string]string{}
	all := ""
	for _, h := range held {
		if h.Version == "*" {
			all = h.Reason
			continue
		}
		reason[h.Version] = h.Reason
	}
	var out []compositiondefinitionsv1alpha1.HeldVersion
	for _, v := range live.Spec.Versions {
		if !v.Served || v.Name == "vacuum" || v.Name == own || v.Name == adding || drop[v.Name] {
			continue
		}
		r, ok := reason[v.Name]
		if !ok && all != "" {
			r, ok = all, true
		}
		if ok {
			out = append(out, compositiondefinitionsv1alpha1.HeldVersion{Version: v.Name, Reason: r})
		}
	}
	return out
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
	plan, err := e.planServedVersionAdd(ctx, cr, gvk, crd)
	if err != nil {
		return schema.GroupVersionResource{}, err
	}
	if r := plan.refusal(); r != nil {
		return schema.GroupVersionResource{Group: gvk.Group, Version: gvk.Version, Resource: crd.Spec.Names.Plural}, r
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
	if r, ok := asServedVersionAddTooLarge(err); ok {
		log.Info("Served-version add refused: a held version must stay and the write would not fit", "detail", r.Error())
		return true, nil
	}
	return false, err
}

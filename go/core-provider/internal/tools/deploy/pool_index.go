package deploy

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/krateo-platformops/core-provider/internal/tools/auditrec"
	contexttools "github.com/krateo-platformops/core-provider/internal/tools/context"
	kubecli "github.com/krateo-platformops/core-provider/internal/tools/kube"
	"github.com/krateo-platformops/core-provider/internal/tools/objects/templates"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

// WHO IS IN A POOL (core-provider#652, docs/design/cdc-pooling.md §5c "Membership, as built").
//
// A pool's index lists every controller due in it. What says a controller is due is its env ConfigMap
// (<resource>-<version>-configmap): Deploy writes it, before the controller runs, and Undeploy removes
// it, after the controller has stopped. So a member is a live env ConfigMap carrying the pool's label,
// and the index is recomputed from those, in one LIST, on every write. Nothing else is asked:
//
//   - not the definitions' status, which the runtime writes only after the reconcile that deploys
//     returns, so during a bump it still names the old version and after a Create it names none;
//   - not the definitions' spec, which says nothing about an older version kept beside the current one
//     (upgradePolicy Manual or Paused).
//
// Every version that still has a controller is therefore indexed, from its first deploy to its
// retirement, whatever has or has not been written to a status.
//
// LEAVING is stop-before-revoke, the pool's form of #263. The ConfigMap is marked draining (so it is
// no longer a member, and a restarted core-provider still knows the controller is on its way out), the
// key is dropped from the index, the pool's process drains that controller and releases its ownership
// Lease, and only then is anything it runs with removed or given to another identity.

// PoolDrainingAnnotation marks a member's env ConfigMap whose controller is leaving its pool. Deploy's
// next write of the ConfigMap clears it; Undeploy deletes the ConfigMap.
const PoolDrainingAnnotation = "krateo.io/cdc-pool-draining"

// The env ConfigMap's own account of the Kind x version it configures, rendered for a pool member only
// (assets/cdc/configmap.yaml). The cdc reads the same keys (internal/manager, MemberFromConfig).
const (
	memberKeyGroup    = "COMPOSITION_CONTROLLER_GROUP"
	memberKeyVersion  = "COMPOSITION_CONTROLLER_VERSION"
	memberKeyResource = "COMPOSITION_CONTROLLER_RESOURCE"
	memberKeyLease    = "COMPOSITION_CONTROLLER_LEASE_NAME"
)

var leaseGVR = schema.GroupVersionResource{Group: "coordination.k8s.io", Version: "v1", Resource: "leases"}

// poolView is one pool as one LIST of its ConfigMaps shows it.
type poolView struct {
	index    *unstructured.Unstructured // nil: there is none
	members  []PoolMember               // sorted by key
	draining []PoolMember               // sorted by key
}

func (v poolView) indexData() map[string]string {
	if v.index == nil {
		return nil
	}
	data, _, _ := unstructured.NestedStringMap(v.index.Object, "data")
	return data
}

// find returns gvr's env ConfigMap in the pool, whether a member or draining.
func (v poolView) find(gvr schema.GroupVersionResource) (m PoolMember, draining, ok bool) {
	for _, m := range v.members {
		if m.GVR == gvr {
			return m, false, true
		}
	}
	for _, m := range v.draining {
		if m.GVR == gvr {
			return m, true, true
		}
	}
	return PoolMember{}, false, false
}

// envNames are the env ConfigMaps the pool's process may read: its members', and a draining one's until
// it is removed.
func (v poolView) envNames() []string {
	var out []string
	for _, m := range append(append([]PoolMember{}, v.members...), v.draining...) {
		out = append(out, MemberEnvConfigMapName(m.GVR))
	}
	sort.Strings(out)
	return out
}

// listPools reads every pool in namespace from ONE list, so a view's index and members are of the same
// moment. A failed list is an error and never an empty pool.
func listPools(ctx context.Context, dyn dynamic.Interface, namespace string) (map[string]poolView, error) {
	list, err := dyn.Resource(configMapGVR).Namespace(namespace).List(ctx, metav1.ListOptions{LabelSelector: PoolLabel})
	if err != nil {
		return nil, fmt.Errorf("listing the pools' ConfigMaps in %s: %w", namespace, err)
	}
	log := contexttools.LoggerFromCtx(ctx, logging.NewNopLogger())
	out := map[string]poolView{}
	for i := range list.Items {
		cm := &list.Items[i]
		pool := cm.GetLabels()[PoolLabel]
		if pool == "" {
			continue
		}
		v := out[pool]
		names := PoolNames(pool)
		switch {
		case cm.GetName() == names.Index:
			v.index = cm
		case cm.GetName() == names.EnvConfigMap: // the process's own
		default:
			m, ok := memberOf(cm)
			if !ok {
				log.Info("A ConfigMap carries a pool's label and is not a member's env ConfigMap; ignored", "configmap", namespace+"/"+cm.GetName(), "pool", pool)
				break
			}
			if cm.GetAnnotations()[PoolDrainingAnnotation] != "" {
				v.draining = append(v.draining, m)
			} else {
				v.members = append(v.members, m)
			}
		}
		out[pool] = v
	}
	for p, v := range out {
		for _, ms := range [][]PoolMember{v.members, v.draining} {
			sort.Slice(ms, func(a, b int) bool { return MemberKey(ms[a].GVR) < MemberKey(ms[b].GVR) })
		}
		out[p] = v
	}
	return out, nil
}

// memberOf reads a member from its env ConfigMap. ok is false for a ConfigMap that does not name a Kind
// x version, or that is not named for the one it names.
func memberOf(cm *unstructured.Unstructured) (PoolMember, bool) {
	data, _, err := unstructured.NestedStringMap(cm.Object, "data")
	if err != nil {
		return PoolMember{}, false
	}
	gvr := schema.GroupVersionResource{Group: data[memberKeyGroup], Version: data[memberKeyVersion], Resource: data[memberKeyResource]}
	if gvr.Version == "" || gvr.Resource == "" || cm.GetName() != MemberEnvConfigMapName(gvr) {
		return PoolMember{}, false
	}
	return PoolMember{GVR: gvr, EnvConfig: data}, true
}

// poolLocks serialises this process's writes to one pool: the reconciles of its members run in parallel
// workers, and each recomputes the whole pool. Between processes (an old and a new core-provider
// without leader election) the index's resourceVersion does it, in syncPoolIndex.
var poolLocks sync.Map

func lockPool(namespace, pool string) (unlock func()) {
	v, _ := poolLocks.LoadOrStore(namespace+"/"+pool, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

const poolIndexAttempts = 20

// syncPoolIndex makes pool's index what its members are now. The caller holds the pool's lock. The index is computed from the same LIST that gave its resourceVersion, and a
// write that is refused because something changed starts again from a new LIST: a computed index is
// never retried.
//
// The pool's Role is written first, from the same LIST: whoever recomputes the index may be the first to
// list a member another reconcile has just deployed, and a member the index names must be readable.
func syncPoolIndex(ctx context.Context, dyn dynamic.Interface, namespace, pool string) error {
	for attempt := 0; ; attempt++ {
		pools, err := listPools(ctx, dyn, namespace)
		if err != nil {
			return err
		}
		v := pools[pool]
		role, binding := PoolRBAC(pool, namespace, v.envNames())
		if _, err := applyRecorded(ctx, dyn, roleGVR, "Role", auditrec.ActionRBACApply, &role, kubecli.ApplyOptions{}); err != nil {
			return fmt.Errorf("pool %s: applying its Role: %w", pool, err)
		}
		if _, err := applyRecorded(ctx, dyn, roleBindingGVR, "RoleBinding", auditrec.ActionRBACApply, &binding, kubecli.ApplyOptions{}); err != nil {
			return fmt.Errorf("pool %s: applying its RoleBinding: %w", pool, err)
		}
		err = writePoolIndex(ctx, dyn, namespace, pool, v)
		if err == nil {
			return nil
		}
		retry := apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) || apierrors.IsNotFound(err)
		if !retry || attempt+1 == poolIndexAttempts {
			return fmt.Errorf("pool %s: writing its membership index: %w", pool, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 10 * time.Millisecond):
		}
	}
}

func writePoolIndex(ctx context.Context, dyn dynamic.Interface, namespace, pool string, v poolView) error {
	index, err := RenderPoolIndex(pool, namespace, v.members)
	if err != nil {
		return err
	}
	if v.index != nil && sameData(v.indexData(), index.Data) {
		return nil
	}
	ri := dyn.Resource(configMapGVR).Namespace(namespace)
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": index.Name, "namespace": namespace},
	}}
	u.SetLabels(index.Labels)
	if err := unstructured.SetNestedStringMap(u.Object, index.Data, "data"); err != nil {
		return err
	}
	if v.index == nil {
		_, err = ri.Create(ctx, u, metav1.CreateOptions{})
		return err
	}
	u.SetResourceVersion(v.index.GetResourceVersion())
	_, err = ri.Update(ctx, u, metav1.UpdateOptions{})
	return err
}

func sameData(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || v != w {
			return false
		}
	}
	return true
}

// PoolMemberVersions are the versions of group/resource whose controller runs in a pool of namespace, or
// is draining in one.
func PoolMemberVersions(ctx context.Context, dyn dynamic.Interface, namespace, group, resource string) ([]string, error) {
	pools, err := listPools(ctx, dyn, namespace)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, v := range pools {
		for _, m := range append(append([]PoolMember{}, v.members...), v.draining...) {
			if m.GVR.Group == group && m.GVR.Resource == resource {
				out = append(out, m.GVR.Version)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// ErrPoolMemberNotDrained is returned when a controller that was dropped from its pool's index still
// holds its ownership Lease at the bound: its identity and configuration are kept, and the caller fails,
// to be retried.
var ErrPoolMemberNotDrained = errors.New("the pool's controller has not released its ownership Lease")

// releasePoolMember stops gvr's controller in every pool of namespace that runs it, except keep, and
// waits until it has stopped: the mark, the index, then the Lease (see the top of this file). left is
// the pools it was dropped from. Nothing is done for a controller no pool runs.
//
// It can be re-run at any point. The mark outlives a restart, so a later call waits for the Lease again
// rather than taking a key already dropped for a controller already stopped. timings are the ones the
// controllers run with (#660): an invalid set, zero included, is an error before anything is changed.
func releasePoolMember(ctx context.Context, dyn dynamic.Interface, namespace string, gvr schema.GroupVersionResource, keep string, timeout time.Duration, timings OwnershipLeaseTimings) (left []string, err error) {
	if err := timings.Validate(); err != nil {
		return nil, fmt.Errorf("releasing %s from its pool: %w", gvr, err)
	}
	pools, err := listPools(ctx, dyn, namespace)
	if err != nil {
		return nil, err
	}
	key := MemberKey(gvr)
	// The controller was stopped where its ConfigMap says it ran. With the ConfigMap in keep, an entry in
	// another index is a leftover to drop: the Lease may rightly be held by keep's member.
	_, _, stays := pools[keep].find(gvr)
	stays = stays && keep != ""
	for pool, v := range pools {
		if pool == keep {
			continue
		}
		_, _, found := v.find(gvr)
		if _, listed := v.indexData()[key]; !found && !listed {
			continue
		}
		left = append(left, pool)
	}
	sort.Strings(left)
	for _, pool := range left {
		if err := dropFromPool(ctx, dyn, namespace, pool, gvr); err != nil {
			return left, err
		}
	}
	if len(left) == 0 || stays {
		return left, nil
	}
	if err := waitLeaseReleased(ctx, dyn, types.NamespacedName{Namespace: namespace, Name: templates.ControllerLeaseName(gvr.Group, gvr.Version, gvr.Resource)}, timeout, timings); err != nil {
		return left, fmt.Errorf("%s left pool %v: %w", gvr, left, err)
	}
	return left, nil
}

func dropFromPool(ctx context.Context, dyn dynamic.Interface, namespace, pool string, gvr schema.GroupVersionResource) error {
	defer lockPool(namespace, pool)()
	patch := []byte(`{"metadata":{"annotations":{"` + PoolDrainingAnnotation + `":"true"}}}`)
	ri := dyn.Resource(configMapGVR).Namespace(namespace)
	cm, err := ri.Get(ctx, MemberEnvConfigMapName(gvr), metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err): // an index entry without its ConfigMap
	case err != nil:
		return fmt.Errorf("pool %s: reading %s's env ConfigMap: %w", pool, gvr, err)
	case cm.GetLabels()[PoolLabel] == pool && cm.GetAnnotations()[PoolDrainingAnnotation] == "":
		if _, err := ri.Patch(ctx, cm.GetName(), types.MergePatchType, patch, metav1.PatchOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("pool %s: marking %s as leaving: %w", pool, gvr, err)
		}
	}
	return syncPoolIndex(ctx, dyn, namespace, pool)
}

// MemberDrainTimeout bounds the wait for a member's Lease: the pool process's own drain (30s) and its
// Lease release, inside the pod's terminationGracePeriodSeconds (45s), plus the margin a dedicated
// controller's drain gets. A variable so a test can shorten it.
var MemberDrainTimeout = 45*time.Second + drainTimeoutMargin

// memberDrainBound is the wait for a member's Lease when the caller sets none: MemberDrainTimeout, or,
// when the timings are longer than that covers, a full G (a holder that died without releasing is seen
// stopped a leaseDuration plus Quiet after this process first saw its record) plus the margin. A bound
// below G would never see such a Lease released, on any retry, since each call measures afresh (#660).
func memberDrainBound(t OwnershipLeaseTimings) time.Duration {
	return max(MemberDrainTimeout, t.MemberGrace()+drainTimeoutMargin)
}

// waitLeaseReleased waits until nobody holds lease. Held means a holder is named and its record still
// changes: a record unchanged for its own leaseDurationSeconds belongs to a holder that died without
// releasing it, and the next controller would take it. That is client-go's rule, and it is measured
// here the same way, on this process's clock from when it first saw the record, so no clock is compared.
// Released means unheld for timings.Quiet: longer than the elector's longest wait between attempts, so a
// second pool pod (a rolling update), or a member that had only been waiting, has either taken it by
// then, and is waited for in turn, or has seen the key leave the index.
//
// At the bound it returns ErrPoolMemberNotDrained: a Lease still held is never read as released. A Lease
// this process may not read is ErrDrainUnobservable, for #270's reason: waiting never grants it.
func waitLeaseReleased(ctx context.Context, dyn dynamic.Interface, lease types.NamespacedName, timeout time.Duration, timings OwnershipLeaseTimings) error {
	if timeout <= 0 {
		timeout = memberDrainBound(timings)
	}
	quiet := timings.Quiet()
	deadline := time.Now().Add(timeout)
	var record, holder string
	var seen, free time.Time // when this record was first seen; since when nobody holds it
	for {
		now := time.Now()
		l, err := dyn.Resource(leaseGVR).Namespace(lease.Namespace).Get(ctx, lease.Name, metav1.GetOptions{})
		held := false
		switch {
		case apierrors.IsNotFound(err):
		case apierrors.IsForbidden(err):
			return fmt.Errorf("%w: ownership Lease %s: %w", ErrDrainUnobservable, lease, err)
		case err != nil:
			return fmt.Errorf("reading the ownership Lease %s: %w", lease, err)
		default:
			holder, _, _ = unstructured.NestedString(l.Object, "spec", "holderIdentity")
			secs, ok, _ := unstructured.NestedInt64(l.Object, "spec", "leaseDurationSeconds")
			if !ok || secs <= 0 {
				secs = int64(timings.Duration / time.Second)
			}
			if r := l.GetResourceVersion(); r != record {
				record, seen = r, now
			}
			held = holder != "" && now.Sub(seen) < time.Duration(secs)*time.Second
		}
		switch {
		case held:
			free = time.Time{}
		case free.IsZero():
			free = now
		}
		if !held && now.Sub(free) >= quiet {
			return nil
		}
		if now.After(deadline) {
			if !held {
				holder = "nobody for less than " + quiet.String()
			}
			return fmt.Errorf("%w: %s is held by %s after %s; its env ConfigMap, RBAC and Lease grant are kept", ErrPoolMemberNotDrained, lease, holder, timeout)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: %s: %w", ErrPoolMemberNotDrained, lease, ctx.Err())
		case <-time.After(drainPollInterval):
		}
	}
}

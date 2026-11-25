//go:build envtest

package compositiondefinitions

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/tools/deploy"
	rtv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// core-provider#652 on a real apiserver, through provider-runtime's reconciler and the real connector
// (env644): a pool's index names every controller due in it, through a bump, a Create, a retirement, a
// restart and a delete.
//
// Everything is judged from outside. invariant652 watches every object the claim is about and replays
// the persisted states in order, so a state that lasted one write counts. leases652 stands in for the
// controllers: the pool's process (it reads the index and each member's env ConfigMap, and takes and
// releases each member's ownership Lease) and the dedicated ones.

const (
	ns652   = self642NS
	group   = "composition.krateo.io"
	grace52 = 14 * time.Second // deploy.MemberDrainTimeout here
)

var (
	cmGVR652   = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	depGVR652  = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	roleGVR652 = schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}
)

type env652 struct {
	*env644
	t      *testing.T
	leases *leases652
	inv    *invariant652
	defs   []*cd644
}

func start652(t *testing.T) *env652 {
	t.Helper()
	e := &env652{env644: start644(t), t: t}
	set := func(p *string, v string) {
		old := *p
		*p = v
		t.Cleanup(func() { *p = old })
	}
	root := t.TempDir()
	if _, err := renderChartAssets(root, ns652, "ghcr.io/krateo-platformops/composition-dynamic-controller:652-test"); err != nil {
		t.Fatal(err)
	}
	set(&PoolConfigmapTemplatePath, filepath.Join(root, "cdc-configmap", "pool-configmap.yaml"))
	set(&PoolDeploymentTemplatePath, filepath.Join(root, "cdc-deployment", "pool-deployment.yaml"))
	set(&PoolSizingPath, filepath.Join(root, "cdc-deployment", "pool-sizing.json"))
	was := deploy.MemberDrainTimeout
	deploy.MemberDrainTimeout = grace52
	t.Cleanup(func() { deploy.MemberDrainTimeout = was })
	e.leases = startLeases652(t, e.c)
	e.inv = startInvariant652(t, e.c)
	return e
}

// member defines chart name at 1.0.0 in pool ("": a dedicated controller), not yet reconciled.
func (e *env652) member(name, pool string, edits ...func(*compositiondefinitionsv1alpha1.CompositionDefinition)) *cd644 {
	e.t.Helper()
	e.charts.add(e.t, name, "1.0.0", `{"type":"object"}`)
	d := e.define(e.t, name, name, func(cd *compositiondefinitionsv1alpha1.CompositionDefinition) {
		setPool(cd, pool)
		for _, f := range edits {
			f(cd)
		}
	})
	e.defs = append(e.defs, d)
	return d
}

func setPool(cd *compositiondefinitionsv1alpha1.CompositionDefinition, pool string) {
	if pool == "" {
		cd.Spec.Controller = nil
		return
	}
	cd.Spec.Controller = &compositiondefinitionsv1alpha1.ControllerConfig{Pool: &pool}
}

func (e *env652) gvr(d *cd644, version string) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: group, Version: version, Resource: d.cd().Status.Resource}
}

func (e *env652) key(d *cd644, version string) string { return deploy.MemberKey(e.gvr(d, version)) }

// instance creates a composition of d at version, labelled as the admission policy labels it, and has
// the invariant hold for it from now on.
func (e *env652) instance(d *cd644, version string) {
	e.t.Helper()
	gvr := e.gvr(d, version)
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(group + "/" + version)
	u.SetKind(d.cd().Status.Kind)
	u.SetName("one")
	u.SetNamespace(self642CDNS)
	u.SetLabels(map[string]string{
		deploy.CompositionVersionLabel:             version,
		deploy.CompositionDefinitionNameLabel:      d.key.Name,
		deploy.CompositionDefinitionNamespaceLabel: d.key.Namespace,
	})
	got, err := e.c.raw.Resource(gvr).Namespace(self642CDNS).Create(context.Background(), u, metav1.CreateOptions{})
	if err != nil {
		e.t.Fatal(err)
	}
	e.inv.follow(gvr, got.GetResourceVersion())
}

// index is pool's index, sorted; nil when there is none.
func (e *env652) index(pool string) []string {
	e.t.Helper()
	cm := &corev1.ConfigMap{}
	err := e.c.cl.Get(context.Background(), types.NamespacedName{Namespace: ns652, Name: deploy.PoolNames(pool).Index}, cm)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		e.t.Fatal(err)
	}
	return deploy.PoolIndexMembers(*cm)
}

func (e *env652) wantIndex(when, pool string, want ...string) {
	e.t.Helper()
	sort.Strings(want)
	if got := e.index(pool); !reflect.DeepEqual(got, want) && len(got)+len(want) > 0 {
		e.t.Errorf("%s: pool %s's index = %v, want %v", when, pool, got, want)
	}
}

func (e *env652) exists(o client.Object, name string) bool {
	e.t.Helper()
	err := e.c.cl.Get(context.Background(), types.NamespacedName{Namespace: ns652, Name: name}, o)
	if err != nil && !apierrors.IsNotFound(err) {
		e.t.Fatal(err)
	}
	return err == nil && o.GetDeletionTimestamp() == nil
}

// grants are what gvr's controller runs with: gone only once it has stopped.
func (e *env652) grants(key string) map[string]bool {
	return map[string]bool{
		"env ConfigMap": e.exists(&corev1.ConfigMap{}, key+"-configmap"),
		"lease Role":    e.exists(&rbacv1.Role{}, key+"-lease"),
		"ClusterRoleBinding": func() bool {
			err := e.c.cl.Get(context.Background(), types.NamespacedName{Name: key}, &rbacv1.ClusterRoleBinding{})
			return err == nil
		}(),
	}
}

func (e *env652) wantGrants(when, key string, want bool) {
	e.t.Helper()
	for what, got := range e.grants(key) {
		if got != want {
			e.t.Errorf("%s: %s's %s present = %v, want %v", when, key, what, got, want)
		}
	}
}

// settle reconciles every definition, workers at a time as the manager does, until all are at rest at
// their spec's version, at most rounds times each.
func (e *env652) settle(workers, rounds int, defs ...*cd644) {
	e.t.Helper()
	if len(defs) == 0 {
		defs = e.defs
	}
	work := make(chan *cd644)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for d := range work {
				_, _ = d.pass()
			}
		}()
	}
	for range rounds {
		for _, d := range defs {
			work <- d
		}
	}
	close(work)
	wg.Wait()
	for _, d := range defs {
		d.rest(4)
	}
}

// ---- the controllers ----

// leases652 holds the ownership Leases as the controllers would. The pool's process runs a member while
// its index names it and its env ConfigMap can be read; a dedicated controller runs while its Deployment
// exists. Whoever runs takes the Lease when nobody holds it, and on stopping drains for drain and then
// releases it, or, for a key in stuck, never does.
type leases652 struct {
	t  *testing.T
	c  *cluster642
	mu sync.Mutex
	// drain and stuck: see above.
	drain time.Duration
	stuck map[string]bool
	// unread are the index entries whose env ConfigMap the pool's process could not read: a member that
	// would restart for ever, and fail its pool's readiness with it.
	unread map[string]string
	since  map[string]time.Time // when a holder was asked to stop
}

func startLeases652(t *testing.T, c *cluster642) *leases652 {
	l := &leases652{t: t, c: c, stuck: map[string]bool{}, unread: map[string]string{}, since: map[string]time.Time{}}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case <-time.After(50 * time.Millisecond):
				l.step()
			}
		}
	}()
	t.Cleanup(func() { close(stop); <-done })
	return l
}

func (l *leases652) set(drain time.Duration, stuck ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.drain, l.stuck = drain, map[string]bool{}
	for _, k := range stuck {
		l.stuck[k] = true
	}
}

func (l *leases652) step() {
	ctx := context.Background()
	cms, err := l.c.cs.CoreV1().ConfigMaps(ns652).List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}
	deps, err := l.c.cs.AppsV1().Deployments(ns652).List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}
	env := map[string]corev1.ConfigMap{}
	for _, cm := range cms.Items {
		if strings.HasSuffix(cm.Name, "-configmap") {
			env[cm.Name] = cm
		}
	}
	runs := map[string]string{} // lease -> who should hold it
	leaseOfKey := func(cm corev1.ConfigMap) string { return cm.Data["COMPOSITION_CONTROLLER_LEASE_NAME"] }
	for _, d := range deps.Items {
		if key, ok := strings.CutSuffix(d.Name, "-controller"); ok && d.DeletionTimestamp == nil {
			if cm, ok := env[key+"-configmap"]; ok {
				runs[leaseOfKey(cm)] = d.Name
			}
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, cm := range cms.Items {
		pool, ok := strings.CutSuffix(strings.TrimPrefix(cm.Name, "cdc-pool-"), "-members")
		if !ok || !strings.HasPrefix(cm.Name, "cdc-pool-") {
			continue
		}
		for key, raw := range cm.Data {
			var entry deploy.PoolIndexEntry
			_ = json.Unmarshal([]byte(raw), &entry)
			m, ok := env[entry.ConfigMap]
			if !ok {
				// Read again: the list above is one moment, and this is a later one.
				if _, err := l.c.cs.CoreV1().ConfigMaps(ns652).Get(ctx, entry.ConfigMap, metav1.GetOptions{}); apierrors.IsNotFound(err) {
					if cur, err := l.c.cs.CoreV1().ConfigMaps(ns652).Get(ctx, cm.Name, metav1.GetOptions{}); err == nil && cur.Data[key] != "" {
						l.unread[key] = entry.ConfigMap
					}
				}
				continue
			}
			if lease := leaseOfKey(m); runs[lease] == "" {
				runs[lease] = "cdc-pool-" + pool + "-0"
			}
		}
	}
	held, err := l.c.cs.CoordinationV1().Leases(ns652).List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}
	have := map[string]*coordinationv1.Lease{}
	for i := range held.Items {
		have[held.Items[i].Name] = &held.Items[i]
	}
	for name, lease := range have {
		holder := ""
		if lease.Spec.HolderIdentity != nil {
			holder = *lease.Spec.HolderIdentity
		}
		if holder == "" || holder == runs[name] {
			delete(l.since, name)
			if holder != "" {
				l.write(ctx, lease, holder)
			}
			continue
		}
		if l.since[name].IsZero() {
			l.since[name] = time.Now()
		}
		if l.stuck[name] || time.Since(l.since[name]) < l.drain {
			l.write(ctx, lease, holder) // still running: draining, and renewing
			continue
		}
		l.write(ctx, lease, "")
	}
	for name, who := range runs {
		if lease := have[name]; lease == nil {
			now := metav1.NowMicro()
			secs := int32(15)
			_, _ = l.c.cs.CoordinationV1().Leases(ns652).Create(ctx, &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns652},
				Spec: coordinationv1.LeaseSpec{HolderIdentity: &who, LeaseDurationSeconds: &secs, RenewTime: &now}}, metav1.CreateOptions{})
		} else if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity == "" {
			l.write(ctx, lease, who)
		}
	}
}

func (l *leases652) write(ctx context.Context, lease *coordinationv1.Lease, holder string) {
	now := metav1.NowMicro()
	out := lease.DeepCopy()
	out.Spec.HolderIdentity, out.Spec.RenewTime = &holder, &now
	_, _ = l.c.cs.CoordinationV1().Leases(ns652).Update(ctx, out, metav1.UpdateOptions{})
}

func (l *leases652) holder(gvr schema.GroupVersionResource) string {
	lease, err := l.c.cs.CoordinationV1().Leases(ns652).Get(context.Background(), gvr.Version+"."+gvr.Resource+"."+gvr.Group, metav1.GetOptions{})
	if err != nil || lease.Spec.HolderIdentity == nil {
		return ""
	}
	return *lease.Spec.HolderIdentity
}

func (l *leases652) wantHolder(t *testing.T, gvr schema.GroupVersionResource, want string) {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if l.holder(gvr) == want {
			return
		}
	}
	t.Errorf("%s's Lease is held by %q, want %q", deploy.MemberKey(gvr), l.holder(gvr), want)
}

// ---- the invariant ----

// invariant652 replays every persisted state of what decides where a controller runs.
type invariant652 struct {
	t    *testing.T
	c    *cluster642
	mu   sync.Mutex
	evs  []event652
	stop []func()
	kind map[string]bool // resources followed
}

type event652 struct {
	rv      int64
	gvr     schema.GroupVersionResource
	deleted bool
	obj     *unstructured.Unstructured
}

func startInvariant652(t *testing.T, c *cluster642) *invariant652 {
	i := &invariant652{t: t, c: c, kind: map[string]bool{}}
	for _, w := range []struct {
		gvr schema.GroupVersionResource
		ns  string
	}{{cmGVR652, ns652}, {depGVR652, ns652}, {roleGVR652, ns652}, {cdGVR644, self642CDNS}} {
		i.watch(w.gvr, w.ns, "")
	}
	t.Cleanup(func() {
		for _, f := range i.stop {
			f()
		}
	})
	return i
}

// follow has the instances of gvr's Kind watched from rv on, whatever version serves them.
func (i *invariant652) follow(gvr schema.GroupVersionResource, rv string) {
	if i.kind[gvr.Resource] {
		return
	}
	i.kind[gvr.Resource] = true
	n, _ := strconv.ParseInt(rv, 10, 64)
	i.watch(gvr, self642CDNS, strconv.FormatInt(n-1, 10))
}

// watch records every event of gvr in ns. A watch the apiserver closes is resumed from the last event,
// so none is missed. One it cannot resume fails the test, except an instance's: a CRD change starts a
// new watch cache, which serves no resourceVersion from before it. That one is listed again, and what
// an instance's label was is then known from its next write on.
func (i *invariant652) watch(gvr schema.GroupVersionResource, ns, from string) {
	i.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	ri := i.c.raw.Resource(gvr).Namespace(ns)
	known := map[string]*unstructured.Unstructured{}
	list := func() (string, error) {
		l, err := ri.List(ctx, metav1.ListOptions{})
		if err != nil {
			return "", err
		}
		live := map[string]*unstructured.Unstructured{}
		for k := range l.Items {
			live[l.Items[k].GetName()] = &l.Items[k]
			i.add(gvr, false, &l.Items[k])
		}
		for name, u := range known {
			if live[name] == nil {
				u = u.DeepCopy()
				u.SetResourceVersion(l.GetResourceVersion())
				i.add(gvr, true, u)
			}
		}
		known = live
		return l.GetResourceVersion(), nil
	}
	if from == "" {
		rv, err := list()
		if err != nil {
			i.t.Fatal(err)
		}
		from = rv
	}
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			w, err := ri.Watch(ctx, metav1.ListOptions{ResourceVersion: from})
			lost := apierrors.IsResourceExpired(err) || apierrors.IsGone(err)
			if err != nil && !lost {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			if err == nil {
				for ev := range w.ResultChan() {
					if ev.Type == watch.Error {
						lost = true
						break
					}
					u, ok := ev.Object.(*unstructured.Unstructured)
					if !ok {
						continue
					}
					from = u.GetResourceVersion()
					if ev.Type == watch.Deleted {
						delete(known, u.GetName())
					} else {
						known[u.GetName()] = u
					}
					i.add(gvr, ev.Type == watch.Deleted, u)
				}
				w.Stop()
			}
			if lost && ctx.Err() == nil {
				if gvr.Group != group {
					i.t.Errorf("the watch of %s from %s lost events", gvr.Resource, from)
					return
				}
				if rv, err := list(); err == nil {
					from = rv
				} else {
					time.Sleep(50 * time.Millisecond)
				}
			}
		}
	}()
	i.stop = append(i.stop, func() { cancel(); <-done })
}

func (i *invariant652) add(gvr schema.GroupVersionResource, deleted bool, u *unstructured.Unstructured) {
	rv, _ := strconv.ParseInt(u.GetResourceVersion(), 10, 64)
	i.mu.Lock()
	defer i.mu.Unlock()
	i.evs = append(i.evs, event652{rv, gvr, deleted, u})
}

// check replays the states in the order they were persisted (one etcd, so resourceVersions order them),
// and holds each to the invariant:
//
//	I1  a controller is in at most one index;
//	I2  a version with an instance has a controller: a dedicated Deployment, or exactly one index naming
//	    it. Except between its stop and what follows it: a controller being moved or retired is stopped
//	    first (marked draining, or its Deployment deleted), here as it always was, and the next one
//	    starts, or the instance is migrated, after. None may be in that state at the end;
//	I3  whatever an index names can run: its env ConfigMap is there and is that pool's member, its Lease
//	    grant is there, and the pool's Role lets its process read that ConfigMap;
//	I4  a dedicated controller a pool takes over is deleted only once that pool lists it.
func (i *invariant652) check(t *testing.T) {
	t.Helper()
	time.Sleep(500 * time.Millisecond) // let the watches deliver
	i.mu.Lock()
	evs := append([]event652{}, i.evs...)
	i.mu.Unlock()
	sort.SliceStable(evs, func(a, b int) bool { return evs[a].rv < evs[b].rv })

	type id struct {
		resource, ns, name string
	}
	state := map[id]*unstructured.Unstructured{}
	stopped := map[string]bool{} // controllers stopped, with nothing in their place yet
	states, judged := 0, 0
	seen := map[string]bool{}
	bad := func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		if !seen[msg] {
			seen[msg] = true
			t.Error(msg)
		}
	}
	get := func(resource, name string) *unstructured.Unstructured { return state[id{resource, ns652, name}] }
	for n, ev := range evs {
		tookOver := ""
		k := id{ev.gvr.Resource, ev.obj.GetNamespace(), ev.obj.GetName()}
		if ev.deleted {
			delete(state, k)
			if key, ok := strings.CutSuffix(k.name, "-controller"); ok && k.resource == "deployments" {
				stopped[key] = true
				if env := get("configmaps", key+"-configmap"); env != nil && env.GetLabels()[deploy.PoolLabel] != "" {
					tookOver = key
				}
			}
		} else {
			state[k] = ev.obj
		}
		if n+1 < len(evs) && evs[n+1].rv == ev.rv {
			continue
		}
		states++
		in := map[string][]string{} // key -> pools whose index names it
		for k, u := range state {
			if k.resource != "configmaps" {
				continue
			}
			if key, ok := strings.CutSuffix(k.name, "-configmap"); ok && u.GetAnnotations()[deploy.PoolDrainingAnnotation] != "" {
				stopped[key] = true
			}
			pool := u.GetLabels()[deploy.PoolLabel]
			if pool == "" || k.name != deploy.PoolNames(pool).Index {
				continue
			}
			data, _, _ := unstructured.NestedStringMap(u.Object, "data")
			for key := range data {
				in[key] = append(in[key], pool)
				env := get("configmaps", key+"-configmap")
				switch {
				case env == nil:
					bad("I3 at %d: pool %s lists %s, whose env ConfigMap does not exist", ev.rv, pool, key)
				case env.GetLabels()[deploy.PoolLabel] != pool:
					bad("I3 at %d: pool %s lists %s, whose env ConfigMap is in %q", ev.rv, pool, key, env.GetLabels()[deploy.PoolLabel])
				}
				if get("roles", key+"-lease") == nil {
					bad("I3 at %d: pool %s lists %s, whose Lease grant does not exist", ev.rv, pool, key)
				}
				if role := get("roles", deploy.PoolNames(pool).Workload); role == nil || !strings.Contains(fmt.Sprint(role.Object["rules"]), key+"-configmap") {
					bad("I3 at %d: pool %s lists %s, whose env ConfigMap its Role does not let it read", ev.rv, pool, key)
				}
			}
		}
		if tookOver != "" && len(in[tookOver]) == 0 {
			bad("I4 at %d: %s's Deployment was deleted before the pool taking it over listed it", ev.rv, tookOver)
		}
		for key, pools := range in {
			if len(pools) > 1 {
				bad("I1 at %d: %s is in the indexes of %v", ev.rv, key, pools)
			}
		}
		for k, u := range state {
			if !i.kind[k.resource] || u.GetDeletionTimestamp() != nil {
				continue
			}
			version := u.GetLabels()[deploy.CompositionVersionLabel]
			key := k.resource + "-" + version
			judged++
			if get("deployments", key+"-controller") != nil || len(in[key]) == 1 {
				delete(stopped, key)
				continue
			}
			if stopped[key] && n+1 < len(evs) {
				continue
			}
			bad("I2 at %d: %s has an instance and no controller: no Deployment, in the indexes of %v", ev.rv, key, in[key])
		}
	}
	if states == 0 || judged == 0 {
		t.Errorf("control: %d states, %d judgements of an instance: the invariant proved nothing", states, judged)
	}
	t.Logf("invariant: %d persisted states, %d judgements of an instance", states, judged)
}

func (e *env652) done() {
	e.t.Helper()
	e.inv.check(e.t)
	e.leases.mu.Lock()
	defer e.leases.mu.Unlock()
	for key, cm := range e.leases.unread {
		e.t.Errorf("a pool's process was told to run %s and could not read %s", key, cm)
	}
}

// ---- the cases ----

const (
	v1 = "v1-0-0"
	v2 = "v2-0-0"
)

// One member bumps. Its new version is in the index before its old one stops; the old one's grants go
// only after its Lease is released; the other member is untouched.
func TestEnvtest_652_Bump(t *testing.T) {
	e := start652(t)
	a, b := e.member("alpha", "web"), e.member("beta", "web")
	e.settle(2, 2)
	e.instance(a, v1)
	e.instance(b, v1)
	e.wantIndex("at rest", "web", e.key(a, v1), e.key(b, v1))
	e.leases.wantHolder(t, e.gvr(a, v1), "cdc-pool-web-0")

	e.leases.set(2 * time.Second) // a is slow to drain
	a.bump("2.0.0", `{"type":"object"}`)
	done := make(chan struct{})
	go func() { defer close(done); _, _ = a.pass() }()
	dropped := false
	for running := true; running; {
		select {
		case <-done:
			running = false
		case <-time.After(20 * time.Millisecond):
		}
		if e.leases.holder(e.gvr(a, v1)) == "cdc-pool-web-0" { // still draining
			e.wantGrants("while its Lease is held", e.key(a, v1), true)
			for _, k := range e.index("web") {
				if k == e.key(a, v2) {
					dropped = true
				}
			}
		}
	}
	if !dropped {
		t.Error("the new version was not in the index while the old one was draining")
	}
	e.settle(2, 1)
	e.wantIndex("after the bump", "web", e.key(a, v2), e.key(b, v1))
	e.wantGrants("retired", e.key(a, v1), false)
	e.wantGrants("running", e.key(a, v2), true)
	e.leases.wantHolder(t, e.gvr(a, v2), "cdc-pool-web-0")
	e.done()
}

// The next portal release: 15 members bump together, reconciled by 5 workers.
func TestEnvtest_652_FifteenBumpTogether(t *testing.T) {
	e := start652(t)
	var want []string
	for n := range 15 {
		e.member(fmt.Sprintf("page%02d", n), "pages")
	}
	e.settle(5, 2)
	for _, d := range e.defs {
		e.instance(d, v1)
		want = append(want, e.key(d, v2))
	}
	if n := len(e.index("pages")); n != 15 {
		t.Fatalf("premise: %d members, want 15", n)
	}
	for _, d := range e.defs {
		d.bump("2.0.0", `{"type":"object"}`)
	}
	e.settle(5, 1)
	e.wantIndex("after every bump", "pages", want...)
	for _, d := range e.defs {
		e.wantGrants("retired", e.key(d, v1), false)
	}
	e.done()
}

// A definition created in a pool joins it in its Create; the only one does not delete it.
func TestEnvtest_652_CreateJoins(t *testing.T) {
	e := start652(t)
	a := e.member("alpha", "web")
	if _, err := a.pass(); err != nil { // the Create
		t.Fatal(err)
	}
	e.wantIndex("after the first member's Create", "web", "alphas-"+v1)
	if !e.exists(&appsv1.Deployment{}, "cdc-pool-web") {
		t.Error("the only member's Create left no pool")
	}
	e.settle(1, 1)
	e.instance(a, v1)
	b := e.member("beta", "web")
	if _, err := b.pass(); err != nil {
		t.Fatal(err)
	}
	e.wantIndex("after the second's Create", "web", "alphas-"+v1, "beta-"+v1)
	e.settle(2, 1)
	e.instance(b, v1)
	e.done()
	// No state in between had no pool.
	for _, ev := range e.inv.evs {
		if ev.deleted && ev.obj.GetName() == "cdc-pool-web" {
			t.Errorf("the pool's %s was deleted", ev.gvr.Resource)
		}
	}
}

// upgradePolicy Manual: the old version's instances keep their controller, so both versions are members;
// once the migration is approved, the old one is retired.
func TestEnvtest_652_ManualKeepsBoth(t *testing.T) {
	e := start652(t)
	a := e.member("alpha", "web", func(cd *compositiondefinitionsv1alpha1.CompositionDefinition) {
		cd.Spec.UpgradePolicy = compositiondefinitionsv1alpha1.UpgradePolicyManual
	})
	e.settle(1, 2)
	e.instance(a, v1)
	a.bump("2.0.0", `{"type":"object"}`)
	e.settle(1, 2)
	e.wantIndex("both versions coexist", "web", e.key(a, v1), e.key(a, v2))
	e.leases.wantHolder(t, e.gvr(a, v1), "cdc-pool-web-0")
	// Another member's pass recomputes the index and must not lose the old version.
	b := e.member("beta", "web")
	e.settle(2, 2)
	e.wantIndex("after another member joined", "web", e.key(a, v1), e.key(a, v2), e.key(b, v1))
	a.edit(func(cd *compositiondefinitionsv1alpha1.CompositionDefinition) {
		cd.Annotations = map[string]string{compositiondefinitionsv1alpha1.AnnotationKeyUpgradeToVersion: v2}
	})
	for range 4 {
		_, _ = a.pass()
	}
	e.done()
}

// A retirement whose Lease is never released: nothing it runs with is removed, the definition says so,
// and a later release completes it.
func TestEnvtest_652_RetireWaitsForTheLease(t *testing.T) {
	e := start652(t)
	a, b := e.member("alpha", "web"), e.member("beta", "web")
	e.settle(2, 2)
	e.instance(a, v1)
	e.instance(b, v1)
	e.leases.wantHolder(t, e.gvr(a, v1), "cdc-pool-web-0")
	e.leases.set(0, v1+"."+a.cd().Status.Resource+"."+group) // a never releases
	a.bump("2.0.0", `{"type":"object"}`)
	for n := range 2 {
		start := time.Now()
		_, _ = a.pass()
		if d := time.Since(start); d < grace52 {
			t.Errorf("pass %d gave up after %s, before the bound (%s)", n+1, d, grace52)
		}
		e.wantGrants("held at the bound", e.key(a, v1), true)
		e.wantIndex("held at the bound", "web", e.key(a, v2), e.key(b, v1))
		if c := a.cd().GetCondition(rtv1.TypeSynced); c.Status != metav1.ConditionFalse || !strings.Contains(c.Message, "has not released its ownership Lease") || !strings.Contains(c.Message, "cdc-pool-web-0") {
			t.Errorf("Synced = %s %q: it must name the Lease's holder", c.Status, c.Message)
		}
	}
	// A restart of core-provider does not forget what is draining.
	a.r = e.reconciler()
	_, _ = a.pass()
	e.wantGrants("after a restart", e.key(a, v1), true)
	e.leases.set(0)
	e.settle(2, 2)
	e.wantGrants("released", e.key(a, v1), false)
	e.wantIndex("released", "web", e.key(a, v2), e.key(b, v1))
	e.done()
}

// failStatus652 fails the definition's status writes: a core-provider that stops between writing the
// index and writing the status.
type failStatus652 struct {
	client.Client
	key  client.ObjectKey
	mu   sync.Mutex
	fail bool
}

func (f *failStatus652) Status() client.SubResourceWriter {
	return &failStatusWriter652{SubResourceWriter: f.Client.Status(), f: f}
}

type failStatusWriter652 struct {
	client.SubResourceWriter
	f *failStatus652
}

func (w *failStatusWriter652) down() error {
	w.f.mu.Lock()
	defer w.f.mu.Unlock()
	if w.f.fail {
		return fmt.Errorf("the process stopped here")
	}
	return nil
}

func (w *failStatusWriter652) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	if cd, ok := obj.(*compositiondefinitionsv1alpha1.CompositionDefinition); ok && client.ObjectKeyFromObject(cd) == w.f.key {
		if err := w.down(); err != nil {
			return err
		}
	}
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

func (w *failStatusWriter652) Patch(ctx context.Context, obj client.Object, p client.Patch, opts ...client.SubResourcePatchOption) error {
	if cd, ok := obj.(*compositiondefinitionsv1alpha1.CompositionDefinition); ok && client.ObjectKeyFromObject(cd) == w.f.key {
		if err := w.down(); err != nil {
			return err
		}
	}
	return w.SubResourceWriter.Patch(ctx, obj, p, opts...)
}

func TestEnvtest_652_RestartBetweenIndexAndStatus(t *testing.T) {
	e := start652(t)
	a, b := e.member("alpha", "web"), e.member("beta", "web")
	e.settle(2, 2)
	e.instance(a, v1)
	e.instance(b, v1)
	f := &failStatus652{Client: e.kube.lagging644.Client, key: a.key, fail: true}
	e.kube.lagging644.Client = f
	a.r = e.reconciler()
	a.bump("2.0.0", `{"type":"object"}`)
	for range 2 { // the Update ran, and its status was never written
		_, _ = a.pass()
	}
	if at := a.cd().Status.ApiVersion; at != group+"/"+v1 {
		t.Fatalf("premise: status.apiVersion = %s, want it still the old one", at)
	}
	e.wantIndex("status not written", "web", e.key(a, v2), e.key(b, v1))
	// Another member's pass recomputes from what is there, not from that status.
	if _, err := b.pass(); err != nil {
		t.Fatal(err)
	}
	b.edit(func(cd *compositiondefinitionsv1alpha1.CompositionDefinition) {
		d := "1m"
		cd.Spec.Controller.ResyncInterval = &d
	})
	_, _ = b.pass()
	e.wantIndex("after another member's Update", "web", e.key(a, v2), e.key(b, v1))
	f.mu.Lock()
	f.fail = false
	f.mu.Unlock()
	a.r = e.reconciler()
	e.settle(2, 2)
	e.wantIndex("recovered", "web", e.key(a, v2), e.key(b, v1))
	e.done()
}

// Leaving: for a dedicated controller (with a bump and without), for another pool, and the last one.
func TestEnvtest_652_LeavingAndMoving(t *testing.T) {
	e := start652(t)
	a, b, c, d := e.member("alpha", "web"), e.member("beta", "web"), e.member("gamma", "other"), e.member("delta", "")
	e.settle(4, 2)
	for _, def := range e.defs {
		e.instance(def, v1)
	}
	e.leases.wantHolder(t, e.gvr(a, v1), "cdc-pool-web-0")
	e.leases.wantHolder(t, e.gvr(d, v1), e.key(d, v1)+"-controller")
	e.leases.set(time.Second)

	// Mid-bump: a's new version is dedicated, its old one leaves the pool.
	e.charts.add(t, "alpha", "2.0.0", `{"type":"object"}`)
	a.edit(func(cd *compositiondefinitionsv1alpha1.CompositionDefinition) {
		setPool(cd, "")
		cd.Spec.Chart.Url, cd.Spec.Chart.Version = e.charts.url("alpha", "2.0.0"), "2.0.0"
	})
	e.settle(1, 2, a)
	e.wantIndex("a left mid-bump", "web", e.key(b, v1))
	e.leases.wantHolder(t, e.gvr(a, v2), e.key(a, v2)+"-controller")

	// Without one: the same Kind x version, from the pool to a Deployment of its own.
	b.edit(func(cd *compositiondefinitionsv1alpha1.CompositionDefinition) { setPool(cd, "") })
	start := time.Now()
	e.settle(1, 1, b)
	if time.Since(start) < time.Second {
		t.Error("b was taken over before the pool had drained it")
	}
	e.leases.wantHolder(t, e.gvr(b, v1), e.key(b, v1)+"-controller")
	if e.exists(&appsv1.Deployment{}, "cdc-pool-web") || e.index("web") != nil {
		t.Error("the last leaver left pool web behind")
	}

	// From one pool to another, and back from dedicated.
	c.edit(func(cd *compositiondefinitionsv1alpha1.CompositionDefinition) { setPool(cd, "third") })
	b.edit(func(cd *compositiondefinitionsv1alpha1.CompositionDefinition) { setPool(cd, "third") })
	e.settle(2, 2, b, c)
	e.wantIndex("moved", "third", e.key(b, v1), e.key(c, v1))
	if e.index("other") != nil {
		t.Error("pool other was left behind")
	}
	e.leases.wantHolder(t, e.gvr(b, v1), "cdc-pool-third-0")
	e.leases.wantHolder(t, e.gvr(c, v1), "cdc-pool-third-0")
	// A dedicated version retired in the pass that joins a pool.
	// Its compositions' derived RBAC follows it there, not to a per-version identity a member does not have.
	if err := e.c.cl.Create(context.Background(), &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "one-krateo-rbac", Namespace: self642CDNS},
		RoleRef:  rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: "one-krateo-rbac"},
		Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Namespace: ns652, Name: e.key(d, v1)}}}); err != nil {
		t.Fatal(err)
	}
	e.charts.add(t, "delta", "2.0.0", `{"type":"object"}`)
	d.edit(func(cd *compositiondefinitionsv1alpha1.CompositionDefinition) {
		setPool(cd, "third")
		cd.Spec.Chart.Url, cd.Spec.Chart.Version = e.charts.url("delta", "2.0.0"), "2.0.0"
	})
	e.settle(1, 2, d)
	e.wantIndex("d joined with a bump", "third", e.key(d, v2), e.key(b, v1), e.key(c, v1))
	e.leases.wantHolder(t, e.gvr(d, v2), "cdc-pool-third-0")
	var rb rbacv1.RoleBinding
	if err := e.c.cl.Get(context.Background(), types.NamespacedName{Namespace: self642CDNS, Name: "one-krateo-rbac"}, &rb); err != nil {
		t.Fatal(err)
	}
	if got := rb.Subjects[0].Name; got != "cdc-pool-third" {
		t.Errorf("the derived binding names %s, want the pool's ServiceAccount", got)
	}
	e.done()
}

// A dedicated controller deployed before the ownership Lease existed cannot hold it, so it is retired
// BEFORE its pool lists it: the pool's member would not wait for it.
func TestEnvtest_652_JoiningWithoutALease(t *testing.T) {
	e := start652(t)
	a := e.member("alpha", "")
	e.settle(1, 2)
	key := e.key(a, v1)
	ctx := context.Background()
	if _, err := e.c.cs.CoreV1().ConfigMaps(ns652).Patch(ctx, key+"-configmap", types.JSONPatchType,
		[]byte(`[{"op":"remove","path":"/data/COMPOSITION_CONTROLLER_LEASE_NAME"}]`), metav1.PatchOptions{}); err != nil {
		t.Fatal(err)
	}
	a.edit(func(cd *compositiondefinitionsv1alpha1.CompositionDefinition) { setPool(cd, "web") })
	e.settle(1, 2)
	e.wantIndex("joined", "web", key)
	time.Sleep(500 * time.Millisecond)
	var retired, listed int64
	e.inv.mu.Lock()
	defer e.inv.mu.Unlock()
	for _, ev := range e.inv.evs {
		if ev.deleted && ev.obj.GetName() == key+"-controller" {
			retired = ev.rv
		}
		if data, _, _ := unstructured.NestedStringMap(ev.obj.Object, "data"); ev.obj.GetName() == deploy.PoolNames("web").Index && data[key] != "" && listed == 0 {
			listed = ev.rv
		}
	}
	if retired == 0 || listed == 0 || listed < retired {
		t.Errorf("the pool listed it at %d, and its Deployment was deleted at %d", listed, retired)
	}
}

// Deleting a definition whose older version a pool still runs (Manual), and whose Lease is never released:
// the Kind's CRD is not removed from under it, and nothing of it is revoked.
func TestEnvtest_652_DeleteWaitsForAnOlderVersion(t *testing.T) {
	e := start652(t)
	a := e.member("alpha", "web", func(cd *compositiondefinitionsv1alpha1.CompositionDefinition) {
		cd.Spec.UpgradePolicy = compositiondefinitionsv1alpha1.UpgradePolicyManual
	})
	e.settle(1, 2)
	e.leases.wantHolder(t, e.gvr(a, v1), "cdc-pool-web-0")
	a.bump("2.0.0", `{"type":"object"}`)
	e.settle(1, 2)
	old, gvr := e.key(a, v1), e.gvr(a, v1)
	e.leases.set(0, v1+"."+gvr.Resource+"."+group)
	if err := e.c.cl.Delete(context.Background(), a.cd()); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		_, _ = a.pass()
	}
	e.wantGrants("its Lease is held", old, true)
	if _, err := e.c.raw.Resource(gvr).Namespace(self642CDNS).List(context.Background(), metav1.ListOptions{}); err != nil {
		t.Errorf("the Kind is gone while a pool still runs its older version: %v", err)
	}
	e.leases.set(0)
	for range 6 {
		_, _ = a.pass()
	}
	if err := e.c.cl.Get(context.Background(), a.key, &compositiondefinitionsv1alpha1.CompositionDefinition{}); !apierrors.IsNotFound(err) {
		t.Fatalf("not deleted once the Lease was released: %v", err)
	}
	e.wantGrants("deleted", old, false)
	if e.exists(&appsv1.Deployment{}, "cdc-pool-web") {
		t.Error("the pool was left behind")
	}
}

// A paused member is in nobody's way and nobody's pass drops it; a deleted one leaves, its controller
// stopped before its grants go; a sibling version's controller survives; the last one takes the pool.
func TestEnvtest_652_PausedAndDeleted(t *testing.T) {
	e := start652(t)
	a, b := e.member("alpha", "web"), e.member("beta", "web")
	e.settle(2, 2)
	e.instance(a, v1)
	e.instance(b, v1)
	b.annotate(map[string]string{"krateo.io/paused": "true"})
	b.repoint("2.0.0") // a paused definition's spec is nobody's to act on
	_, _ = b.pass()
	a.bump("2.0.0", `{"type":"object"}`)
	e.settle(1, 2, a)
	e.wantIndex("b paused", "web", e.key(a, v2), e.key(b, v1))
	e.inv.check(t) // every state so far; the instances go next

	e.leases.wantHolder(t, e.gvr(a, v2), "cdc-pool-web-0")
	e.leases.set(time.Second)
	key := e.key(a, v2)
	gvr := e.gvr(a, v2)
	if err := e.c.cl.Delete(context.Background(), a.cd()); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 6; n++ {
		if e.leases.holder(gvr) != "" {
			e.wantGrants("deleting, its Lease held", key, true)
		}
		_, _ = a.pass()
	}
	if err := e.c.cl.Get(context.Background(), a.key, &compositiondefinitionsv1alpha1.CompositionDefinition{}); !apierrors.IsNotFound(err) {
		t.Fatalf("a was not deleted: %v", err)
	}
	e.wantGrants("deleted", key, false)
	e.wantIndex("a deleted", "web", e.key(b, v1))
	if !e.exists(&appsv1.Deployment{}, "cdc-pool-web") {
		t.Error("deleting one member deleted the pool")
	}
	b.annotate(map[string]string{"krateo.io/paused": ""})
	if err := e.c.cl.Delete(context.Background(), b.cd()); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 6; n++ {
		_, _ = b.pass()
	}
	if e.exists(&appsv1.Deployment{}, "cdc-pool-web") || e.index("web") != nil {
		t.Error("the last member's delete left the pool behind")
	}
}

// Two definitions on two versions of one Kind, in one pool. Deleting one keeps the Kind, so the runtime
// may drop the finalizer as soon as its controller is gone: not before Delete has taken it out.
func TestEnvtest_652_DeleteOneVersionOfAKind(t *testing.T) {
	e := start652(t)
	a := e.member("alpha", "web")
	e.settle(1, 2)
	e.charts.add(t, "alpha", "2.0.0", `{"type":"object"}`)
	b := e.define(t, "alpha", "alpha-two", func(cd *compositiondefinitionsv1alpha1.CompositionDefinition) {
		setPool(cd, "web")
		cd.Spec.Chart.Url, cd.Spec.Chart.Version = e.charts.url("alpha", "2.0.0"), "2.0.0"
	})
	for range 4 {
		_, _ = b.pass()
	}
	key1, key2 := "alphas-"+v1, "alphas-"+v2
	e.wantIndex("two versions of one Kind", "web", key1, key2)
	if err := e.c.cl.Delete(context.Background(), b.cd()); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 6; n++ {
		_, _ = b.pass()
	}
	if err := e.c.cl.Get(context.Background(), b.key, &compositiondefinitionsv1alpha1.CompositionDefinition{}); !apierrors.IsNotFound(err) {
		t.Fatalf("b was not deleted: %v", err)
	}
	e.wantIndex("one deleted", "web", key1)
	e.wantGrants("deleted", key2, false)
	e.wantGrants("the other version", key1, true)
	_ = a
}

// Two pools do not touch each other, and a follower's own pool field changes nothing: the owner's decides.
func TestEnvtest_652_TwoPoolsAndAFollower(t *testing.T) {
	e := start652(t)
	a, b := e.member("alpha", "one"), e.member("beta", "two")
	e.settle(2, 2)
	e.instance(a, v1)
	e.instance(b, v1)
	time.Sleep(1100 * time.Millisecond) // creationTimestamp is the election's order, in seconds
	f := e.define(t, "alpha", "alpha-follower", func(cd *compositiondefinitionsv1alpha1.CompositionDefinition) { setPool(cd, "two") })
	for range 3 {
		_, _ = f.pass()
	}
	e.wantIndex("one", "one", e.key(a, v1))
	e.wantIndex("two", "two", e.key(b, v1))
	two := e.poolRV(t, "two")
	a.bump("2.0.0", `{"type":"object"}`)
	for range 4 {
		_, _ = a.pass()
	}
	e.wantIndex("one, after its member's bump", "one", e.key(a, v1), e.key(a, v2)) // v1 is still the follower's
	if got := e.poolRV(t, "two"); got != two {
		t.Errorf("pool two was written by pool one's bump: %s -> %s", two, got)
	}
	e.done()
}

func (e *env652) poolRV(t *testing.T, pool string) string {
	t.Helper()
	cm := &corev1.ConfigMap{}
	if err := e.c.cl.Get(context.Background(), types.NamespacedName{Namespace: ns652, Name: deploy.PoolNames(pool).Index}, cm); err != nil {
		t.Fatal(err)
	}
	return cm.ResourceVersion
}

// A pool's objects are in no definition's digest, so each is compared in Observe: whatever is lost or
// edited, and a release that changes only the pool's image or sizing, is repaired by a member's next
// pass, with nothing else of the member changed.
func TestEnvtest_652_ObserveRepairsThePool(t *testing.T) {
	e := start652(t)
	ctx := context.Background()
	a, b := e.member("alpha", "web"), e.member("beta", "web")
	e.settle(2, 2)
	e.instance(a, v1)
	names := deploy.PoolNames("web")
	cms, deps := e.c.cs.CoreV1().ConfigMaps(ns652), e.c.cs.AppsV1().Deployments(ns652)
	want := []string{e.key(a, v1), e.key(b, v1)}

	// Control: at rest, a pass writes nothing.
	quiet := func() string {
		dep, err := deps.Get(ctx, names.Workload, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return e.poolRV(t, "web") + "/" + strconv.FormatInt(dep.Generation, 10)
	}
	before := quiet()
	a.rest(1)
	b.rest(1)
	if after := quiet(); after != before {
		t.Errorf("control: passes at rest changed the pool: %s -> %s", before, after)
	}

	patchIndex := func(t *testing.T, f func(map[string]string)) {
		cm, err := cms.Get(ctx, names.Index, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		f(cm.Data)
		if _, err := cms.Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	image := func() string {
		dep, err := deps.Get(ctx, names.Workload, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return dep.Spec.Template.Spec.Containers[0].Image
	}
	for _, c := range []struct {
		name   string
		break_ func(t *testing.T)
		fixed  func() bool
	}{
		{"its entry is gone", func(t *testing.T) { patchIndex(t, func(d map[string]string) { delete(d, e.key(a, v1)) }) },
			func() bool { return reflect.DeepEqual(e.index("web"), want) }},
		{"its entry is another configuration's", func(t *testing.T) {
			patchIndex(t, func(d map[string]string) { d[e.key(a, v1)] = `{"configMap":"x","checksum":"y"}` })
		}, func() bool {
			cm, err := cms.Get(ctx, names.Index, metav1.GetOptions{})
			return err == nil && strings.Contains(cm.Data[e.key(a, v1)], e.key(a, v1)+"-configmap")
		}},
		{"an entry nothing backs", func(t *testing.T) {
			patchIndex(t, func(d map[string]string) {
				d["ghosts-v1-0-0"] = `{"configMap":"ghosts-v1-0-0-configmap","checksum":"y"}`
			})
		}, func() bool { return reflect.DeepEqual(e.index("web"), want) }},
		{"another pool's index lists it too", func(t *testing.T) {
			if _, err := cms.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: deploy.PoolNames("old").Index, Labels: map[string]string{deploy.PoolLabel: "old"}},
				Data: map[string]string{e.key(a, v1): `{"configMap":"` + e.key(a, v1) + `-configmap","checksum":"y"}`}}, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
		}, func() bool { return e.index("old") == nil }},
		{"the index is gone", func(t *testing.T) {
			if err := cms.Delete(ctx, names.Index, metav1.DeleteOptions{}); err != nil {
				t.Fatal(err)
			}
		}, func() bool { return reflect.DeepEqual(e.index("web"), want) }},
		{"its env ConfigMap still reads as leaving", func(t *testing.T) {
			if _, err := cms.Patch(ctx, e.key(a, v1)+"-configmap", types.MergePatchType,
				[]byte(`{"metadata":{"annotations":{"`+deploy.PoolDrainingAnnotation+`":"true"}}}`), metav1.PatchOptions{}); err != nil {
				t.Fatal(err)
			}
		}, func() bool {
			cm, err := cms.Get(ctx, e.key(a, v1)+"-configmap", metav1.GetOptions{})
			return err == nil && cm.Annotations[deploy.PoolDrainingAnnotation] == "" && reflect.DeepEqual(e.index("web"), want)
		}},
		{"the Deployment is gone", func(t *testing.T) {
			if err := deps.Delete(ctx, names.Workload, metav1.DeleteOptions{}); err != nil {
				t.Fatal(err)
			}
		}, func() bool { return e.exists(&appsv1.Deployment{}, names.Workload) }},
		{"the Role is gone", func(t *testing.T) {
			if err := e.c.cs.RbacV1().Roles(ns652).Delete(ctx, names.Workload, metav1.DeleteOptions{}); err != nil {
				t.Fatal(err)
			}
		}, func() bool { return e.exists(&rbacv1.Role{}, names.Workload) }},
		{"a release changes only the pool's image", func(_ *testing.T) {
			root := t.TempDir() // the outer test's: read by every later pass
			if _, err := renderChartAssets(root, ns652, "ghcr.io/krateo-platformops/composition-dynamic-controller:652-next"); err != nil {
				t.Fatal(err)
			}
			PoolDeploymentTemplatePath = filepath.Join(root, "cdc-deployment", "pool-deployment.yaml")
		}, func() bool { return strings.HasSuffix(image(), ":652-next") }},
		{"a release changes only the pool's sizing", func(_ *testing.T) {
			root := t.TempDir() // the outer test's: read by every later pass
			if _, err := renderChartAssetsWith(root, ns652, "ghcr.io/krateo-platformops/composition-dynamic-controller:652-next", map[string]any{
				"cdc": map[string]any{"pool": map[string]any{"default": map[string]any{"workers": 3}},
					"image": map[string]any{"registry": "", "repository": "ghcr.io/krateo-platformops/composition-dynamic-controller", "tag": "652-next"}},
			}); err != nil {
				t.Fatal(err)
			}
			PoolSizingPath = filepath.Join(root, "cdc-deployment", "pool-sizing.json")
		}, func() bool {
			cm, err := cms.Get(ctx, names.EnvConfigMap, metav1.GetOptions{})
			return err == nil && cm.Data["COMPOSITION_CONTROLLER_POOL_WORKERS"] == "3"
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			c.break_(t)
			for range 2 {
				if _, err := a.pass(); err != nil {
					t.Fatalf("pass: %v", err)
				}
			}
			if !c.fixed() {
				t.Errorf("not repaired: index %v, image %s, Ready %s %q", e.index("web"), image(), a.cond("Ready"), a.message("Ready"))
			}
			a.rest(2)
			b.rest(2)
		})
	}
}

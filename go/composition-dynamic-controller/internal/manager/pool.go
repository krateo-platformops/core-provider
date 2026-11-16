package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/krateo-platformops/unstructured-runtime/pkg/controller"
	metricsserver "github.com/krateo-platformops/unstructured-runtime/pkg/metrics/server"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// Pool runs the members a pool's membership index names (core-provider docs/design/cdc-pooling.md
// §5c). It is step 4's manager with membership read at runtime:
//
//   - core-provider writes the index, ConfigMap `cdc-pool-<p>-members`: one key per member, valued
//     with IndexEntry, the member's env ConfigMap and that ConfigMap's checksum. The pool WATCHES that
//     one ConfigMap by name, so a join, a leave, or a member's configuration changing is one event,
//     and nothing restarts the process (§3.2: the pool is not told through its own environment).
//   - A member's configuration is its env ConfigMap, read by name and parsed by MemberFromConfig,
//     exactly as a dedicated controller parses its environment.
//   - FAILURE ISOLATION (Diego, 2026-11-16, replacing step 4's provisional rule): a member that fails,
//     a lost Lease included, is rebuilt and restarted ALONE, with backoff; the others keep
//     reconciling. Only a process-level failure (the index watch, the metrics server) ends Run.
type Pool struct {
	Process Process
	Client  kubernetes.Interface
	// Namespace holds the index and the member ConfigMaps: the pool's own (the management namespace).
	Namespace string
	// Index is the membership index ConfigMap's name.
	Index    string
	Resolver ResolverFactory
	// Backoff is the wait before a failed member's next start; nil is 1s doubling to 1m.
	Backoff func(attempt int) time.Duration

	mu      sync.Mutex
	running map[string]*poolMember
}

// IndexEntry is one member's value in the index.
type IndexEntry struct {
	ConfigMap string `json:"configMap"`
	Checksum  string `json:"checksum"`
}

// ParseIndex reads the index's data into its entries. An entry that does not parse is reported, not
// skipped silently: a member core-provider meant to run would otherwise never start, with no trace.
func ParseIndex(data map[string]string) (map[string]IndexEntry, error) {
	out := make(map[string]IndexEntry, len(data))
	var bad []string
	for k, v := range data {
		var e IndexEntry
		if err := json.Unmarshal([]byte(v), &e); err != nil || e.ConfigMap == "" {
			bad = append(bad, k)
			continue
		}
		out[k] = e
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return out, fmt.Errorf("index entries that are not {configMap, checksum}: %s", strings.Join(bad, ", "))
	}
	return out, nil
}

type poolMember struct {
	entry  IndexEntry
	cancel context.CancelFunc
	done   chan struct{}

	mu   sync.Mutex
	ctrl *controller.Controller // nil while building or backing off
	last error                  // the last failure, for readiness
}

func (m *poolMember) set(c *controller.Controller, err error) {
	m.mu.Lock()
	m.ctrl, m.last = c, err
	m.mu.Unlock()
}

// Run watches the index and keeps the running members equal to it until ctx ends.
func (p *Pool) Run(ctx context.Context) error {
	if p.Index == "" || p.Namespace == "" || p.Client == nil {
		return errors.New("manager: a pool needs its index name, namespace and client")
	}
	p.mu.Lock()
	p.running = map[string]*poolMember{}
	p.mu.Unlock()
	if err := p.startMetrics(ctx); err != nil {
		return err
	}

	lw := cache.NewListWatchFromClient(p.Client.CoreV1().RESTClient(), "configmaps", p.Namespace,
		fields.OneTermEqualSelector("metadata.name", p.Index))
	_, informer := cache.NewInformerWithOptions(cache.InformerOptions{
		ListerWatcher: lw,
		ObjectType:    &corev1.ConfigMap{},
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc:    func(obj any) { p.sync(ctx, obj) },
			UpdateFunc: func(_, obj any) { p.sync(ctx, obj) },
			DeleteFunc: func(any) { p.sync(ctx, nil) }, // no index: no members (the pool is being torn down)
		},
	})
	informer.Run(ctx.Done())
	p.stopAll()
	return nil
}

// sync makes the running members match the index.
func (p *Pool) sync(ctx context.Context, obj any) {
	want := map[string]IndexEntry{}
	if cm, ok := obj.(*corev1.ConfigMap); ok {
		var err error
		if want, err = ParseIndex(cm.Data); err != nil {
			p.logError(err, "the membership index has entries that cannot be read; those members are not started")
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for key, m := range p.running {
		if e, ok := want[key]; !ok || e != m.entry {
			p.stopLocked(key) // left the pool, or its configuration changed: restarted below
		}
	}
	keys := make([]string, 0, len(want))
	for k := range want {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if _, ok := p.running[key]; ok {
			continue
		}
		p.startLocked(ctx, key, want[key])
	}
}

func (p *Pool) startLocked(ctx context.Context, key string, e IndexEntry) {
	mctx, cancel := context.WithCancel(ctx)
	m := &poolMember{entry: e, cancel: cancel, done: make(chan struct{})}
	p.running[key] = m
	go func() {
		defer close(m.done)
		for attempt := 0; ; attempt++ {
			err := p.runOnce(mctx, key, m)
			if mctx.Err() != nil {
				return // removed from the pool, or the process is stopping
			}
			m.set(nil, err)
			p.logError(err, "pool member stopped; restarting it alone", "member", key, "attempt", attempt+1)
			select {
			case <-mctx.Done():
				return
			case <-time.After(p.backoff(attempt)):
			}
		}
	}()
}

// runOnce reads the member's configuration, builds its controller and runs it until it stops.
func (p *Pool) runOnce(ctx context.Context, key string, m *poolMember) error {
	cm, err := p.Client.CoreV1().ConfigMaps(p.Namespace).Get(ctx, m.entry.ConfigMap, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("reading member configuration %s/%s: %w", p.Namespace, m.entry.ConfigMap, err)
	}
	member, err := MemberFromConfig(cm.Data, p.Resolver, func(msg string, err error) { p.logError(err, msg, "member", key) })
	if err != nil {
		return err
	}
	c, err := p.Process.build(ctx, member)
	if err != nil {
		return fmt.Errorf("building the controller for %s: %w", member.GVR, err)
	}
	m.set(c, nil)
	if err := c.Run(ctx, member.workers(p.Process)); err != nil {
		return fmt.Errorf("%s: %w", member.GVR, err)
	}
	return errors.New("the controller returned without an error")
}

func (p *Pool) stopLocked(key string) {
	m := p.running[key]
	delete(p.running, key)
	m.cancel()
	<-m.done // drained: its Lease is released before a successor can take it (step 2)
}

func (p *Pool) stopAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for key := range p.running {
		p.stopLocked(key)
	}
}

// ReadyzCheck is ready when every member the index names is running and ready. A member being
// rebuilt after a failure is reported by name, so /readyz says which one.
func (p *Pool) ReadyzCheck(req *http.Request) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	keys := make([]string, 0, len(p.running))
	for k := range p.running {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		m := p.running[k]
		m.mu.Lock()
		c, last := m.ctrl, m.last
		m.mu.Unlock()
		if c == nil {
			if last != nil {
				return fmt.Errorf("%s: restarting after: %w", k, last)
			}
			return fmt.Errorf("%s: starting", k)
		}
		if err := c.ReadyzCheck(req); err != nil {
			return fmt.Errorf("%s: %w", k, err)
		}
	}
	return nil
}

// Members lists the running members' keys, sorted.
func (p *Pool) Members() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	keys := make([]string, 0, len(p.running))
	for k := range p.running {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (p *Pool) backoff(attempt int) time.Duration {
	if p.Backoff != nil {
		return p.Backoff(attempt)
	}
	d := time.Second << min(attempt, 6)
	return min(d, time.Minute)
}

func (p *Pool) startMetrics(ctx context.Context) error {
	addr := p.Process.MetricsBindAddress
	if addr == "" || addr == "0" {
		return nil
	}
	srv, err := metricsserver.NewServer(metricsserver.Options{BindAddress: addr}, p.Process.Config, http.DefaultClient)
	if err != nil {
		return fmt.Errorf("manager: creating the metrics server: %w", err)
	}
	if srv != nil {
		go func() {
			if err := srv.WithLogger(p.Process.Log).Start(ctx); err != nil {
				p.logError(err, "metrics server failed")
			}
		}()
	}
	return nil
}

func (p *Pool) logError(err error, msg string, kv ...any) {
	if p.Process.Log != nil && err != nil {
		p.Process.Log.Error(err, msg, kv...)
	}
}

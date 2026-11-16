//go:build envtest

// docs/design/cdc-pooling.md §5c, against a real apiserver: a pool runs the members its index names,
// adds and removes them at runtime without stopping the others, restarts a member whose configuration
// changed, and restarts a FAILING member alone while the healthy one keeps reconciling.
package manager

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/krateo-platformops/unstructured-runtime/pkg/controller"
)

func memberCM(ns string, gvr schema.GroupVersionResource) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: gvr.Resource + "-" + gvr.Version + "-configmap", Namespace: ns},
		Data: map[string]string{KeyGroup: gvr.Group, KeyVersion: gvr.Version, KeyResource: gvr.Resource}}
}

func entry(t *testing.T, cm string, sum string) string {
	t.Helper()
	b, err := json.Marshal(IndexEntry{ConfigMap: cm, Checksum: sum})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(30 * time.Second); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func (p *Pool) controllerOf(key string) *controller.Controller {
	p.mu.Lock()
	defer p.mu.Unlock()
	m := p.running[key]
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ctrl
}

func TestEnvtest_PoolMembership(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS unset")
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest apiserver: %v", err)
	}
	defer func() { _ = env.Stop() }()
	ctx := context.Background()
	ac := apiextclient.NewForConfigOrDie(cfg)
	for gvr, kind := range map[schema.GroupVersionResource]string{fireworks: "FireworksApp", portals: "Portal"} {
		if _, err := ac.ApiextensionsV1().CustomResourceDefinitions().Create(ctx, compositionCRD(gvr, kind), metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	dyn := dynamic.NewForConfigOrDie(cfg)
	for _, gvr := range []schema.GroupVersionResource{fireworks, portals} {
		eventually(t, gvr.String()+" served", func() bool { _, err := dyn.Resource(gvr).List(ctx, metav1.ListOptions{}); return err == nil })
	}
	cs := kubernetes.NewForConfigOrDie(cfg)
	const ns = "krateo-system"
	if _, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, gvr := range []schema.GroupVersionResource{fireworks, portals} {
		if _, err := cs.CoreV1().ConfigMaps(ns).Create(ctx, memberCM(ns, gvr), metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	fwKey, ptKey := fireworks.Resource+"-"+fireworks.Version, portals.Resource+"-"+portals.Version
	index := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cdc-pool-web-members", Namespace: ns}, Data: map[string]string{
		fwKey: entry(t, fwKey+"-configmap", "1"),
		ptKey: entry(t, ptKey+"-configmap", "1"),
	}}
	if _, err := cs.CoreV1().ConfigMaps(ns).Create(ctx, index, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	setIndex := func(data map[string]string) {
		t.Helper()
		cur, err := cs.CoreV1().ConfigMaps(ns).Get(ctx, index.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		cur.Data = data
		if _, err := cs.CoreV1().ConfigMaps(ns).Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	proc := testProcess()
	proc.Config = cfg
	pool := &Pool{Process: proc, Client: cs, Namespace: ns, Index: index.Name, Backoff: func(int) time.Duration { return 200 * time.Millisecond }}
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- pool.Run(runCtx) }()

	eventually(t, "both members running and ready", func() bool {
		return reflect.DeepEqual(pool.Members(), []string{fwKey, ptKey}) && pool.ReadyzCheck(nil) == nil
	})
	fw := pool.controllerOf(fwKey)

	// Leave: portals is removed, fireworks keeps its controller (not restarted).
	setIndex(map[string]string{fwKey: entry(t, fwKey+"-configmap", "1")})
	eventually(t, "portals left the pool", func() bool { return reflect.DeepEqual(pool.Members(), []string{fwKey}) })
	if pool.controllerOf(fwKey) != fw {
		t.Fatal("a member leaving restarted the member that stayed")
	}

	// A configuration change (the checksum in its entry) restarts exactly that member.
	setIndex(map[string]string{fwKey: entry(t, fwKey+"-configmap", "2")})
	eventually(t, "fireworks restarted on a configuration change", func() bool {
		c := pool.controllerOf(fwKey)
		return c != nil && c != fw && c.ReadyzCheck(nil) == nil
	})
	fw = pool.controllerOf(fwKey)

	// Isolation: a member whose configuration cannot be read fails and is retried ALONE; the healthy
	// member keeps its controller and keeps reconciling, and readiness names the failing one.
	setIndex(map[string]string{fwKey: entry(t, fwKey+"-configmap", "2"), ptKey: entry(t, "missing-configmap", "1")})
	eventually(t, "readiness names the failing member", func() bool {
		err := pool.ReadyzCheck(nil)
		return err != nil && strings.Contains(err.Error(), ptKey) && strings.Contains(err.Error(), "restarting after")
	})
	time.Sleep(time.Second) // several backoff cycles of the failing member
	if pool.controllerOf(fwKey) != fw || fw.ReadyzCheck(nil) != nil {
		t.Fatal("a failing member disturbed the healthy one")
	}
	// It recovers once its configuration exists.
	setIndex(map[string]string{fwKey: entry(t, fwKey+"-configmap", "2"), ptKey: entry(t, ptKey+"-configmap", "1")})
	eventually(t, "the failing member recovered", func() bool { return pool.ReadyzCheck(nil) == nil })

	time.Sleep(time.Second) // past informer startup, as TestEnvtest_TwoMembersInOneProcess explains
	stop()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v on a clean shutdown", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}
}

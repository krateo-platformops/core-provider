//go:build integration
// +build integration

package watcher

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/util/retry"

	"sigs.k8s.io/e2e-framework/pkg/env"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/envfuncs"
	"sigs.k8s.io/e2e-framework/pkg/features"
	"sigs.k8s.io/e2e-framework/support/kind"

	// if you have e2e helpers; otherwise remove
	"github.com/krateo-platformops/plumbing/e2e"
	xenv "github.com/krateo-platformops/plumbing/env"
)

var (
	testenv     env.Environment
	clusterName string
	namespace   string
)

func TestMain(m *testing.M) {
	xenv.SetTestMode(true)

	// Every cluster-starting suite pins KUBECONFIG first: kind writes to AND switches
	// ~/.kube/config otherwise, and DestroyCluster then removes that context again, leaving the
	// developer pointed at nothing. Invisible on CI, where there is no kubeconfig worth
	// clobbering. See e2e.PinKubeconfig.
	restore := e2e.PinKubeconfig("cp-kube-watcher")

	namespace = "demo-system"
	clusterName = "krateo-watcher"
	testenv = env.New()

	testenv.Setup(
		envfuncs.CreateCluster(kind.NewProvider(), clusterName),
		envfuncs.CreateNamespace(namespace),
	).Finish(
		envfuncs.DeleteNamespace(namespace),
		envfuncs.DestroyCluster(clusterName),
	)

	code := testenv.Run(m)
	restore() // not deferred: os.Exit does not run defers
	os.Exit(code)
}

// ---- Tests ----

func TestWatcher_ImmediateReady_E2E(t *testing.T) {
	os.Setenv("DEBUG", "1")

	f := features.New("Watcher Immediate Ready - e2e").
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			return ctx
		}).Assess("ImmediateReady", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
		ns := namespace
		name := "ready-deploy-e2e"

		dyn, err := dynamic.NewForConfig(cfg.Client().RESTConfig())
		if err != nil {
			t.Fatalf("failed to create dynamic client: %v", err)
		}

		// replicas 0: the Deployment controller reports Available=True (MinimumReplicasAvailable) with no
		// pod and no image pull, so "already ready when the watch starts" no longer waits on a registry.
		// A Create cannot set status itself (status is a subresource), which is why the old version
		// slept 20s for a real nginx rollout. Wait for the controller's condition, bounded, then watch.
		_, err = dyn.Resource(gvr).Namespace(ns).Create(ctx, makeDeployment(name, ns, 0, neverPulledImage), metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("failed to create deployment: %v", err)
		}
		defer func() { _ = dyn.Resource(gvr).Namespace(ns).Delete(context.Background(), name, metav1.DeleteOptions{}) }()
		if err := waitForAvailable(ctx, dyn, gvr, ns, name, 30*time.Second); err != nil {
			t.Fatalf("premise: a replicas-0 Deployment is reported Available by its controller: %v", err)
		}

		w := NewWatcher(dyn, gvr, 5*time.Second, isAvailableChecker)

		if err := w.WatchResource(ctx, ns, name); err != nil {
			t.Fatalf("expected ready, got error: %v", err)
		}

		return ctx
	}).Feature()

	testenv.Test(t, f)
}

func TestWatcher_BecomesReadyAfterModify_E2E(t *testing.T) {
	os.Setenv("DEBUG", "1")

	f := features.New("Watcher Becomes Ready After Modify - e2e").
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			return ctx
		}).Assess("BecomesReadyAfterModify", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
		ns := namespace
		name := "eventual-deploy-e2e"

		dyn, err := dynamic.NewForConfig(cfg.Client().RESTConfig())
		if err != nil {
			t.Fatalf("failed to create dynamic client: %v", err)
		}

		// An image that can never be pulled: the Deployment controller can only ever report it
		// Available=False, so the ONLY way to Available=True is this test's own write (#631: the old
		// version wrote status with a main-resource Update, which the apiserver drops, and passed only
		// when kind pulled nginx:latest and rolled it out within the 10s timeout).
		_, err = dyn.Resource(gvr).Namespace(ns).Create(ctx, makeDeployment(name, ns, 1, neverPulledImage), metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("failed to create initial deploy: %v", err)
		}
		defer func() { _ = dyn.Resource(gvr).Namespace(ns).Delete(context.Background(), name, metav1.DeleteOptions{}) }()

		w := NewWatcher(dyn, gvr, 10*time.Second, isAvailableChecker)

		// After a short delay, write Available=True through the STATUS subresource. Its error comes back
		// on writeErr: t.Fatalf from another goroutine is not allowed.
		writeErr := make(chan error, 1)
		go func() {
			time.Sleep(500 * time.Millisecond)
			writeErr <- setAvailableViaStatus(context.Background(), dyn, gvr, ns, name)
		}()

		if err := w.WatchResource(ctx, ns, name); err != nil {
			t.Fatalf("expected eventual ready, got error: %v", err)
		}
		select {
		case err := <-writeErr:
			if err != nil {
				t.Fatalf("the status write failed: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("the status write never returned")
		}

		return ctx
	}).Feature()

	testenv.Test(t, f)
}

// TestWatcher_NotReadyWithoutTheStatusWrite_E2E is BecomesReadyAfterModify's CONTROL (#631): the same
// never-pulled Deployment and the same 10s watcher, with no status write. It must time out, which
// proves that nothing but the test's own write makes the other test pass (no rollout, no controller).
func TestWatcher_NotReadyWithoutTheStatusWrite_E2E(t *testing.T) {
	f := features.New("Watcher Not Ready Without The Status Write - e2e").
		Assess("TimesOutWithoutTheWrite", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
			ns := namespace
			name := "never-written-e2e"

			dyn, err := dynamic.NewForConfig(cfg.Client().RESTConfig())
			if err != nil {
				t.Fatalf("failed to create dynamic client: %v", err)
			}
			_, err = dyn.Resource(gvr).Namespace(ns).Create(ctx, makeDeployment(name, ns, 1, neverPulledImage), metav1.CreateOptions{})
			if err != nil {
				t.Fatalf("failed to create deploy: %v", err)
			}
			defer func() { _ = dyn.Resource(gvr).Namespace(ns).Delete(context.Background(), name, metav1.DeleteOptions{}) }()

			w := NewWatcher(dyn, gvr, 10*time.Second, isAvailableChecker)
			if err := w.WatchResource(ctx, ns, name); err == nil {
				t.Fatalf("control: without the status write the Deployment must never be Available, but the watcher reported ready")
			}
			return ctx
		}).Feature()

	testenv.Test(t, f)
}

func TestWatcher_Timeout_E2E(t *testing.T) {
	os.Setenv("DEBUG", "1")

	f := features.New("Watcher Timeout - e2e").
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			return ctx
		}).Assess("Timeout", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
		ns := namespace
		name := "never-ready-e2e"

		dyn, err := dynamic.NewForConfig(cfg.Client().RESTConfig())
		if err != nil {
			t.Fatalf("failed to create dynamic client: %v", err)
		}

		w := NewWatcher(dyn, gvr, 500*time.Millisecond, isAvailableChecker)

		if err := w.WatchResource(ctx, ns, name); err == nil {
			t.Fatalf("expected timeout error, got nil")
		}

		return ctx
	}).Feature()

	testenv.Test(t, f)
}

func TestWatcher_DeletedDuringWatch_E2E(t *testing.T) {
	os.Setenv("DEBUG", "1")

	f := features.New("Watcher Deleted During Watch - e2e").
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			return ctx
		}).Assess("DeletedDuringWatch", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
		ns := namespace
		name := "will-delete-e2e"

		dyn, err := dynamic.NewForConfig(cfg.Client().RESTConfig())
		if err != nil {
			t.Fatalf("failed to create dynamic client: %v", err)
		}

		// A never-pulled image: a real rollout cannot make it Available before the delete lands.
		_, err = dyn.Resource(gvr).Namespace(ns).Create(ctx, makeDeployment(name, ns, 1, neverPulledImage), metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("failed to create initial deploy: %v", err)
		}

		w := NewWatcher(dyn, gvr, 5*time.Second, isAvailableChecker)

		// delete the object after a short delay to simulate a delete event
		go func() {
			time.Sleep(300 * time.Millisecond)
			_ = dyn.Resource(gvr).Namespace(ns).Delete(context.Background(), name, metav1.DeleteOptions{})
		}()

		if err := w.WatchResource(ctx, ns, name); err == nil {
			t.Fatalf("expected deletion error, got nil")
		}

		return ctx
	}).Feature()

	testenv.Test(t, f)
}

// neverPulledImage is an image no node can pull (.invalid is reserved, RFC 2606), so a Deployment
// that runs it is never Available by its controller's own account.
const neverPulledImage = "registry.invalid/krateo/never-pulled:0"

// makeDeployment builds a Deployment with no status: a Create or main-resource Update cannot set
// status (it is a subresource), so status is written only by the controller or by
// setAvailableViaStatus.
func makeDeployment(name, namespace string, replicas int64, image string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": namespace,
			},
			"spec": map[string]interface{}{
				"replicas": replicas,
				"selector": map[string]interface{}{
					"matchLabels": map[string]interface{}{
						"app": name,
					},
				},
				"template": map[string]interface{}{
					"metadata": map[string]interface{}{
						"labels": map[string]interface{}{
							"app": name,
						},
					},
					"spec": map[string]interface{}{
						"containers": []interface{}{
							map[string]interface{}{
								"name":  "app",
								"image": image,
							},
						},
					},
				},
			},
		},
	}
}

// setAvailableViaStatus writes Available=True through the status subresource, re-reading and retrying
// on a conflict (the Deployment controller writes the same status concurrently). Any other error is
// returned, never swallowed.
func setAvailableViaStatus(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, ns, name string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur, err := dyn.Resource(gvr).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		conds := []interface{}{map[string]interface{}{
			"type":   "Available",
			"status": "True",
			"reason": "SetByWatcherTest",
		}}
		if err := unstructured.SetNestedSlice(cur.Object, conds, "status", "conditions"); err != nil {
			return err
		}
		_, err = dyn.Resource(gvr).Namespace(ns).UpdateStatus(ctx, cur, metav1.UpdateOptions{})
		return err
	})
}

// waitForAvailable polls until the Deployment reports Available=True, or fails after limit.
func waitForAvailable(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, ns, name string, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		cur, err := dyn.Resource(gvr).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			var d appsv1.Deployment
			if runtime.DefaultUnstructuredConverter.FromUnstructured(cur.Object, &d) == nil && isAvailableChecker(&d) {
				return nil
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("%s/%s not Available within %s", ns, name, limit)
}

func isAvailableChecker(u *appsv1.Deployment) bool {
	mu, err := runtime.DefaultUnstructuredConverter.ToUnstructured(u.DeepCopyObject())
	if err != nil {
		return false
	}
	uns := &unstructured.Unstructured{Object: mu}
	conds, found, _ := unstructured.NestedSlice(uns.Object, "status", "conditions")
	if !found {
		fmt.Println("conditions not found")
		return false
	}
	for _, c := range conds {
		fmt.Println("condition:", c)
		m, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if t, _ := m["type"].(string); t == "Available" {
			if s, _ := m["status"].(string); s == "True" {
				return true
			}
		}
	}
	return false
}

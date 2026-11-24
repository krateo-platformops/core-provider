package deploy

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

const idxNS = "krateo-system"

func page(version string) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "composition.krateo.io", Version: version, Resource: "pages"}
}

func envCM(pool string, gvr schema.GroupVersionResource, edit ...func(*unstructured.Unstructured)) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": MemberEnvConfigMapName(gvr), "namespace": idxNS},
		"data":     map[string]any{memberKeyGroup: gvr.Group, memberKeyVersion: gvr.Version, memberKeyResource: gvr.Resource},
	}}
	if pool != "" {
		u.SetLabels(map[string]string{PoolLabel: pool})
	}
	for _, f := range edit {
		f(u)
	}
	return u
}

func draining(u *unstructured.Unstructured) {
	u.SetAnnotations(map[string]string{PoolDrainingAnnotation: "true"})
}

func lease(gvr schema.GroupVersionResource, holder, rv string, secs int64) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "coordination.k8s.io/v1", "kind": "Lease",
		"metadata": map[string]any{"name": gvr.Version + "." + gvr.Resource + "." + gvr.Group, "namespace": idxNS, "resourceVersion": rv},
		"spec":     map[string]any{"holderIdentity": holder, "leaseDurationSeconds": secs},
	}}
}

func idxCluster(objs ...runtime.Object) *fake.FakeDynamicClient {
	return fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		configMapGVR: "ConfigMapList", leaseGVR: "LeaseList", roleGVR: "RoleList", roleBindingGVR: "RoleBindingList",
	}, objs...)
}

func indexKeys(t *testing.T, dyn *fake.FakeDynamicClient, pool string) []string {
	t.Helper()
	u, err := dyn.Resource(configMapGVR).Namespace(idxNS).Get(context.Background(), PoolNames(pool).Index, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	data, _, _ := unstructured.NestedStringMap(u.Object, "data")
	keys := []string{}
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// A member is an env ConfigMap carrying the pool's label, and nothing else is: not one being drained, not
// another pool's, not a ConfigMap that does not configure the Kind x version it is named for, not the
// pool's own ConfigMaps.
func TestPoolMembership_IsTheLiveEnvConfigMaps(t *testing.T) {
	stray := envCM("web", page("v9-0-0"), func(u *unstructured.Unstructured) { u.SetName("someone-elses") })
	noKind := envCM("web", page("v8-0-0"), func(u *unstructured.Unstructured) {
		unstructured.RemoveNestedField(u.Object, "data", memberKeyVersion)
	})
	own := envCM("web", page("v7-0-0"), func(u *unstructured.Unstructured) { u.SetName(PoolNames("web").EnvConfigMap) })
	dyn := idxCluster(envCM("web", page("v1-0-0")), envCM("web", page("v2-0-0")), envCM("web", page("v0-9-0"), draining),
		envCM("other", page("v3-0-0")), envCM("", page("v4-0-0")), stray, noKind, own)
	if err := syncPoolIndex(context.Background(), dyn, idxNS, "web"); err != nil {
		t.Fatal(err)
	}
	if got, want := indexKeys(t, dyn, "web"), []string{"pages-v1-0-0", "pages-v2-0-0"}; !reflect.DeepEqual(got, want) {
		t.Errorf("index = %v, want %v", got, want)
	}
	if got := indexKeys(t, dyn, "other"); got != nil {
		t.Errorf("another pool's index was written: %v", got)
	}
	// What the index names is readable, whoever recomputed it.
	role, err := dyn.Resource(roleGVR).Namespace(idxNS).Get(context.Background(), PoolNames("web").Workload, metav1.GetOptions{})
	if err != nil || !strings.Contains(fmt.Sprint(role.Object["rules"]), "pages-v2-0-0-configmap") {
		t.Errorf("the pool's Role: %v %v", role, err)
	}
	// A draining controller's env ConfigMap stays readable to the pool until it is removed.
	pools, err := listPools(context.Background(), dyn, idxNS)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := pools["web"].envNames(), []string{"pages-v0-9-0-configmap", "pages-v1-0-0-configmap", "pages-v2-0-0-configmap"}; !reflect.DeepEqual(got, want) {
		t.Errorf("the pool may read %v, want %v", got, want)
	}
	if got, err := PoolMemberVersions(context.Background(), dyn, idxNS, "composition.krateo.io", "pages"); err != nil || !reflect.DeepEqual(got, []string{"v0-9-0", "v1-0-0", "v2-0-0", "v3-0-0"}) {
		t.Errorf("PoolMemberVersions = %v, %v", got, err)
	}
}

// A refused index write starts again from the live members: one that joined between the read and the
// write is in what is written, which a retried precomputed index would not have.
func TestSyncPoolIndex_RecomputesUnderConflict(t *testing.T) {
	ctx := context.Background()
	dyn := idxCluster(envCM("web", page("v1-0-0")))
	if err := syncPoolIndex(ctx, dyn, idxNS, "web"); err != nil {
		t.Fatal(err)
	}
	if err := dyn.Tracker().Add(envCM("web", page("v2-0-0"))); err != nil {
		t.Fatal(err)
	}
	conflicts := 0
	dyn.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		if conflicts++; conflicts > 1 {
			return false, nil, nil
		}
		// Another writer, between our read and our write.
		if err := dyn.Tracker().Add(envCM("web", page("v3-0-0"))); err != nil {
			t.Fatal(err)
		}
		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, "cdc-pool-web-members", errors.New("changed"))
	})
	if err := syncPoolIndex(ctx, dyn, idxNS, "web"); err != nil {
		t.Fatal(err)
	}
	if got, want := indexKeys(t, dyn, "web"), []string{"pages-v1-0-0", "pages-v2-0-0", "pages-v3-0-0"}; !reflect.DeepEqual(got, want) {
		t.Errorf("index = %v, want %v", got, want)
	}
	// Unchanged: no write at all.
	before := len(dyn.Actions())
	if err := syncPoolIndex(ctx, dyn, idxNS, "web"); err != nil {
		t.Fatal(err)
	}
	for _, a := range dyn.Actions()[before:] {
		if a.GetResource() == configMapGVR && a.GetVerb() != "list" {
			t.Errorf("an unchanged index was written: %s", a.GetVerb())
		}
	}
	// A conflict that never ends is an error, not a loop.
	if err := dyn.Tracker().Add(envCM("web", page("v5-0-0"))); err != nil {
		t.Fatal(err)
	}
	dyn.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, "x", errors.New("changed"))
	})
	if err := syncPoolIndex(ctx, dyn, idxNS, "web"); !apierrors.IsConflict(err) {
		t.Errorf("err = %v, want the conflict", err)
	}
}

// A failed list is never an empty pool.
func TestPoolIndex_FailedListChangesNothing(t *testing.T) {
	dyn := idxCluster(envCM("web", page("v1-0-0")))
	dyn.PrependReactor("list", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("apiserver unavailable")
	})
	if err := syncPoolIndex(context.Background(), dyn, idxNS, "web"); err == nil {
		t.Error("syncPoolIndex accepted a failed list")
	}
	if _, err := releasePoolMember(context.Background(), dyn, idxNS, page("v1-0-0"), "", time.Second); err == nil {
		t.Error("releasePoolMember read a failed list as a controller no pool runs")
	}
	for _, a := range dyn.Actions() {
		if a.GetVerb() != "list" {
			t.Errorf("%s after a failed list", a.GetVerb())
		}
	}
}

// Leaving: mark, index, then the Lease. Nothing for a controller no pool runs, or that its own pool keeps.
func TestReleasePoolMember(t *testing.T) {
	ctx := context.Background()
	v1 := page("v1-0-0")
	t.Run("marked, dropped, and waited for", func(t *testing.T) {
		t.Parallel()
		dyn := idxCluster(envCM("web", v1), envCM("web", page("v2-0-0")), lease(v1, "cdc-pool-web-0", "1", 15))
		released := make(chan struct{})
		go func() {
			defer close(released)
			for { // the pool's process: once the index has dropped the key, it drains and releases
				u, err := dyn.Resource(configMapGVR).Namespace(idxNS).Get(ctx, PoolNames("web").Index, metav1.GetOptions{})
				if err == nil {
					if data, _, _ := unstructured.NestedStringMap(u.Object, "data"); data["pages-v1-0-0"] == "" {
						break
					}
				}
				time.Sleep(10 * time.Millisecond)
			}
			time.Sleep(700 * time.Millisecond) // draining
			_, _ = dyn.Resource(leaseGVR).Namespace(idxNS).Update(ctx, lease(v1, "", "2", 1), metav1.UpdateOptions{})
		}()
		if err := syncPoolIndex(ctx, dyn, idxNS, "web"); err != nil {
			t.Fatal(err)
		}
		left, err := releasePoolMember(ctx, dyn, idxNS, v1, "", 20*time.Second)
		if err != nil || !reflect.DeepEqual(left, []string{"web"}) {
			t.Fatalf("left %v, %v", left, err)
		}
		select {
		case <-released:
		default:
			t.Error("returned before the Lease was released")
		}
		if got := indexKeys(t, dyn, "web"); !reflect.DeepEqual(got, []string{"pages-v2-0-0"}) {
			t.Errorf("index = %v", got)
		}
		u, err := dyn.Resource(configMapGVR).Namespace(idxNS).Get(ctx, MemberEnvConfigMapName(v1), metav1.GetOptions{})
		if err != nil || u.GetAnnotations()[PoolDrainingAnnotation] == "" {
			t.Errorf("the env ConfigMap is kept and marked: %v %v", u.GetAnnotations(), err)
		}
	})
	t.Run("a restart between the mark and the release still waits", func(t *testing.T) {
		t.Parallel()
		dyn := idxCluster(envCM("web", v1, draining), lease(v1, "cdc-pool-web-0", "1", 3600))
		if _, err := releasePoolMember(ctx, dyn, idxNS, v1, "", time.Second); !errors.Is(err, ErrPoolMemberNotDrained) {
			t.Errorf("err = %v, want ErrPoolMemberNotDrained", err)
		}
	})
	t.Run("an index entry without its ConfigMap is dropped, and its Lease waited for", func(t *testing.T) {
		t.Parallel()
		dyn := idxCluster(envCM("web", v1), lease(v1, "cdc-pool-web-0", "1", 3600))
		if err := syncPoolIndex(ctx, dyn, idxNS, "web"); err != nil {
			t.Fatal(err)
		}
		if err := dyn.Resource(configMapGVR).Namespace(idxNS).Delete(ctx, MemberEnvConfigMapName(v1), metav1.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
		if _, err := releasePoolMember(ctx, dyn, idxNS, v1, "", time.Second); !errors.Is(err, ErrPoolMemberNotDrained) {
			t.Errorf("err = %v, want ErrPoolMemberNotDrained", err)
		}
		if got := indexKeys(t, dyn, "web"); len(got) != 0 {
			t.Errorf("index = %v", got)
		}
	})
	for name, dyn := range map[string]*fake.FakeDynamicClient{
		"a dedicated controller":             idxCluster(envCM("", v1), lease(v1, "pages-v1-0-0-controller-x", "1", 3600)),
		"its own pool keeps it":              idxCluster(envCM("web", v1), lease(v1, "cdc-pool-web-0", "1", 3600)),
		"rejoining the pool it was leaving":  idxCluster(envCM("web", v1, draining), lease(v1, "cdc-pool-web-0", "1", 3600)),
		"another version of its Kind leaves": idxCluster(envCM("other", page("v2-0-0")), lease(v1, "x", "1", 3600)),
	} {
		t.Run("nothing to do: "+name, func(t *testing.T) {
			t.Parallel()
			left, err := releasePoolMember(ctx, dyn, idxNS, v1, "web", time.Second)
			if err != nil || len(left) != 0 {
				t.Errorf("left %v, %v", left, err)
			}
			for _, a := range dyn.Actions() {
				if a.GetVerb() != "list" {
					t.Errorf("%s %s", a.GetVerb(), a.GetResource().Resource)
				}
			}
		})
	}
	t.Run("a leftover entry in another index is dropped without waiting for its own pool's Lease", func(t *testing.T) {
		t.Parallel()
		dyn := idxCluster(envCM("web", v1), lease(v1, "cdc-pool-web-0", "1", 3600), &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": PoolNames("old").Index, "namespace": idxNS, "labels": map[string]any{PoolLabel: "old"}},
			"data":     map[string]any{"pages-v1-0-0": `{"configMap":"pages-v1-0-0-configmap","checksum":"x"}`},
		}})
		left, err := releasePoolMember(ctx, dyn, idxNS, v1, "web", time.Second)
		if err != nil || !reflect.DeepEqual(left, []string{"old"}) {
			t.Fatalf("left %v, %v", left, err)
		}
		if got := indexKeys(t, dyn, "old"); len(got) != 0 {
			t.Errorf("old's index = %v", got)
		}
		u, _ := dyn.Resource(configMapGVR).Namespace(idxNS).Get(ctx, MemberEnvConfigMapName(v1), metav1.GetOptions{})
		if u.GetAnnotations()[PoolDrainingAnnotation] != "" {
			t.Error("the member's own pool's env ConfigMap was marked as leaving")
		}
	})
}

// When a Lease counts as released. "Cannot read" and "still held at the bound" never do.
func TestWaitLeaseReleased(t *testing.T) {
	v1 := page("v1-0-0")
	nn := types.NamespacedName{Namespace: idxNS, Name: "v1-0-0.pages.composition.krateo.io"}
	wait := func(dyn *fake.FakeDynamicClient, d time.Duration) error {
		return waitLeaseReleased(context.Background(), dyn, nn, d)
	}
	for name, dyn := range map[string]*fake.FakeDynamicClient{
		"never taken": idxCluster(), "released": idxCluster(lease(v1, "", "1", 1)),
		"held by a pod that died: its record never changes": idxCluster(lease(v1, "gone", "1", 1)),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			start := time.Now()
			if err := wait(dyn, 20*time.Second); err != nil {
				t.Fatal(err)
			}
			if d := time.Since(start); d < leaseQuiet {
				t.Errorf("released after %s, before it had stayed unheld for %s", d, leaseQuiet)
			}
		})
	}
	t.Run("held and renewed: the gate is kept at the bound, naming the holder", func(t *testing.T) {
		t.Parallel()
		dyn := idxCluster(lease(v1, "cdc-pool-web-0", "1", 1))
		n := 1
		dyn.PrependReactor("get", "leases", func(k8stesting.Action) (bool, runtime.Object, error) {
			n++ // a renewal
			return true, lease(v1, "cdc-pool-web-0", fmt.Sprint(n), 1), nil
		})
		err := wait(dyn, 6*time.Second) // longer than its 1s duration and leaseQuiet
		if !errors.Is(err, ErrPoolMemberNotDrained) || !strings.Contains(err.Error(), "cdc-pool-web-0") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("taken again while it looked released: not released", func(t *testing.T) {
		t.Parallel()
		dyn := idxCluster()
		start := time.Now()
		dyn.PrependReactor("get", "leases", func(k8stesting.Action) (bool, runtime.Object, error) {
			if time.Since(start) < time.Second {
				return true, lease(v1, "", "1", 1), nil
			}
			return true, lease(v1, "the-new-pod", fmt.Sprint(time.Now().UnixNano()), 15), nil
		})
		if err := wait(dyn, 5*time.Second); !errors.Is(err, ErrPoolMemberNotDrained) {
			t.Errorf("err = %v, want ErrPoolMemberNotDrained", err)
		}
	})
	t.Run("forbidden: unobservable, at once (#270)", func(t *testing.T) {
		t.Parallel()
		dyn := idxCluster()
		dyn.PrependReactor("get", "leases", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "coordination.k8s.io", Resource: "leases"}, nn.Name, errors.New("no"))
		})
		if err := wait(dyn, 20*time.Second); !errors.Is(err, ErrDrainUnobservable) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("any other read error is an error", func(t *testing.T) {
		t.Parallel()
		dyn := idxCluster()
		dyn.PrependReactor("get", "leases", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("apiserver unavailable")
		})
		if err := wait(dyn, 20*time.Second); err == nil || errors.Is(err, ErrDrainUnobservable) {
			t.Errorf("err = %v", err)
		}
	})
}

//go:build envtest

// A pool's objects against a REAL apiserver (docs/design/cdc-pooling.md §5c), rendered from the
// shipped chart's own assets exactly as core-provider mounts them: EnsurePool applies the
// ServiceAccount, Role, RoleBinding, process ConfigMap, index and Deployment; DeletePool removes all
// six.
//
//	Run: KUBEBUILDER_ASSETS=$(setup-envtest use 1.36.0 -p path) \
//	go test -tags envtest ./internal/tools/deploy/ -run PoolLifecycleEnvtest -v
package deploy

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestPoolLifecycleEnvtest(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Fatal("KUBEBUILDER_ASSETS is not set: this test exists to run against a real apiserver, and a skip would read as a pass")
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	kube, err := client.New(cfg, client.Options{Scheme: clientgoscheme.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	dyn := dynamic.NewForConfigOrDie(cfg)
	ctx := context.Background()
	const ns = "krateo-system"
	if err := kube.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatal(err)
	}

	manifests := renderShippedChart(t, map[string]any{"cdc": map[string]any{"pool": map[string]any{"overrides": map[string]any{
		"heavy": map[string]any{"workers": 4, "resources": map[string]any{"limits": map[string]any{"memory": "3Gi"}}},
	}}}})
	members := []PoolMember{{GVR: schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1-2-0", Resource: "portals"}}}
	// The member, as Deploy leaves it: its env ConfigMap, labelled with the pool.
	if err := kube.Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: MemberEnvConfigMapName(members[0].GVR), Namespace: ns, Labels: map[string]string{PoolLabel: "heavy"}},
		Data:       map[string]string{memberKeyGroup: "composition.krateo.io", memberKeyVersion: "v1-2-0", memberKeyResource: "portals"},
	}); err != nil {
		t.Fatal(err)
	}
	opts := PoolOptions{
		Namespace: ns, DynClient: dyn, KubeClient: kube,
		Assets: PoolAssets{
			SizingPath:             mountedTemplate(t, manifests, "cdc-deployment.yaml", "pool-sizing.json"),
			ConfigmapTemplatePath:  mountedTemplate(t, manifests, "cdc-configmap.yaml", "pool-configmap.yaml"),
			DeploymentTemplatePath: mountedTemplate(t, manifests, "cdc-deployment.yaml", "pool-deployment.yaml"),
			AuthnURL:               "http://authn.krateo-system.svc:8082",
		},
	}
	if err := EnsurePool(ctx, opts, "heavy"); err != nil {
		t.Fatalf("EnsurePool: %v", err)
	}
	names := PoolNames("heavy")
	saNN := PoolServiceAccount("heavy", ns)

	var dep appsv1.Deployment
	if err := kube.Get(ctx, types.NamespacedName{Namespace: ns, Name: names.Workload}, &dep); err != nil {
		t.Fatalf("the pool's Deployment: %v", err)
	}
	var cm corev1.ConfigMap
	if err := kube.Get(ctx, types.NamespacedName{Namespace: ns, Name: names.EnvConfigMap}, &cm); err != nil {
		t.Fatalf("the pool's process ConfigMap: %v", err)
	}
	pod := dep.Spec.Template.Spec
	if len(pod.Containers) != 1 {
		t.Fatalf("containers = %d, want 1", len(pod.Containers))
	}
	c := pod.Containers[0]

	t.Run("runs as the pool's ServiceAccount, from the pool's process ConfigMap, with no Kind args", func(t *testing.T) {
		if pod.ServiceAccountName != saNN.Name {
			t.Errorf("serviceAccountName = %q, want %q", pod.ServiceAccountName, saNN.Name)
		}
		if len(c.EnvFrom) != 1 || c.EnvFrom[0].ConfigMapRef == nil || c.EnvFrom[0].ConfigMapRef.Name != names.EnvConfigMap {
			t.Errorf("envFrom = %+v, want only %s", c.EnvFrom, names.EnvConfigMap)
		}
		for _, a := range append(append([]string{}, c.Command...), c.Args...) {
			if strings.HasPrefix(a, "-group") || strings.HasPrefix(a, "-version") || strings.HasPrefix(a, "-resource") {
				t.Errorf("pool container carries a Kind argument %q", a)
			}
		}
		if cm.Data["COMPOSITION_CONTROLLER_POOL_INDEX"] != names.Index {
			t.Errorf("process ConfigMap POOL_INDEX = %q, want %q (it is what puts the cdc in pool mode)", cm.Data["COMPOSITION_CONTROLLER_POOL_INDEX"], names.Index)
		}
	})

	t.Run("sized by the deep-merged override", func(t *testing.T) {
		got := map[string]string{
			"requests.cpu":    c.Resources.Requests.Cpu().String(),
			"requests.memory": c.Resources.Requests.Memory().String(),
			"limits.memory":   c.Resources.Limits.Memory().String(),
		}
		want := map[string]string{"requests.cpu": "100m", "requests.memory": "660Mi", "limits.memory": "3Gi"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("resources = %v, want %v", got, want)
		}
	})

	t.Run("the authn token is always mounted", func(t *testing.T) {
		mounted := false
		for _, m := range c.VolumeMounts {
			if m.Name == "authn-serviceaccount-token" && m.MountPath == "/var/run/secrets/krateo.io/serviceaccount" {
				mounted = true
			}
		}
		if !mounted {
			t.Errorf("no authn token mount in %+v", c.VolumeMounts)
		}
	})

	t.Run("the pod carries the process ConfigMap's checksum", func(t *testing.T) {
		if got, want := dep.Spec.Template.Annotations["krateo.io/config-checksum"], configChecksum(cm.Data); got == "" || got != want {
			t.Errorf("config-checksum = %q, want %q", got, want)
		}
	})

	t.Run("the index lists the member, and the Role grants only it", func(t *testing.T) {
		var index corev1.ConfigMap
		if err := kube.Get(ctx, types.NamespacedName{Namespace: ns, Name: names.Index}, &index); err != nil {
			t.Fatal(err)
		}
		raw, ok := index.Data[MemberKey(members[0].GVR)]
		if !ok {
			t.Fatalf("index %v does not list %s", index.Data, MemberKey(members[0].GVR))
		}
		var entry PoolIndexEntry
		if err := json.Unmarshal([]byte(raw), &entry); err != nil {
			t.Fatal(err)
		}
		var role rbacv1.Role
		if err := kube.Get(ctx, types.NamespacedName{Namespace: ns, Name: names.Workload}, &role); err != nil {
			t.Fatal(err)
		}
		want, _ := PoolRBAC("heavy", ns, []string{MemberEnvConfigMapName(members[0].GVR)})
		if !reflect.DeepEqual(role.Rules, want.Rules) {
			t.Errorf("Role rules %+v, want %+v", role.Rules, want.Rules)
		}
	})

	// DeletePool removes every object EnsurePool applied, and a second call is a no-op.
	for i := 0; i < 2; i++ {
		if err := DeletePool(ctx, opts, "heavy"); err != nil {
			t.Fatalf("DeletePool #%d: %v", i+1, err)
		}
	}
	left := []string{}
	for _, o := range []struct {
		name string
		obj  client.Object
	}{
		{"Deployment", &appsv1.Deployment{}},
		{"ConfigMap " + names.Index, &corev1.ConfigMap{}},
		{"ConfigMap " + names.EnvConfigMap, &corev1.ConfigMap{}},
		{"RoleBinding", &rbacv1.RoleBinding{}},
		{"Role", &rbacv1.Role{}},
		{"ServiceAccount", &corev1.ServiceAccount{}},
	} {
		name := names.Workload
		if strings.HasPrefix(o.name, "ConfigMap ") {
			name = strings.TrimPrefix(o.name, "ConfigMap ")
		}
		err := kube.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, o.obj)
		if err == nil {
			if o.obj.GetDeletionTimestamp() == nil {
				left = append(left, o.name)
			}
		} else if !apierrors.IsNotFound(err) {
			t.Fatalf("reading %s: %v", o.name, err)
		}
	}
	if len(left) > 0 {
		t.Errorf("DeletePool left %v", left)
	}
}

// hookTransport runs before, once, ahead of the first request it matches.
type hookTransport struct {
	next   http.RoundTripper
	match  func(*http.Request) bool
	before func()
	once   sync.Once
}

func (h *hookTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if h.match(r) {
		h.once.Do(h.before)
	}
	return h.next.RoundTrip(r)
}

// Two writers of one pool, on a real apiserver. Another PROCESS (no shared lock) writes the index between
// this one's read and its write: the write is refused and recomputed, so its member is kept. Another
// WORKER joins while this one deletes the pool: it waits, and the pool it joined is whole.
func TestPoolWritersEnvtest(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Fatal("KUBEBUILDER_ASSETS is not set: this test exists to run against a real apiserver, and a skip would read as a pass")
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	kube, err := client.New(cfg, client.Options{Scheme: clientgoscheme.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const ns = "krateo-system"
	if err := kube.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatal(err)
	}
	plain := dynamic.NewForConfigOrDie(cfg)
	hooked := func(match func(*http.Request) bool, before func()) dynamic.Interface {
		c := rest.CopyConfig(cfg)
		c.WrapTransport = func(rt http.RoundTripper) http.RoundTripper {
			return &hookTransport{next: rt, match: match, before: before}
		}
		return dynamic.NewForConfigOrDie(c)
	}
	member := func(pool, version string) {
		t.Helper()
		gvr := schema.GroupVersionResource{Group: "composition.krateo.io", Version: version, Resource: "pages"}
		if err := kube.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: MemberEnvConfigMapName(gvr), Namespace: ns, Labels: map[string]string{PoolLabel: pool}},
			Data:       map[string]string{memberKeyGroup: gvr.Group, memberKeyVersion: gvr.Version, memberKeyResource: gvr.Resource},
		}); err != nil {
			t.Fatal(err)
		}
	}
	index := func(pool string) []string {
		t.Helper()
		var cm corev1.ConfigMap
		if err := kube.Get(ctx, types.NamespacedName{Namespace: ns, Name: PoolNames(pool).Index}, &cm); err != nil {
			t.Fatal(err)
		}
		return PoolIndexMembers(cm)
	}

	t.Run("another process writes the index first", func(t *testing.T) {
		member("web", "v1-0-0")
		if err := syncPoolIndex(ctx, plain, ns, "web"); err != nil {
			t.Fatal(err)
		}
		member("web", "v2-0-0")
		hit := false
		dyn := hooked(func(r *http.Request) bool {
			return r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/configmaps/"+PoolNames("web").Index)
		}, func() {
			hit = true
			member("web", "v3-0-0")
			if err := syncPoolIndex(ctx, plain, ns, "web"); err != nil {
				t.Error(err)
			}
		})
		if err := syncPoolIndex(ctx, dyn, ns, "web"); err != nil {
			t.Fatal(err)
		}
		if !hit {
			t.Fatal("control: the other writer never ran")
		}
		if got, want := index("web"), []string{"pages-v1-0-0", "pages-v2-0-0", "pages-v3-0-0"}; !reflect.DeepEqual(got, want) {
			t.Errorf("index = %v, want %v", got, want)
		}
	})

	t.Run("a member joins while the pool is being deleted", func(t *testing.T) {
		manifests := renderShippedChart(t, nil)
		opts := PoolOptions{Namespace: ns, DynClient: plain, KubeClient: kube, Assets: PoolAssets{
			SizingPath:             mountedTemplate(t, manifests, "cdc-deployment.yaml", "pool-sizing.json"),
			ConfigmapTemplatePath:  mountedTemplate(t, manifests, "cdc-configmap.yaml", "pool-configmap.yaml"),
			DeploymentTemplatePath: mountedTemplate(t, manifests, "cdc-deployment.yaml", "pool-deployment.yaml"),
			AuthnURL:               "http://authn",
		}}
		if err := EnsurePool(ctx, opts, "late"); err != nil {
			t.Fatal(err)
		}
		joined := make(chan error, 1)
		deleting := opts
		deleting.DynClient = hooked(func(r *http.Request) bool {
			return r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/rolebindings/cdc-pool-late")
		}, func() {
			go func() {
				member("late", "v9-0-0")
				joined <- EnsurePool(ctx, opts, "late")
			}()
			time.Sleep(time.Second) // long enough for it to have finished, were it not waiting
		})
		if err := ReconcilePools(ctx, deleting, nil); err != nil {
			t.Fatal(err)
		}
		if err := <-joined; err != nil {
			t.Fatal(err)
		}
		if got, want := index("late"), []string{"pages-v9-0-0"}; !reflect.DeepEqual(got, want) {
			t.Errorf("index = %v, want %v", got, want)
		}
		names := PoolNames("late")
		for what, o := range map[string]client.Object{"Deployment": &appsv1.Deployment{}, "Role": &rbacv1.Role{}, "RoleBinding": &rbacv1.RoleBinding{}, "ServiceAccount": &corev1.ServiceAccount{}} {
			if err := kube.Get(ctx, types.NamespacedName{Namespace: ns, Name: names.Workload}, o); err != nil || o.GetDeletionTimestamp() != nil {
				t.Errorf("the joined pool has no %s: %v", what, err)
			}
		}
	})
}

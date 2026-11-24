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
	"os"
	"reflect"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
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

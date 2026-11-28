//go:build envtest

// #660: a release is not complete before the MEMBER's own Quiet, whatever this process's timings are,
// against a real apiserver. The member's timings are the ones core-provider rendered into its env
// ConfigMap; a process on shorter ones (an old pod during an upgrade) must still wait for them.
//
//	Run: KUBEBUILDER_ASSETS=$(setup-envtest use 1.36.0 -p path) \
//	go test -tags envtest ./internal/tools/deploy/ -run PoolMemberQuietEnvtest -v
package deploy

import (
	"context"
	"os"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/krateo-platformops/core-provider/internal/tools/objects/templates"
)

func TestPoolMemberQuietEnvtest(t *testing.T) {
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
	// This process: 3s/2s/0.5s, Quiet 1.25s. The member: 4s/3s/1s, Quiet 2.5s.
	process := OwnershipLeaseTimings{Duration: 3 * time.Second, RenewDeadline: 2 * time.Second, RetryPeriod: 500 * time.Millisecond}
	release := func(version string, extra map[string]string) time.Duration {
		t.Helper()
		gvr := schema.GroupVersionResource{Group: "composition.krateo.io", Version: version, Resource: "pages"}
		data := map[string]string{memberKeyGroup: gvr.Group, memberKeyVersion: gvr.Version, memberKeyResource: gvr.Resource}
		for k, v := range extra {
			data[k] = v
		}
		if err := kube.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: MemberEnvConfigMapName(gvr), Namespace: ns, Labels: map[string]string{PoolLabel: "web"}},
			Data:       data,
		}); err != nil {
			t.Fatal(err)
		}
		if err := syncPoolIndex(ctx, dyn, ns, "web"); err != nil {
			t.Fatal(err)
		}
		holder, secs := "", int32(4)
		if err := kube.Create(ctx, &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Name: templates.ControllerLeaseName(gvr.Group, gvr.Version, gvr.Resource), Namespace: ns},
			Spec:       coordinationv1.LeaseSpec{HolderIdentity: &holder, LeaseDurationSeconds: &secs},
		}); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		left, err := releasePoolMember(ctx, dyn, ns, gvr, "", 20*time.Second, process)
		if err != nil || len(left) != 1 {
			t.Fatalf("%s: left %v, %v", version, left, err)
		}
		return time.Since(start)
	}

	if d := release("v1-0-0", map[string]string{
		EnvOwnershipLeaseDuration: "4s", EnvOwnershipLeaseRenewDeadline: "3s", EnvOwnershipLeaseRetryPeriod: "1s",
	}); d < 2500*time.Millisecond {
		t.Errorf("a member on 4s/3s/1s was read as released after %s, before its own Quiet of 2.5s", d)
	}
	// Control: a member rendered before #660 (no keys) is the process's Quiet, 1.25s, and no longer.
	if d := release("v2-0-0", nil); d < 1250*time.Millisecond || d >= 2500*time.Millisecond {
		t.Errorf("a member with no timings was released after %s, want [1.25s, 2.5s): this process's Quiet", d)
	}
}

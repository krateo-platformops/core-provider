//go:build envtest

// core-provider#697, against a real apiserver: a pass whose key is unchanged does not call
// chart-inspector, and still restores a derived grant somebody deleted.
//
//	Run: KUBEBUILDER_ASSETS=$(setup-envtest use -p path 1.36.0) \
//	go test -tags envtest ./internal/composition/ -run Envtest_DerivedRBACCache -v
package composition

import (
	"context"
	"os"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestEnvtest_DerivedRBACCache_RestoresADeletedGrantWithoutACall(t *testing.T) {
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

	cs := kubernetes.NewForConfigOrDie(cfg)
	if _, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ns1"}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	d := newInspectorDouble(t)
	p := newRBACPass(t, d)
	real := dynamic.NewForConfigOrDie(cfg)
	name := p.release + "-krateo-rbac"
	exists := func() (cr, role bool) {
		_, e1 := cs.RbacV1().ClusterRoles().Get(ctx, name, metav1.GetOptions{})
		_, e2 := cs.RbacV1().Roles("ns1").Get(ctx, name, metav1.GetOptions{})
		return e1 == nil, e2 == nil
	}
	pass := func() {
		t.Helper()
		if _, err := p.h.applyDerivedRBACWith(ctx, p.mg, p.pkg, real, p.release, &rbacCacheUse{revision: p.revision}); err != nil {
			t.Fatalf("pass: %v", err)
		}
	}

	pass()
	if cr, role := exists(); !cr || !role {
		t.Fatalf("first pass did not apply: clusterrole=%v role=%v", cr, role)
	}

	if err := cs.RbacV1().ClusterRoles().Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := cs.RbacV1().Roles("ns1").Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if cr, role := exists(); cr || role {
		t.Fatalf("setup: grants still present: clusterrole=%v role=%v", cr, role)
	}

	pass()
	if cr, role := exists(); !cr || !role {
		t.Errorf("the deleted grants were not restored: clusterrole=%v role=%v", cr, role)
	}
	if got := d.calls.Load(); got != 1 {
		t.Errorf("restoring cost a chart-inspector call: %d calls, want 1", got)
	}

	// an EDITED grant is put back too
	cr, err := cs.RbacV1().ClusterRoles().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cr.Rules = nil
	if _, err := cs.RbacV1().ClusterRoles().Update(ctx, cr, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	pass()
	cr, err = cs.RbacV1().ClusterRoles().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(cr.Rules) == 0 {
		t.Error("an emptied ClusterRole was not restored")
	}
	if got := d.calls.Load(); got != 1 {
		t.Errorf("%d chart-inspector calls in all, want 1", got)
	}
}

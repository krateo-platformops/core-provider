//go:build envtest

// ApplyReport.Written against a real apiserver: only a real apiserver tells a no-op update from a
// write (the fake dynamic client never moves resourceVersion, so every update would read as a
// no-op there). The composition controller marks a pass acted only when Written > 0 (Diego,
// 2026-11-14: "mark only on a real write"), so the count must be 0 on an unchanged resync, including
// the RoleBinding, which ApplyRBAC always sends as an Update.
//
//	Run: KUBEBUILDER_ASSETS=$(setup-envtest use -p path 1.36.0) \
//	go test -tags envtest ./internal/tools/rbac/ -run Envtest_Written -v
package rbac

import (
	"context"
	"os"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestEnvtest_WrittenCountsOnlyRealWrites(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS unset")
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest apiserver: %v", err)
	}
	defer func() { _ = env.Stop() }()
	if _, err := kubernetes.NewForConfigOrDie(cfg).CoreV1().Namespaces().Create(context.Background(),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "apps"}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	inst := NewRBACInstaller(dynamic.NewForConfigOrDie(cfg))

	desired := func(verbs ...string) *RBAC {
		return &RBAC{
			ClusterRole: &rbacv1.ClusterRole{
				ObjectMeta: metav1.ObjectMeta{Name: "fireworks-krateo-rbac"},
				Rules:      []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"get"}}},
			},
			ClusterRoleBinding: &rbacv1.ClusterRoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: "fireworks-krateo-rbac"},
				RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "fireworks-krateo-rbac"},
				Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: "fireworks", Namespace: "apps"}},
			},
			Namespaced: map[string]Namespaced{"apps": {
				Role: &rbacv1.Role{
					ObjectMeta: metav1.ObjectMeta{Name: "fireworks-krateo-rbac", Namespace: "apps"},
					Rules:      []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: verbs}},
				},
				RoleBinding: &rbacv1.RoleBinding{
					ObjectMeta: metav1.ObjectMeta{Name: "fireworks-krateo-rbac", Namespace: "apps"},
					RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: "fireworks-krateo-rbac"},
					Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: "fireworks", Namespace: "apps"}},
				},
			}},
		}
	}

	r, err := inst.ApplyRBAC(desired("get"))
	if err != nil || r.Written != 4 {
		t.Fatalf("first apply: written=%d err=%v, want 4 (all created)", r.Written, err)
	}
	r, err = inst.ApplyRBAC(desired("get"))
	if err != nil || r.Written != 0 {
		t.Fatalf("unchanged re-apply (a resync): written=%d err=%v, want 0, though the bindings are always sent as an Update", r.Written, err)
	}
	r, err = inst.ApplyRBAC(desired("get", "list"))
	if err != nil || r.Written != 1 {
		t.Fatalf("one new rule: written=%d err=%v, want 1 (the Role)", r.Written, err)
	}
}

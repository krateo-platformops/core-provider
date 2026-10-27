package rbac

import (
	"context"
	"os"
	"testing"

	"github.com/krateo-platformops/plumbing/e2e"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
	"sigs.k8s.io/e2e-framework/pkg/env"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/envfuncs"
	"sigs.k8s.io/e2e-framework/pkg/features"
	"sigs.k8s.io/e2e-framework/support/kind"

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
	//
	// This suite is the one that actually did it: alone among the eleven it has NO build tag, so
	// a plain `go test ./...` reaches it.
	restore := e2e.PinKubeconfig("cdc-rbac")

	namespace = "demo-system"
	altNamespace := "krateo-system"
	clusterName = "krateo"
	testenv = env.New()

	testenv.Setup(
		envfuncs.CreateCluster(kind.NewProvider(), clusterName),
		e2e.CreateNamespace(namespace),
		e2e.CreateNamespace(altNamespace),

		func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
			r, err := resources.New(cfg.Client().RESTConfig())
			if err != nil {
				return ctx, err
			}

			r.WithNamespace(namespace)

			return ctx, nil
		},
	).Finish(
		envfuncs.DeleteNamespace(namespace),
		envfuncs.DestroyCluster(clusterName),
	)

	code := testenv.Run(m)
	restore() // not deferred: os.Exit does not run defers
	os.Exit(code)
}

func TestApplyRole(t *testing.T) {
	f := features.New("Setup").
		Setup(e2e.Logger("test")).
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			return ctx
		}).Assess("Install", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		dyn := dynamic.NewForConfigOrDie(cfg.Client().RESTConfig())

		installer := &RBACInstaller{DynamicClient: dyn}

		role := &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-role",
				Namespace: "default",
			},
		}

		_, err := installer.ApplyRole(context.Background(), role)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		return ctx
	}).Feature()

	testenv.Test(t, f)
}

func TestApplyRole_IncrementalRules(t *testing.T) {
	f := features.New("Setup").
		Setup(e2e.Logger("test")).
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			return ctx
		}).Assess("Install", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		dyn := dynamic.NewForConfigOrDie(cfg.Client().RESTConfig())

		installer := &RBACInstaller{DynamicClient: dyn}
		// Create initial role
		initialRole := &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-role-incremental",
				Namespace: "default",
			},
			Rules: []rbacv1.PolicyRule{
				{
					Verbs:     []string{"get"},
					APIGroups: []string{""},
					Resources: []string{"pods"},
				},
			},
		}

		_, err := installer.ApplyRole(context.Background(), initialRole)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}

		// Apply new role with the same name but different rules
		newRole := &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-role-incremental",
				Namespace: "default",
			},
			Rules: []rbacv1.PolicyRule{
				{
					Verbs:     []string{"list"},
					APIGroups: []string{""},
					Resources: []string{"pods"},
				},
			},
		}

		_, err = installer.ApplyRole(context.Background(), newRole)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}

		// Retrieve the role from the cluster using dynamic client directly
		res, err := dyn.Resource(schema.GroupVersionResource{
			Group:    "rbac.authorization.k8s.io",
			Version:  "v1",
			Resource: "roles",
		}).Namespace("default").Get(ctx, "test-role-incremental", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("failed to get role: %v", err)
		}

		rules, _, _ := unstructured.NestedSlice(res.Object, "rules")
		if len(rules) != 2 {
			t.Errorf("expected 2 role rules, got %v", len(rules))
		}
		return ctx
	}).Feature()

	testenv.Test(t, f)
}

func TestApplyRole_Idempotency(t *testing.T) {
	f := features.New("Setup").
		Setup(e2e.Logger("test")).
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			return ctx
		}).Assess("Install", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		dyn := dynamic.NewForConfigOrDie(cfg.Client().RESTConfig())

		installer := &RBACInstaller{DynamicClient: dyn}
		// Create initial role
		role := &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-role-idempotency",
				Namespace: "default",
			},
			Rules: []rbacv1.PolicyRule{
				{
					Verbs:     []string{"get"},
					APIGroups: []string{""},
					Resources: []string{"pods"},
				},
			},
		}

		// Apply twice
		_, err := installer.ApplyRole(context.Background(), role)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		_, err = installer.ApplyRole(context.Background(), role)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}

		// Retrieve and check
		res, err := dyn.Resource(schema.GroupVersionResource{
			Group:    "rbac.authorization.k8s.io",
			Version:  "v1",
			Resource: "roles",
		}).Namespace("default").Get(ctx, "test-role-idempotency", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("failed to get role: %v", err)
		}

		rules, _, _ := unstructured.NestedSlice(res.Object, "rules")
		if len(rules) != 1 {
			t.Errorf("expected 1 role rule, got %v", len(rules))
		}
		return ctx
	}).Feature()

	testenv.Test(t, f)
}
func TestApplyClusterRole(t *testing.T) {
	f := features.New("Setup").
		Setup(e2e.Logger("test")).
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			return ctx
		}).Assess("Install", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		dyn := dynamic.NewForConfigOrDie(cfg.Client().RESTConfig())

		installer := &RBACInstaller{DynamicClient: dyn}

		clusterRole := &rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-clusterrole",
			},
		}

		_, err := installer.ApplyClusterRole(context.Background(), clusterRole)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		return ctx
	}).Feature()

	testenv.Test(t, f)
}

func TestApplyClusterRoleBinding(t *testing.T) {
	f := features.New("Setup").
		Setup(e2e.Logger("test")).
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			return ctx
		}).Assess("Install", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		dyn := dynamic.NewForConfigOrDie(cfg.Client().RESTConfig())

		installer := &RBACInstaller{DynamicClient: dyn}

		clusterRoleBinding := &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-clusterrolebinding",
			},
			RoleRef: rbacv1.RoleRef{
				APIGroup: "rbac.authorization.k8s.io",
				Kind:     "ClusterRole",
				Name:     "test-clusterrole",
			},
			Subjects: []rbacv1.Subject{
				{
					Kind:      "User",
					Name:      "test-user",
					Namespace: "default",
				},
			},
		}

		_, err := installer.ApplyClusterRoleBinding(context.Background(), clusterRoleBinding)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		return ctx
	}).Feature()

	testenv.Test(t, f)
}

func TestApplyRoleBinding(t *testing.T) {
	f := features.New("Setup").
		Setup(e2e.Logger("test")).
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			return ctx
		}).Assess("Install", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		dyn := dynamic.NewForConfigOrDie(cfg.Client().RESTConfig())

		installer := &RBACInstaller{DynamicClient: dyn}

		roleBinding := &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-rolebinding",
				Namespace: "default",
			},
			RoleRef: rbacv1.RoleRef{
				APIGroup: "rbac.authorization.k8s.io",
				Kind:     "Role",
				Name:     "test-role",
			},
			Subjects: []rbacv1.Subject{
				{
					Kind:      "User",
					Name:      "test-user",
					Namespace: "default",
				},
			},
		}

		_, err := installer.ApplyRoleBinding(context.Background(), roleBinding)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		return ctx
	}).Feature()

	testenv.Test(t, f)
}

func TestApplyRole_NamespaceNotFound(t *testing.T) {
	f := features.New("Setup").
		Setup(e2e.Logger("test")).
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			return ctx
		}).Assess("Install", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		dyn := dynamic.NewForConfigOrDie(cfg.Client().RESTConfig())

		installer := &RBACInstaller{DynamicClient: dyn}

		role := &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-role",
				Namespace: "non-existent-namespace",
			},
		}

		_, err := installer.ApplyRole(context.Background(), role)
		if err == nil {
			t.Fatalf("expected error, got none")
		}
		return ctx
	}).Feature()

	testenv.Test(t, f)
}

func TestApplyClusterRole_IncrementalRules(t *testing.T) {
	f := features.New("Setup").
		Setup(e2e.Logger("test")).
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			return ctx
		}).Assess("Install", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		dyn := dynamic.NewForConfigOrDie(cfg.Client().RESTConfig())

		installer := &RBACInstaller{DynamicClient: dyn}
		// Create initial cluster role
		initialClusterRole := &rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-clusterrole-incremental",
			},
			Rules: []rbacv1.PolicyRule{
				{
					Verbs:     []string{"get"},
					APIGroups: []string{""},
					Resources: []string{"pods"},
				},
			},
		}

		_, err := installer.ApplyClusterRole(context.Background(), initialClusterRole)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}

		// Apply new cluster role with the same name but different rules
		newClusterRole := &rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-clusterrole-incremental",
			},
			Rules: []rbacv1.PolicyRule{
				{
					Verbs:     []string{"list"},
					APIGroups: []string{""},
					Resources: []string{"pods"},
				},
			},
		}

		_, err = installer.ApplyClusterRole(context.Background(), newClusterRole)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}

		// Retrieve the cluster role from the cluster using dynamic client directly
		res, err := dyn.Resource(schema.GroupVersionResource{
			Group:    "rbac.authorization.k8s.io",
			Version:  "v1",
			Resource: "clusterroles",
		}).Get(ctx, "test-clusterrole-incremental", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("failed to get cluster role: %v", err)
		}

		rules, _, _ := unstructured.NestedSlice(res.Object, "rules")
		if len(rules) != 2 {
			t.Errorf("expected 2 cluster role rules, got %v", len(rules))
		}
		return ctx
	}).Feature()

	testenv.Test(t, f)
}

func TestApplyRoleBinding_NamespaceNotFound(t *testing.T) {
	f := features.New("Setup").
		Setup(e2e.Logger("test")).
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			return ctx
		}).Assess("Install", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		dyn := dynamic.NewForConfigOrDie(cfg.Client().RESTConfig())

		installer := &RBACInstaller{DynamicClient: dyn}

		roleBinding := &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-rolebinding",
				Namespace: "non-existent-namespace",
			},
			RoleRef: rbacv1.RoleRef{
				APIGroup: "rbac.authorization.k8s.io",
				Kind:     "Role",
				Name:     "test-role",
			},
			Subjects: []rbacv1.Subject{
				{
					Kind:      "User",
					Name:      "test-user",
					Namespace: "default",
				},
			},
		}

		_, err := installer.ApplyRoleBinding(context.Background(), roleBinding)
		if err == nil {
			t.Fatalf("expected error, got none")
		}
		return ctx
	}).Feature()

	testenv.Test(t, f)
}
func TestApplyRBAC(t *testing.T) {
	f := features.New("Setup").
		Setup(e2e.Logger("test")).
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			return ctx
		}).Assess("Install", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		dyn := dynamic.NewForConfigOrDie(cfg.Client().RESTConfig())

		installer := &RBACInstaller{DynamicClient: dyn}

		rbac := &RBAC{
			ClusterRole: &rbacv1.ClusterRole{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-clusterrole",
				},
			},
			ClusterRoleBinding: &rbacv1.ClusterRoleBinding{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-clusterrolebinding",
				},
				RoleRef: rbacv1.RoleRef{
					APIGroup: "rbac.authorization.k8s.io",
					Kind:     "ClusterRole",
					Name:     "test-clusterrole",
				},
				Subjects: []rbacv1.Subject{
					{
						Kind: "User",
						Name: "test-user",
					},
				},
			},
			Namespaces: []*corev1.Namespace{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test",
					},
				},
			},
			Namespaced: map[string]Namespaced{
				"default": {
					Role: &rbacv1.Role{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "test-role",
							Namespace: "default",
						},
					},
					RoleBinding: &rbacv1.RoleBinding{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "test-rolebinding",
							Namespace: "default",
						},
						RoleRef: rbacv1.RoleRef{
							APIGroup: "rbac.authorization.k8s.io",
							Kind:     "Role",
							Name:     "test-role",
						},
						Subjects: []rbacv1.Subject{
							{
								Kind:      "User",
								Name:      "test-user",
								Namespace: "default",
							},
						},
					},
				},
			},
		}

		err := installer.ApplyRBAC(rbac)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		return ctx
	}).Feature()

	testenv.Test(t, f)
}

func TestApplyRBAC_IncrementalRules(t *testing.T) {
	f := features.New("Setup").
		Setup(e2e.Logger("test")).
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			return ctx
		}).Assess("Install", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		dyn := dynamic.NewForConfigOrDie(cfg.Client().RESTConfig())

		installer := &RBACInstaller{DynamicClient: dyn}

		// Create initial RBAC
		initialRBAC := &RBAC{
			ClusterRole: &rbacv1.ClusterRole{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-clusterrole-rbac-incremental",
				},
				Rules: []rbacv1.PolicyRule{
					{
						Verbs:     []string{"get"},
						APIGroups: []string{""},
						Resources: []string{"pods"},
					},
				},
			},
			ClusterRoleBinding: &rbacv1.ClusterRoleBinding{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-clusterrolebinding-rbac-incremental",
				},
				RoleRef: rbacv1.RoleRef{
					APIGroup: "rbac.authorization.k8s.io",
					Kind:     "ClusterRole",
					Name:     "test-clusterrole-rbac-incremental",
				},
				Subjects: []rbacv1.Subject{
					{
						Kind: "User",
						Name: "test-user",
					},
				},
			},
			Namespaced: map[string]Namespaced{
				"default": {
					Role: &rbacv1.Role{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "test-role-rbac-incremental",
							Namespace: "default",
						},
						Rules: []rbacv1.PolicyRule{
							{
								Verbs:     []string{"get"},
								APIGroups: []string{""},
								Resources: []string{"pods"},
							},
						},
					},
					RoleBinding: &rbacv1.RoleBinding{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "test-rolebinding-rbac-incremental",
							Namespace: "default",
						},
						RoleRef: rbacv1.RoleRef{
							APIGroup: "rbac.authorization.k8s.io",
							Kind:     "Role",
							Name:     "test-role-rbac-incremental",
						},
						Subjects: []rbacv1.Subject{
							{
								Kind:      "User",
								Name:      "test-user",
								Namespace: "default",
							},
						},
					},
				},
			},
		}

		err := installer.ApplyRBAC(initialRBAC)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}

		// Apply new RBAC with the same names but different rules
		newRBAC := &RBAC{
			ClusterRole: &rbacv1.ClusterRole{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-clusterrole-rbac-incremental",
				},
				Rules: []rbacv1.PolicyRule{
					{
						Verbs:     []string{"list"},
						APIGroups: []string{""},
						Resources: []string{"pods"},
					},
				},
			},
			ClusterRoleBinding: &rbacv1.ClusterRoleBinding{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-clusterrolebinding-rbac-incremental",
				},
				RoleRef: rbacv1.RoleRef{
					APIGroup: "rbac.authorization.k8s.io",
					Kind:     "ClusterRole",
					Name:     "test-clusterrole-rbac-incremental",
				},
				Subjects: []rbacv1.Subject{
					{
						Kind: "User",
						Name: "test-user",
					},
				},
			},
			Namespaced: map[string]Namespaced{
				"default": {
					Role: &rbacv1.Role{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "test-role-rbac-incremental",
							Namespace: "default",
						},
						Rules: []rbacv1.PolicyRule{
							{
								Verbs:     []string{"list"},
								APIGroups: []string{""},
								Resources: []string{"pods"},
							},
						},
					},
					RoleBinding: &rbacv1.RoleBinding{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "test-rolebinding-rbac-incremental",
							Namespace: "default",
						},
						RoleRef: rbacv1.RoleRef{
							APIGroup: "rbac.authorization.k8s.io",
							Kind:     "Role",
							Name:     "test-role-rbac-incremental",
						},
						Subjects: []rbacv1.Subject{
							{
								Kind:      "User",
								Name:      "test-user",
								Namespace: "default",
							},
						},
					},
				},
			},
		}

		err = installer.ApplyRBAC(newRBAC)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}

		// Retrieve the RBAC from the cluster using dynamic client directly
		resCR, err := dyn.Resource(schema.GroupVersionResource{
			Group:    "rbac.authorization.k8s.io",
			Version:  "v1",
			Resource: "clusterroles",
		}).Get(ctx, "test-clusterrole-rbac-incremental", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("failed to get cluster role: %v", err)
		}

		crRules, _, _ := unstructured.NestedSlice(resCR.Object, "rules")
		if len(crRules) != 2 {
			t.Errorf("expected 2 cluster role rules, got %v", len(crRules))
		}

		resR, err := dyn.Resource(schema.GroupVersionResource{
			Group:    "rbac.authorization.k8s.io",
			Version:  "v1",
			Resource: "roles",
		}).Namespace("default").Get(ctx, "test-role-rbac-incremental", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("failed to get role: %v", err)
		}

		rRules, _, _ := unstructured.NestedSlice(resR.Object, "rules")
		if len(rRules) != 2 {
			t.Errorf("expected 2 role rules, got %v", len(rRules))
		}

		return ctx
	}).Feature()

	testenv.Test(t, f)
}

func TestUninstallRole(t *testing.T) {
	f := features.New("Uninstall").
		Setup(e2e.Logger("test")).
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			return ctx
		}).Assess("Uninstall", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		dyn := dynamic.NewForConfigOrDie(cfg.Client().RESTConfig())

		installer := &RBACInstaller{DynamicClient: dyn}

		role := &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-role",
				Namespace: "default",
			},
		}

		// Apply the role first
		_, err := installer.ApplyRole(context.Background(), role)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}

		// Uninstall the role
		err = installer.DeleteRole(context.Background(), role.Namespace, role.Name)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}

		// Verify the role is deleted
		_, err = dyn.Resource(
			schema.GroupVersionResource{
				Group:    "rbac.authorization.k8s.io",
				Version:  "v1",
				Resource: "roles",
			},
		).Namespace(role.Namespace).Get(ctx, role.Name, metav1.GetOptions{})
		if !errors.IsNotFound(err) {
			t.Fatalf("expected not found error, got none")
		}
		return ctx
	}).Feature()

	testenv.Test(t, f)
}

func TestUninstallClusterRole(t *testing.T) {
	f := features.New("Uninstall").
		Setup(e2e.Logger("test")).
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			return ctx
		}).Assess("Uninstall", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		dyn := dynamic.NewForConfigOrDie(cfg.Client().RESTConfig())

		installer := &RBACInstaller{DynamicClient: dyn}

		clusterRole := &rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-clusterrole",
			},
		}

		// Apply the cluster role first
		_, err := installer.ApplyClusterRole(context.Background(), clusterRole)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}

		// Uninstall the cluster role
		err = installer.DeleteClusterRole(context.Background(), clusterRole.Name)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}

		// Verify the cluster role is deleted
		_, err = dyn.Resource(
			schema.GroupVersionResource{
				Group:    "rbac.authorization.k8s.io",
				Version:  "v1",
				Resource: "clusterroles",
			},
		).Namespace(clusterRole.Namespace).Get(ctx, clusterRole.Name, metav1.GetOptions{})
		if !errors.IsNotFound(err) {
			t.Fatalf("expected not found error, got none")
		}
		return ctx
	}).Feature()

	testenv.Test(t, f)
}

func TestUninstallClusterRoleBinding(t *testing.T) {
	f := features.New("Uninstall").
		Setup(e2e.Logger("test")).
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			return ctx
		}).Assess("Uninstall", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		dyn := dynamic.NewForConfigOrDie(cfg.Client().RESTConfig())

		installer := &RBACInstaller{DynamicClient: dyn}

		clusterRoleBinding := &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-clusterrolebinding",
			},
			RoleRef: rbacv1.RoleRef{
				APIGroup: "rbac.authorization.k8s.io",
				Kind:     "ClusterRole",
				Name:     "test-clusterrole",
			},
			Subjects: []rbacv1.Subject{
				{
					Kind:      "User",
					Name:      "test-user",
					Namespace: "default",
				},
			},
		}

		// Apply the cluster role binding first
		_, err := installer.ApplyClusterRoleBinding(context.Background(), clusterRoleBinding)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}

		// Uninstall the cluster role binding
		err = installer.DeleteClusterRoleBinding(context.Background(), clusterRoleBinding.Name)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}

		// Verify the cluster role binding is deleted
		_, err = dyn.Resource(
			schema.GroupVersionResource{
				Group:    "rbac.authorization.k8s.io",
				Version:  "v1",
				Resource: "clusterrolebindings",
			},
		).Namespace(clusterRoleBinding.Namespace).Get(ctx, clusterRoleBinding.Name, metav1.GetOptions{})
		if !errors.IsNotFound(err) {
			t.Fatalf("expected not found error, got none")
		}
		return ctx
	}).Feature()

	testenv.Test(t, f)
}

func TestUninstallRoleBinding(t *testing.T) {
	f := features.New("Uninstall").
		Setup(e2e.Logger("test")).
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			return ctx
		}).Assess("Uninstall", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		dyn := dynamic.NewForConfigOrDie(cfg.Client().RESTConfig())

		installer := &RBACInstaller{DynamicClient: dyn}

		roleBinding := &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-rolebinding",
				Namespace: "default",
			},
			RoleRef: rbacv1.RoleRef{
				APIGroup: "rbac.authorization.k8s.io",
				Kind:     "Role",
				Name:     "test-role",
			},
			Subjects: []rbacv1.Subject{
				{
					Kind:      "User",
					Name:      "test-user",
					Namespace: "default",
				},
			},
		}

		// Apply the role binding first
		_, err := installer.ApplyRoleBinding(context.Background(), roleBinding)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}

		// Uninstall the role binding
		err = installer.DeleteRoleBinding(context.Background(), roleBinding.Namespace, roleBinding.Name)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}

		// Verify the role binding is deleted
		_, err = dyn.Resource(
			schema.GroupVersionResource{
				Group:    "rbac.authorization.k8s.io",
				Version:  "v1",
				Resource: "rolebindings",
			},
		).Namespace(roleBinding.Namespace).Get(ctx, roleBinding.Name, metav1.GetOptions{})
		if !errors.IsNotFound(err) {
			t.Fatalf("expected not found error, got none")
		}
		return ctx
	}).Feature()

	testenv.Test(t, f)
}

func TestUninstallRBAC(t *testing.T) {
	f := features.New("Uninstall").
		Setup(e2e.Logger("test")).
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			return ctx
		}).Assess("Uninstall", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		dyn := dynamic.NewForConfigOrDie(cfg.Client().RESTConfig())

		installer := &RBACInstaller{DynamicClient: dyn}

		rbac := &RBAC{
			ClusterRole: &rbacv1.ClusterRole{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-clusterrole",
				},
			},
			ClusterRoleBinding: &rbacv1.ClusterRoleBinding{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-clusterrolebinding",
				},
				RoleRef: rbacv1.RoleRef{
					APIGroup: "rbac.authorization.k8s.io",
					Kind:     "ClusterRole",
					Name:     "test-clusterrole",
				},
				Subjects: []rbacv1.Subject{
					{
						Kind: "User",
						Name: "test-user",
					},
				},
			},
			Namespaced: map[string]Namespaced{
				"default": {
					Role: &rbacv1.Role{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "test-role",
							Namespace: "default",
						},
					},
					RoleBinding: &rbacv1.RoleBinding{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "test-rolebinding",
							Namespace: "default",
						},
						RoleRef: rbacv1.RoleRef{
							APIGroup: "rbac.authorization.k8s.io",
							Kind:     "Role",
							Name:     "test-role",
						},
						Subjects: []rbacv1.Subject{
							{
								Kind:      "User",
								Name:      "test-user",
								Namespace: "default",
							},
						},
					},
				},
			},
		}

		// Apply the RBAC first
		err := installer.ApplyRBAC(rbac)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}

		// Uninstall the RBAC
		err = installer.UninstallRBAC(rbac)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}

		return ctx
	}).Feature()

	testenv.Test(t, f)
}
func TestApplyNamespace(t *testing.T) {
	f := features.New("ApplyNamespace").
		Setup(e2e.Logger("test")).
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			return ctx
		}).Assess("Create new namespace", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		dyn := dynamic.NewForConfigOrDie(cfg.Client().RESTConfig())

		installer := &RBACInstaller{DynamicClient: dyn}

		namespace := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-namespace",
			},
		}

		result, err := installer.ApplyNamespace(context.Background(), namespace)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}

		if result.Name != namespace.Name {
			t.Errorf("expected namespace name %s, got %s", namespace.Name, result.Name)
		}

		return ctx
	}).Feature()

	testenv.Test(t, f)
}

func TestApplyNamespace_UpdateExisting(t *testing.T) {
	f := features.New("ApplyNamespace_UpdateExisting").
		Setup(e2e.Logger("test")).
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			return ctx
		}).Assess("Update existing namespace", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		dyn := dynamic.NewForConfigOrDie(cfg.Client().RESTConfig())

		installer := &RBACInstaller{DynamicClient: dyn}

		// Create initial namespace
		initialNamespace := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-namespace-update",
				Labels: map[string]string{
					"initial": "true",
				},
			},
		}

		_, err := installer.ApplyNamespace(context.Background(), initialNamespace)
		if err != nil {
			t.Fatalf("expected no error creating initial namespace, got %v", err)
		}

		// Update the namespace with new labels
		updatedNamespace := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-namespace-update",
				Labels: map[string]string{
					"updated": "true",
				},
			},
		}

		result, err := installer.ApplyNamespace(context.Background(), updatedNamespace)
		if err != nil {
			t.Fatalf("expected no error updating namespace, got %v", err)
		}

		if result.Name != updatedNamespace.Name {
			t.Errorf("expected namespace name %s, got %s", updatedNamespace.Name, result.Name)
		}

		// Verify the labels were updated
		if result.Labels["updated"] != "true" {
			t.Errorf("expected updated label to be 'true', got %s", result.Labels["updated"])
		}

		return ctx
	}).Feature()

	testenv.Test(t, f)
}

// TestApplyNamespace_PreservesForeignLabels is the regression guard for the
// demo-system label-strip churn: when the Namespace already exists and carries
// labels this installer does not manage (e.g. the postrenderer's
// krateo.io/composition-* set stamped by Helm), ApplyNamespace must merge its
// own labels in WITHOUT clobbering the foreign ones. A blind full-PUT of the
// bare CreateNamespace object used to strip them, and Helm re-added them every
// reconcile — one helm revision per cycle, forever.
func TestApplyNamespace_PreservesForeignLabels(t *testing.T) {
	f := features.New("ApplyNamespace_PreservesForeignLabels").
		Setup(e2e.Logger("test")).
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			return ctx
		}).Assess("Foreign composition labels survive a bare apply", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		dyn := dynamic.NewForConfigOrDie(cfg.Client().RESTConfig())

		installer := &RBACInstaller{DynamicClient: dyn}

		// Simulate the Helm-owned namespace: created by the chart + postrenderer
		// with a krateo.io/composition-* label the RBAC installer knows nothing about.
		helmOwned := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-namespace-foreign",
				Labels: map[string]string{
					"krateo.io/composition-id": "abc123",
				},
			},
		}
		if _, err := installer.ApplyNamespace(context.Background(), helmOwned); err != nil {
			t.Fatalf("expected no error seeding helm-owned namespace, got %v", err)
		}

		// The RBAC installer's own view: only the managed-by label (see CreateNamespace).
		bare := CreateNamespace("test-namespace-foreign", "some-release", "krateo-system")

		result, err := installer.ApplyNamespace(context.Background(), bare)
		if err != nil {
			t.Fatalf("expected no error applying bare namespace, got %v", err)
		}

		// Our label must be present...
		if result.Labels["app.kubernetes.io/managed-by"] != "Helm" {
			t.Errorf("expected managed-by=Helm to be merged in, got %q", result.Labels["app.kubernetes.io/managed-by"])
		}
		// ...AND the foreign composition label must NOT have been stripped.
		if result.Labels["krateo.io/composition-id"] != "abc123" {
			t.Errorf("foreign label krateo.io/composition-id was stripped: got %q, want abc123", result.Labels["krateo.io/composition-id"])
		}

		return ctx
	}).Feature()

	testenv.Test(t, f)
}

func TestApplyNamespace_InvalidName(t *testing.T) {
	f := features.New("ApplyNamespace_InvalidName").
		Setup(e2e.Logger("test")).
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			return ctx
		}).Assess("Handle invalid namespace name", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		dyn := dynamic.NewForConfigOrDie(cfg.Client().RESTConfig())

		installer := &RBACInstaller{DynamicClient: dyn}

		// Create namespace with invalid name (uppercase not allowed)
		namespace := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: "INVALID-NAMESPACE-NAME",
			},
		}

		_, err := installer.ApplyNamespace(context.Background(), namespace)
		if err == nil {
			t.Fatalf("expected error for invalid namespace name, got none")
		}

		return ctx
	}).Feature()

	testenv.Test(t, f)
}

package rbac

import (
	"context"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

// #357. The refusal is built EXACTLY as the apiserver's NamespaceLifecycle admission builds it
// (k8s.io/apiserver/pkg/admission/plugin/namespace/lifecycle/admission.go, v1.33.0, lines 174-180):
// a Forbidden StatusError with a NamespaceTerminating cause appended.
func terminatingRefusal(resource, name, ns string) error {
	err := apierrors.NewForbidden(schema.GroupResource{Group: "rbac.authorization.k8s.io", Resource: resource}, name,
		fmt.Errorf("unable to create new content in namespace %s because it is being terminated", ns))
	err.ErrStatus.Details.Causes = append(err.ErrStatus.Details.Causes, metav1.StatusCause{
		Type:    corev1.NamespaceTerminatingCause,
		Message: fmt.Sprintf("namespace %s is being terminated", ns),
		Field:   "metadata.namespace",
	})
	return err
}

var (
	rolesGVR        = schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}
	roleBindingsGVR = schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}
)

// refusingClient refuses every CREATE in namespace `ns` with refusal(resource, name, ns).
func refusingClient(ns string, refusal func(resource, name, ns string) error) *dynamicfake.FakeDynamicClient {
	cli := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		rolesGVR: "RoleList", roleBindingsGVR: "RoleBindingList",
	})
	cli.PrependReactor("create", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		ca, ok := a.(k8stesting.CreateAction)
		if !ok || ca.GetNamespace() != ns {
			return false, nil, nil
		}
		name := ""
		if o, ok := ca.GetObject().(metav1.Object); ok {
			name = o.GetName()
		}
		return true, nil, refusal(ca.GetResource().Resource, name, ns)
	})
	return cli
}

func pairIn(ns string) Namespaced {
	return Namespaced{
		Role: &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "portal-krateo-rbac", Namespace: ns},
			Rules: []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"get"}}}},
		RoleBinding: &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "portal-krateo-rbac", Namespace: ns},
			RoleRef:  rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: "portal-krateo-rbac"},
			Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: "sa", Namespace: "krateo-system"}}},
	}
}

// THE FIX: the 057 shape. portal's derived RBAC includes a Role in demo-system, which is
// terminating. The call succeeds, reports both refused objects, and still applies the rest.
//
// REPEATED, because rbac.Namespaced is a map and its iteration order is random. A skip that stopped
// the loop would leave the healthy namespace unapplied only when the dying one came first; twenty
// runs make that order all but certain to occur.
func TestApplyRBAC_TerminatingNamespaceIsSkippedAndReported(t *testing.T) {
	for run := 0; run < 20; run++ {
		applyAroundATerminatingNamespace(t)
	}
}

func applyAroundATerminatingNamespace(t *testing.T) {
	t.Helper()
	cli := refusingClient("demo-system", terminatingRefusal)
	report, err := NewRBACInstaller(cli).ApplyRBAC(&RBAC{Namespaced: map[string]Namespaced{
		"demo-system":   pairIn("demo-system"),
		"krateo-system": pairIn("krateo-system"),
	}})
	if err != nil {
		t.Fatalf("a terminating namespace must not fail the whole apply (#357), got: %v", err)
	}
	got := map[string]bool{}
	for _, s := range report.SkippedTerminating {
		got[s.Kind+" "+s.Namespace+"/"+s.Name] = true
	}
	for _, want := range []string{"Role demo-system/portal-krateo-rbac", "RoleBinding demo-system/portal-krateo-rbac"} {
		if !got[want] {
			t.Errorf("skipped object %q is not in the report, so nothing would say it was skipped: %v", want, report.SkippedTerminating)
		}
	}
	if len(report.SkippedTerminating) != 2 {
		t.Errorf("only the two objects in the terminating namespace may be reported, got %v", report.SkippedTerminating)
	}
	// The healthy namespace was still applied: skipping must not stop the loop.
	if _, err := cli.Resource(rolesGVR).Namespace("krateo-system").Get(context.Background(), "portal-krateo-rbac", metav1.GetOptions{}); err != nil {
		t.Errorf("the Role in the non-terminating namespace was not applied: %v", err)
	}
	if _, err := cli.Resource(roleBindingsGVR).Namespace("krateo-system").Get(context.Background(), "portal-krateo-rbac", metav1.GetOptions{}); err != nil {
		t.Errorf("the RoleBinding in the non-terminating namespace was not applied: %v", err)
	}
}

// THE CONTROLS. Only the NamespaceTerminating cause is skipped. Any other refusal still fails: a
// Role that cannot be created in a namespace that is NOT terminating is a real failure.
func TestApplyRBAC_OtherRefusalsStillFail(t *testing.T) {
	for _, tc := range []struct {
		name    string
		refusal func(resource, name, ns string) error
	}{
		{"a different Forbidden (quota)", func(resource, name, ns string) error {
			return apierrors.NewForbidden(schema.GroupResource{Group: "rbac.authorization.k8s.io", Resource: resource}, name,
				fmt.Errorf("exceeded quota: compute-resources"))
		}},
		// Keyed on the typed CAUSE, never on message text: the same words with no cause still fail.
		{"the same message text without the cause", func(resource, name, ns string) error {
			return apierrors.NewForbidden(schema.GroupResource{Group: "rbac.authorization.k8s.io", Resource: resource}, name,
				fmt.Errorf("unable to create new content in namespace %s because it is being terminated", ns))
		}},
		{"a server error", func(resource, name, ns string) error {
			return apierrors.NewInternalError(fmt.Errorf("etcdserver: request timed out"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report, err := NewRBACInstaller(refusingClient("team-a", tc.refusal)).ApplyRBAC(&RBAC{
				Namespaced: map[string]Namespaced{"team-a": pairIn("team-a")}})
			if err == nil {
				t.Fatalf("a refusal that is not NamespaceTerminating must still fail the apply")
			}
			if len(report.SkippedTerminating) != 0 {
				t.Errorf("a non-terminating refusal was reported as skipped: %v", report.SkippedTerminating)
			}
		})
	}
}

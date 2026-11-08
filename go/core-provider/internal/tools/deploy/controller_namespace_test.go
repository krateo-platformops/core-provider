package deploy

import (
	"context"
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	kscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// #313 step 1. Where the controller lives (ControllerNamespace) is a separate input from the
// namespace a namespaced grant covers (Namespace). These tests use two DIFFERENT values, because
// with equal values every placement bug is invisible.
const (
	testControllerNS = "mgmt"
	testGrantNS      = "defns"
)

var testGVR = schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1-0-0", Resource: "fireworksapps"}

// An empty ControllerNamespace must be refused, never defaulted to Namespace: a forgotten call
// site would otherwise keep the old placement silently.
func TestControllerNamespaceIsRequired(t *testing.T) {
	ctx := context.Background()
	kube := fake.NewClientBuilder().WithScheme(kscheme.Scheme).Build()
	opts := DeployOptions{GVR: testGVR, Namespace: testGrantNS, RBACFolderPath: "testdata"}

	if _, err := Deploy(ctx, kube, opts); !errors.Is(err, errNoControllerNamespace) {
		t.Errorf("Deploy: err = %v, want errNoControllerNamespace", err)
	}
	if _, err := Lookup(ctx, kube, opts); !errors.Is(err, errNoControllerNamespace) {
		t.Errorf("Lookup: err = %v, want errNoControllerNamespace", err)
	}
	uopts := UndeployOptions{GVR: testGVR, Namespace: testGrantNS, RBACFolderPath: "testdata", SkipCRD: true}
	if err := Undeploy(ctx, kube, uopts); !errors.Is(err, errNoControllerNamespace) {
		t.Errorf("Undeploy: err = %v, want errNoControllerNamespace", err)
	}
}

// The controller's own RBAC objects go to the controller namespace; the namespaced grant stays in
// the grant namespace; and every subject names the ServiceAccount where it actually lives.
func TestCreateRBACResourcesSplitsControllerAndGrant(t *testing.T) {
	name := resourceNamer(testGVR.Resource, testGVR.Version)
	sa, _, crb, role, rb, err := createRBACResources(testGVR,
		getCDCrbacNN(types.NamespacedName{Namespace: testControllerNS, Name: name}),
		getCDCrbacNN(types.NamespacedName{Namespace: testGrantNS, Name: name}),
		"testdata")
	if err != nil {
		t.Fatal(err)
	}
	if sa.Namespace != testControllerNS {
		t.Errorf("ServiceAccount namespace = %q, want the controller namespace %q", sa.Namespace, testControllerNS)
	}
	if role.Namespace != testGrantNS || rb.Namespace != testGrantNS {
		t.Errorf("Role/RoleBinding namespaces = %q/%q, want the grant namespace %q", role.Namespace, rb.Namespace, testGrantNS)
	}
	for kind, subjects := range map[string]int{"RoleBinding": len(rb.Subjects), "ClusterRoleBinding": len(crb.Subjects)} {
		if subjects == 0 {
			t.Fatalf("%s has no subjects: the subject assertions below would pass vacuously", kind)
		}
	}
	for _, s := range append(rb.Subjects, crb.Subjects...) {
		if s.Kind == "ServiceAccount" && s.Namespace != testControllerNS {
			t.Errorf("subject %s/%s: namespace %q, want %q (where the ServiceAccount lives)", s.Kind, s.Name, s.Namespace, testControllerNS)
		}
	}
}

// The authn mapping name is the identity's username (authn's login.go). It follows the
// ServiceAccount's namespace, so it can't drift from where the ServiceAccount actually lives.
func TestAuthnMappingFollowsTheControllerNamespace(t *testing.T) {
	opts := DeployOptions{
		GVR: testGVR, Namespace: testGrantNS, ControllerNamespace: testControllerNS,
		RBACFolderPath: "testdata", ApiRefName: "status-sources", AuthnNamespace: "krateo-system",
	}
	sa := resourceNamer(testGVR.Resource, testGVR.Version)
	obj, err := renderAuthnServiceAccountMapping(opts, sa, testControllerNS)
	if err != nil {
		t.Fatal(err)
	}
	if want := "cdc-" + testControllerNS + "-" + sa; obj.GetName() != want {
		t.Errorf("mapping name = %q, want %q", obj.GetName(), want)
	}
	if got := authnMappingName(opts.ControllerNamespace, sa); got != obj.GetName() {
		t.Errorf("the hashed name %q differs from the rendered name %q, so Deploy and Lookup would disagree", got, obj.GetName())
	}
}

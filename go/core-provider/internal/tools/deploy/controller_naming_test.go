package deploy

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// Every object of a Kind+version's composition-dynamic-controller is named from {resource} and
// {apiVersion} (and its namespace) ONLY, never from the CompositionDefinition that deployed it.
// core-provider's one-controller-per-version hand-over depends on that: two definitions in one
// namespace on one version share ONE ServiceAccount, Deployment and ConfigMap, so deleting either
// leaves the shared ClusterRoleBinding naming a live identity and needs no re-subjecting
// (compositiondefinitions.handedOver). Putting the definition's name into these names would look
// like a fix for a two-definition collision and would silently break that: the leaving definition
// would then own a ServiceAccount the survivor's ClusterRoleBinding points at. If this test fails,
// revisit handedOver's same-namespace case before changing the expectation.
func TestControllerObjectNamesCarryNoDefinitionIdentity(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1-0-0", Resource: "fireworksapps"}
	const ns = "krateo-system"

	if got := resourceNamer(gvr.Resource, gvr.Version); got != "fireworksapps-v1-0-0" {
		t.Fatalf("resourceNamer = %q, want fireworksapps-v1-0-0", got)
	}
	if got := ControllerDeploymentKey(ns, gvr); got != (types.NamespacedName{Namespace: ns, Name: "fireworksapps-v1-0-0-controller"}) {
		t.Fatalf("ControllerDeploymentKey = %v", got)
	}

	sa, clusterrole, clusterrolebinding, _, _, err := createRBACResources(gvr,
		getCDCrbacNN(types.NamespacedName{Namespace: ns, Name: resourceNamer(gvr.Resource, gvr.Version)}), "testdata")
	if err != nil {
		t.Fatalf("createRBACResources: %v", err)
	}
	if clusterrole.Name != "fireworksapps-v1-0-0" || clusterrolebinding.Name != "fireworksapps-v1-0-0" {
		t.Fatalf("shared cluster-scoped RBAC names = %q / %q, want fireworksapps-v1-0-0", clusterrole.Name, clusterrolebinding.Name)
	}
	if len(clusterrolebinding.Subjects) != 1 ||
		clusterrolebinding.Subjects[0].Name != sa.Name || clusterrolebinding.Subjects[0].Namespace != sa.Namespace {
		t.Fatalf("ClusterRoleBinding subject %v does not name the controller ServiceAccount %s/%s",
			clusterrolebinding.Subjects, sa.Namespace, sa.Name)
	}
	if sa.Namespace != ns {
		t.Fatalf("controller ServiceAccount namespace = %q, want %q", sa.Namespace, ns)
	}
}

// The same property for the templates core-provider actually SHIPS (helm/core-provider/assets/cdc,
// mounted at runtime), which the testdata copies above only mirror: each controller object's
// metadata.name may use .resource and .apiVersion and nothing else.
func TestShippedControllerTemplatesNameNoDefinition(t *testing.T) {
	const assets = "../../../../../helm/core-provider/assets/cdc"
	templateVar := regexp.MustCompile(`\.([A-Za-z_][A-Za-z0-9_]*)`)
	for _, f := range []string{
		"rbac/serviceaccount.yaml", "rbac/clusterrole.yaml", "rbac/clusterrolebinding.yaml",
		"deployment.yaml", "configmap.yaml", "service.yaml",
	} {
		b, err := os.ReadFile(filepath.Join(assets, f))
		if err != nil {
			t.Fatalf("reading shipped template %s: %v", f, err)
		}
		var nameLine string
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "  name:") {
				nameLine = line
				break
			}
		}
		if nameLine == "" {
			t.Fatalf("%s: no metadata.name line", f)
		}
		for _, m := range templateVar.FindAllStringSubmatch(nameLine, -1) {
			if m[1] != "resource" && m[1] != "apiVersion" {
				t.Errorf("%s: metadata.name uses .%s; controller objects must be named from .resource and .apiVersion only (%s)",
					f, m[1], strings.TrimSpace(nameLine))
			}
		}
	}
}

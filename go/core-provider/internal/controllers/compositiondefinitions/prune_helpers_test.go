//go:build e2e || envtest

package compositiondefinitions

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	crdclient "github.com/krateo-platformops/core-provider/internal/tools/crd"
	crdutils "github.com/krateo-platformops/core-provider/internal/tools/crd/generation"
	"github.com/krateo-platformops/core-provider/internal/tools/deploy"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Shared by the e2e prune suite (a real cluster from KUBECONFIG) and the envtest prune-grace
// test (#454): they need only a client and a dynamic client, so the real-apiserver helpers live
// here rather than in either suite.

const compositionGroup = "composition.krateo.io"

func pruneE2EClients(t *testing.T) (client.Client, dynamic.Interface) {
	t.Helper()
	if os.Getenv("KUBECONFIG") == "" {
		t.Skip("KUBECONFIG must be set")
	}
	rc, err := clientcmd.BuildConfigFromFlags("", os.Getenv("KUBECONFIG"))
	if err != nil {
		t.Fatalf("rest config: %v", err)
	}
	sc := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(sc)
	_ = apiextensionsv1.AddToScheme(sc)
	_ = compositiondefinitionsv1alpha1.SchemeBuilder.AddToScheme(sc)
	cl, err := client.New(rc, client.Options{Scheme: sc})
	if err != nil {
		t.Fatalf("kube client: %v", err)
	}
	dyn, err := dynamic.NewForConfig(rc)
	if err != nil {
		t.Fatalf("dynamic client: %v", err)
	}
	return cl, dyn
}

func specSchema(field string) []byte {
	return []byte(fmt.Sprintf(`{"type":"object","properties":{%q:{"type":"string"}}}`, field))
}

func waitEstablished(t *testing.T, cl client.Client, name string) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := cl.Get(context.Background(), types.NamespacedName{Name: name}, crd); err == nil {
			for _, c := range crd.Status.Conditions {
				if c.Type == apiextensionsv1.Established && c.Status == apiextensionsv1.ConditionTrue {
					return
				}
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("CRD %s not established", name)
}

// buildTwoVersionCRD applies v1-0-0 then v2-0-0 through the REAL ApplyOrUpdateCRD, yielding a CRD
// with versions [vacuum(storage), v1-0-0, v2-0-0]. Returns the current gvk (v2-0-0), the gvr for
// v1-0-0, the plural, and the CRD name.
func buildTwoVersionCRD(t *testing.T, ctx context.Context, cl client.Client, dyn dynamic.Interface) (curGVK schema.GroupVersionKind, gvrV1, gvrV2 schema.GroupVersionResource, crdName string) {
	t.Helper()
	kind := fmt.Sprintf("Prune%d", time.Now().UnixNano()%1000000)
	gvkV1 := schema.GroupVersionKind{Group: compositionGroup, Version: "v1-0-0", Kind: kind}
	gvkV2 := schema.GroupVersionKind{Group: compositionGroup, Version: "v2-0-0", Kind: kind}

	crd1, err := crdutils.GenerateCRD(specSchema("foo"), gvkV1)
	if err != nil {
		t.Fatalf("GenerateCRD v1: %v", err)
	}
	crd2, err := crdutils.GenerateCRD(specSchema("bar"), gvkV2)
	if err != nil {
		t.Fatalf("GenerateCRD v2: %v", err)
	}
	crdName = crd1.Name
	t.Cleanup(func() {
		c := &apiextensionsv1.CustomResourceDefinition{}
		c.Name = crdName
		_ = cl.Delete(context.Background(), c)
	})

	gvrV1, err = crdclient.ApplyOrUpdateCRD(ctx, cl, dyn, crd1)
	if err != nil {
		t.Fatalf("ApplyOrUpdateCRD v1: %v", err)
	}
	gvrV2, err = crdclient.ApplyOrUpdateCRD(ctx, cl, dyn, crd2)
	if err != nil {
		t.Fatalf("ApplyOrUpdateCRD v2: %v", err)
	}
	waitEstablished(t, cl, crdName)
	return gvkV2, gvrV1, gvrV2, crdName
}

func newExternal(cl client.Client, dyn dynamic.Interface) *external {
	return &external{
		mgmt:    cl,
		kube:    cl,
		dynamic: dyn,
		log:     logging.NewNopLogger(),
	}
}

// createInstance creates a composition instance through the given served endpoint, optionally
// stamping the composition-version label (the real MutatingAdmissionPolicy would do this on CREATE;
// here we set it explicitly so the label-present vs label-absent cases are deterministic).
func createInstance(t *testing.T, ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionKind, gvrRes schema.GroupVersionResource, name, versionLabel string) {
	t.Helper()
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvr)
	obj.SetName(name)
	obj.SetNamespace("default")
	if versionLabel != "" {
		obj.SetLabels(map[string]string{deploy.CompositionVersionLabel: versionLabel})
	}
	if _, err := dyn.Resource(gvrRes).Namespace("default").Create(ctx, obj, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create instance %s: %v", name, err)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

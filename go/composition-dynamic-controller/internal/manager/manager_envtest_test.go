//go:build envtest

// docs/design/cdc-pooling.md step 4, against a real apiserver: one process hosting two members (two
// Kinds) runs both, reports Ready only once both have listed, and stops cleanly.
//
//	Run: KUBEBUILDER_ASSETS=$(setup-envtest use -p path 1.36.0) \
//	go test -tags envtest ./internal/manager/ -run Envtest -v
package manager

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	apixv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func compositionCRD(gvr schema.GroupVersionResource, kind string) *apixv1.CustomResourceDefinition {
	preserve := true
	return &apixv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: gvr.Resource + "." + gvr.Group},
		Spec: apixv1.CustomResourceDefinitionSpec{
			Group: gvr.Group,
			Names: apixv1.CustomResourceDefinitionNames{Plural: gvr.Resource, Singular: gvr.Resource[:len(gvr.Resource)-1], Kind: kind, ListKind: kind + "List"},
			Scope: apixv1.NamespaceScoped,
			Versions: []apixv1.CustomResourceDefinitionVersion{{
				Name: gvr.Version, Served: true, Storage: true,
				Schema:       &apixv1.CustomResourceValidation{OpenAPIV3Schema: &apixv1.JSONSchemaProps{Type: "object", XPreserveUnknownFields: &preserve}},
				Subresources: &apixv1.CustomResourceSubresources{Status: &apixv1.CustomResourceSubresourceStatus{}},
			}},
		},
	}
}

func TestEnvtest_TwoMembersInOneProcess(t *testing.T) {
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
	ac := apiextclient.NewForConfigOrDie(cfg)
	for gvr, kind := range map[schema.GroupVersionResource]string{fireworks: "FireworksApp", portals: "Portal"} {
		if _, err := ac.ApiextensionsV1().CustomResourceDefinitions().Create(ctx, compositionCRD(gvr, kind), metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	dyn := dynamic.NewForConfigOrDie(cfg)
	deadline := time.Now().Add(30 * time.Second)
	for _, gvr := range []schema.GroupVersionResource{fireworks, portals} {
		for {
			if _, err := dyn.Resource(gvr).List(ctx, metav1.ListOptions{}); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("CRD %s never served", gvr)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	p := testProcess()
	p.Config = cfg
	built, err := Build(ctx, p, []Member{{GVR: fireworks}, {GVR: portals}})
	if err != nil {
		t.Fatal(err)
	}
	if err := Readiness(built).ReadyzCheck(nil); err == nil {
		t.Fatal("control: before Run nothing has listed, so the process must not be Ready")
	}

	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- Run(runCtx, p, built) }()
	ready := false
	for end := time.Now().Add(30 * time.Second); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		if Readiness(built).ReadyzCheck(nil) == nil {
			ready = true
			break
		}
	}
	if !ready {
		t.Fatalf("the process never became Ready: %v", Readiness(built).ReadyzCheck(nil))
	}
	for _, b := range built {
		if err := b.Controller.ReadyzCheck(nil); err != nil {
			t.Errorf("member %s not ready although the process is: %v", b.Member.GVR, err)
		}
	}
	// Readiness reads the informer's synced flag directly; the runtime's WaitForCacheSync polls the
	// same flag every 100ms before it starts workers. Cancelling inside that window makes Run report
	// "failed to wait for informers caches to sync", which is how a controller stopped during
	// startup has always ended (a single cdc too). This case is about a clean stop AFTER startup.
	time.Sleep(time.Second)
	stop()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v on a clean shutdown", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}

	// Readiness is EVERY member's, not the first's: a member whose CRD is never served keeps the
	// process not-ready, and is named, although the member beside it has listed.
	unserved := schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v9-9-9", Resource: "ghosts"}
	built2, err := Build(ctx, p, []Member{{GVR: fireworks}, {GVR: unserved}})
	if err != nil {
		t.Fatal(err)
	}
	runCtx2, stop2 := context.WithCancel(ctx)
	done2 := make(chan error, 1)
	go func() { done2 <- Run(runCtx2, p, built2) }()
	for end := time.Now().Add(30 * time.Second); time.Now().Before(end) && built2[0].Controller.ReadyzCheck(nil) != nil; time.Sleep(100 * time.Millisecond) {
	}
	if err := built2[0].Controller.ReadyzCheck(nil); err != nil {
		t.Fatalf("control: the served member must list: %v", err)
	}
	if err := Readiness(built2).ReadyzCheck(nil); err == nil || !strings.Contains(err.Error(), unserved.String()) {
		t.Fatalf("one member unlisted: the process must be not-ready naming %s, got %v", unserved, err)
	}
	stop2()
	<-done2
}

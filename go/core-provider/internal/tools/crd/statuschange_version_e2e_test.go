//go:build e2e

// Does ApplyOrUpdateCRD always create the version it reports creating?
//
// The CompositionDefinition reconcile trusts the GVR it returns for everything that follows: the
// CDC Deployment and ServiceAccount are built for that version, the instance migration lists and
// relabels THROUGH that version's endpoint, and the served-version prune evaluates against it.
//
// ApplyOrUpdateCRD has three paths for an existing CRD, tested in this order:
//
//  1. status schema differs  -> UpdateStatus on every version, apply, RETURN
//  2. the version exists     -> return
//  3. otherwise              -> AppendVersion, apply, return
//
// Path 1 returns before path 3 can run. So when a reconcile changes BOTH the chart version and the
// declared status fields — which happens on any `statusDataTemplate` edit landing in the same
// generation as a version bump, and on a core-provider upgrade that changes the embedded
// status.schema.json for every definition at once — the function reports the new GVR having never
// added that version to the CRD.
//
//	Run: KUBEBUILDER_ASSETS=$(setup-envtest use -p path 1.36.0) \
//	     go test -tags e2e ./internal/tools/crd/ -run StatusChange -v
package crd

import (
	"context"
	"os"
	"testing"

	"github.com/krateo-platformops/core-provider/internal/tools/crd/generation"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

const minimalValuesSchema = `{
  "$schema": "http://json-schema.org/draft-07/schema#",
  "type": "object",
  "properties": { "replicas": { "type": "integer" } }
}`

func TestE2E_StatusChangeSwallowsTheVersionBump(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via setup-envtest")
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest apiserver: %v", err)
	}
	defer func() { _ = env.Stop() }()

	ctx := context.Background()
	if err := registerEventually(); err != nil {
		t.Fatalf("register scheme: %v", err)
	}
	cli, err := client.New(cfg, client.Options{})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("dynamic client: %v", err)
	}

	gen := func(version string, status []generation.StatusField) *apiextensionsv1.CustomResourceDefinition {
		gvk := schema.GroupVersionKind{Group: "composition.krateo.io", Kind: "Doodad", Version: version}
		crd, err := generation.GenerateCRD([]byte(minimalValuesSchema), gvk)
		if err != nil {
			t.Fatalf("GenerateCRD %s: %v", version, err)
		}
		if err := generation.InjectStatusFields(crd, status); err != nil {
			t.Fatalf("InjectStatusFields %s: %v", version, err)
		}
		return crd
	}

	// --- Reconcile 1: initial version, no declared status fields.
	gvr1, err := ApplyOrUpdateCRD(ctx, cli, dyn, gen("v1-0-0", nil))
	if err != nil {
		t.Fatalf("first ApplyOrUpdateCRD: %v", err)
	}
	t.Logf("reconcile 1 -> gvr %s", gvr1.Version)
	if ok, _ := Lookup(ctx, cli, gvr1); !ok {
		t.Fatalf("precondition failed: v1-0-0 was not created, so this test proves nothing")
	}

	// --- Reconcile 2: the chart version moves to v2-0-0 AND a status field is declared in the
	// same generation (e.g. the user added a statusDataTemplate entry alongside the bump).
	withStatus := []generation.StatusField{{ForPath: "url", Expression: "${ .spec.replicas }", Type: "string"}}
	gvr2, err := ApplyOrUpdateCRD(ctx, cli, dyn, gen("v2-0-0", withStatus))
	if err != nil {
		t.Fatalf("second ApplyOrUpdateCRD: %v", err)
	}
	t.Logf("reconcile 2 -> gvr %s (reported as created, err=nil)", gvr2.Version)

	if gvr2.Version != "v2-0-0" {
		t.Fatalf("expected the returned GVR to name the new version, got %q", gvr2.Version)
	}

	exists, err := Lookup(ctx, cli, gvr2)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}

	live, err := Get(ctx, cli, gvr2.GroupResource())
	if err != nil {
		t.Fatalf("get live CRD: %v", err)
	}
	var names []string
	for _, v := range live.Spec.Versions {
		names = append(names, v.Name)
	}
	t.Logf("live CRD spec.versions = %v", names)

	if !exists {
		t.Errorf("ApplyOrUpdateCRD returned GVR %s with err=nil, but that version is NOT in the "+
			"CRD (live versions: %v).\n"+
			"The caller treats this as success and proceeds to: deploy a CDC Deployment+SA for a "+
			"version the apiserver does not serve, migrate composition instances THROUGH that "+
			"non-existent endpoint, and evaluate the served-version prune against it. The status-only "+
			"branch returns before AppendVersion is ever reached.", gvr2.Version, names)
	}

	// Whether or not the version landed, a second identical reconcile must converge — if it does,
	// the defect is a transient failed reconcile; if it does not, it is a permanent wedge. Say which.
	gvr3, err := ApplyOrUpdateCRD(ctx, cli, dyn, gen("v2-0-0", withStatus))
	if err != nil {
		t.Fatalf("third ApplyOrUpdateCRD: %v", err)
	}
	exists3, _ := Lookup(ctx, cli, gvr3)
	if exists3 {
		t.Logf("CONVERGES: a second reconcile with the same inputs does add %s, so the window is "+
			"one reconcile wide rather than permanent", gvr3.Version)
	} else {
		t.Errorf("DOES NOT CONVERGE: %s is still absent after a second identical reconcile", gvr3.Version)
	}

	// And confirm the status change itself did land, so this is specifically the version being
	// dropped rather than the whole apply failing.
	live2, _ := Get(ctx, cli, gvr2.GroupResource())
	for _, v := range live2.Spec.Versions {
		if v.Name == "v1-0-0" && v.Schema != nil && v.Schema.OpenAPIV3Schema != nil {
			st := v.Schema.OpenAPIV3Schema.Properties["status"]
			if _, ok := st.Properties["url"]; ok {
				t.Logf("the declared status field DID land on v1-0-0 — the status apply succeeded; " +
					"only the version append was skipped")
			}
		}
	}
	_ = metav1.ObjectMeta{}
}

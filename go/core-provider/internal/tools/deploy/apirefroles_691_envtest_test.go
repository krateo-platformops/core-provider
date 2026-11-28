//go:build envtest

// core-provider#691 against a real apiserver: the ClusterRole Deploy renders for a definition whose
// compositions' reads were resolved, before (one ClusterRole for every read) and after (cluster-scoped
// reads and the unresolvable residual only), and the Roles applied beside it.
//
//	Run: KUBEBUILDER_ASSETS=$(setup-envtest use 1.36.0 -p path) \
//	go test -tags envtest ./internal/tools/deploy/ -run ApiRefClusterRole691 -v
package deploy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"testing"

	kubecli "github.com/krateo-platformops/core-provider/internal/tools/kube"
	"github.com/krateo-platformops/core-provider/internal/tools/restactionrbac"
	hasher "github.com/krateo-platformops/plumbing/kubeutil/hasher"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func clusterRoleResources(cr *rbacv1.ClusterRole) []string {
	var out []string
	for _, r := range cr.Rules {
		out = append(out, r.Resources...)
	}
	sort.Strings(out)
	return out
}

func TestApiRefClusterRole691Envtest(t *testing.T) {
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
	for _, ns := range []string{"team-a", "team-b"} {
		if err := kube.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
			t.Fatal(err)
		}
	}

	// The definition's own resolution, with its static extras only: the cluster-scoped read, and the
	// namespaced ones whose namespace a composition decides.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"readSet": []restactionrbac.Resource{
			{Version: "v1", Resource: "nodes", Verb: "get"},
			{Version: "v1", Resource: "pods", Verb: "list", NamespaceUnknown: true},
		}})
	}))
	defer srv.Close()
	// What each composition's own resolution gave: pods in its own namespace, and, for one, a read
	// whose namespace comes from a prior stage.
	comps := [][]restactionrbac.Resource{
		{{Version: "v1", Resource: "nodes", Verb: "get"}, {Version: "v1", Resource: "pods", Namespace: "team-a", Verb: "list"}},
		{{Version: "v1", Resource: "nodes", Verb: "get"}, {Version: "v1", Resource: "pods", Namespace: "team-b", Verb: "list"},
			{Version: "v1", Resource: "secrets", Verb: "get", NamespaceUnknown: true}},
	}
	apply := func(gvr schema.GroupVersionResource, perNamespace bool) (*rbacv1.ClusterRole, string) {
		t.Helper()
		opts := DeployOptions{GVR: gvr, ApiRefName: "ra", ApiRefNamespace: "krateo-system", SnowplowURL: srv.URL,
			ApiRefPerNamespace: perNamespace, ApiRefCompositions: comps}
		hsh := hasher.NewFNVObjectHash()
		if err := applyRestActionRBAC(ctx, dyn, opts, "sa-"+gvr.Version, &hsh, kubecli.ApplyOptions{}); err != nil {
			t.Fatal(err)
		}
		cr := &rbacv1.ClusterRole{}
		if err := kube.Get(ctx, types.NamespacedName{Name: ApiRefRBACName(gvr)}, cr); err != nil {
			t.Fatal(err)
		}
		return cr, hsh.GetHash()
	}

	// BEFORE (a remote definition still takes this path): every read in the one ClusterRole. Its rows
	// are the definition's resolution, so the namespaced read is cluster-wide.
	legacyGVR := schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1-0-1", Resource: "fireworksapps"}
	before, _ := apply(legacyGVR, false)
	t.Logf("BEFORE ClusterRole %s resources: %v", before.Name, clusterRoleResources(before))
	if got := clusterRoleResources(before); !contains(got, "pods") {
		t.Fatalf("control: the single-ClusterRole path must carry pods (the cluster-wide grant #691 removes), got %v", got)
	}

	// AFTER: no pods rule at all; the cluster-scoped read, the restactions get, and the named residual.
	gvr := schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1-0-2", Resource: "fireworksapps"}
	after, digest := apply(gvr, true)
	t.Logf("AFTER  ClusterRole %s resources: %v", after.Name, clusterRoleResources(after))
	got := clusterRoleResources(after)
	if contains(got, "pods") {
		t.Fatalf("ClusterRole carries pods: a namespaced read a composition places is a Role, got %v", got)
	}
	for _, want := range []string{"nodes", "restactions", "secrets"} {
		if !contains(got, want) {
			t.Errorf("ClusterRole lacks %s: %v", want, got)
		}
	}
	// Applied again, the same plan is the same digest and the same object.
	again, digest2 := apply(gvr, true)
	if digest != digest2 || again.ResourceVersion != after.ResourceVersion {
		t.Errorf("same plan, different result: digest %s vs %s, resourceVersion %s vs %s", digest, digest2, after.ResourceVersion, again.ResourceVersion)
	}

	// The Roles, beside it: one per namespace, then pruned when a namespace leaves, then all removed.
	composed := restactionrbac.Compose(nil, comps, "krateo:cdc:sa-v1-0-2", ApiRefRBACName(gvr))
	def := types.NamespacedName{Namespace: "default", Name: "d"}
	if err := ApplyApiRefRoles(ctx, dyn, gvr, def, composed); err != nil {
		t.Fatal(err)
	}
	roles := func() []string {
		l := &rbacv1.RoleList{}
		if err := kube.List(ctx, l, client.MatchingLabels{ApiRefRBACLabel: ApiRefRBACName(gvr)}); err != nil {
			t.Fatal(err)
		}
		var ns []string
		for _, r := range l.Items {
			ns = append(ns, r.Namespace)
		}
		sort.Strings(ns)
		return ns
	}
	if got := roles(); len(got) != 2 || got[0] != "team-a" || got[1] != "team-b" {
		t.Fatalf("Roles in %v, want [team-a team-b]", got)
	}
	if err := ApplyApiRefRoles(ctx, dyn, gvr, def, restactionrbac.Compose(nil, comps[:1], "krateo:cdc:sa-v1-0-2", ApiRefRBACName(gvr))); err != nil {
		t.Fatal(err)
	}
	if got := roles(); len(got) != 1 || got[0] != "team-a" {
		t.Fatalf("after team-b left, Roles in %v, want [team-a]", got)
	}
	if err := DeleteApiRefRoles(ctx, dyn, ApiRefRBACName(gvr)); err != nil {
		t.Fatal(err)
	}
	if got := roles(); len(got) != 0 {
		t.Fatalf("after teardown, Roles in %v, want none", got)
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

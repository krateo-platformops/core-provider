//go:build envtest

// core-provider#691 against a real apiserver: a definition's apiRef reads are Roles in the namespaces
// its compositions' reads land in, driven by the composition watch of a running manager, with an
// httptest snowplow whose /rbac answers per the extras it is sent (so the inputs really do differ by
// compositionNamespace, which is what makes the namespaces differ).
//
//	Run: KUBEBUILDER_ASSETS=$(setup-envtest use 1.36.0 -p path) \
//	go test -tags envtest ./internal/controllers/compositiondefinitions/ -run ApiRefRoles691 -v
package compositiondefinitions

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/tools/deploy"
	"github.com/krateo-platformops/core-provider/internal/tools/restactionrbac"
	"github.com/krateo-platformops/plumbing/labels"
	rtv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/event"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// rbac691Snowplow answers GET /rbac per extras: a pods read in the composition's own namespace, a
// configmaps read of the composition by name there, a cluster-scoped nodes read, and (when prior is
// set) a read whose namespace a prior stage decides.
type rbac691Snowplow struct {
	srv    *httptest.Server
	calls  atomic.Int64
	status atomic.Int32 // 0: answer; else that status
	prior  atomic.Bool
	last   atomic.Value // map[string]any: the extras of the last call
}

func newRBAC691Snowplow(t *testing.T) *rbac691Snowplow {
	s := &rbac691Snowplow{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.calls.Add(1)
		if st := s.status.Load(); st != 0 {
			w.WriteHeader(int(st))
			return
		}
		extras := map[string]any{}
		_ = json.Unmarshal([]byte(r.URL.Query().Get("extras")), &extras)
		s.last.Store(extras)
		ns, _ := extras["compositionNamespace"].(string)
		name, _ := extras["compositionName"].(string)
		rows := []restactionrbac.Resource{
			{Version: "v1", Resource: "pods", Namespace: ns, Verb: "list"},
			{Version: "v1", Resource: "configmaps", Namespace: ns, Name: name, Verb: "get"},
			{Version: "v1", Resource: "nodes", Verb: "get"},
		}
		if s.prior.Load() {
			rows = append(rows, restactionrbac.Resource{Version: "v1", Resource: "secrets", Verb: "get", NamespaceUnknown: true})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"restaction": map[string]string{"name": "ra", "namespace": "krateo-system"}, "readSet": rows})
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func eventually691(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestApiRefRoles691Envtest(t *testing.T) {
	// startStzEnv sets the package's ManagementNamespace and leaves it set; this file sorts before
	// controllerowner_test.go, whose TestHandedOver reads it, so it is put back (CI runs the package whole).
	oldNS := ManagementNamespace
	t.Cleanup(func() { ManagementNamespace = oldNS })
	e := startStzEnv(t)
	ctx := context.Background()
	sp := newRBAC691Snowplow(t)
	old := SnowplowURL
	SnowplowURL = sp.srv.URL
	t.Cleanup(func() { SnowplowURL = old })

	for _, ns := range []string{"team-a", "team-b", "team-c"} {
		if err := e.kube.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
			t.Fatal(err)
		}
	}
	// The RESTAction, so its generation keys the cache.
	raCRD := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "restactions.templates.krateo.io"},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "templates.krateo.io", Scope: apiextensionsv1.NamespaceScoped,
			Names: apiextensionsv1.CustomResourceDefinitionNames{Plural: "restactions", Singular: "restaction", Kind: "RESTAction", ListKind: "RESTActionList"},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{Name: "v1", Served: true, Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{Type: "object", XPreserveUnknownFields: ptr.To(true)}}}},
		},
	}
	if _, err := envtest.InstallCRDs(e.cfg.Config, envtest.CRDInstallOptions{CRDs: []*apiextensionsv1.CustomResourceDefinition{raCRD}}); err != nil {
		t.Fatal(err)
	}
	dyn := dynamic.NewForConfigOrDie(e.cfg.Config)
	eventually691(t, "the RESTAction CRD is served", func() bool {
		ra := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "templates.krateo.io/v1", "kind": "RESTAction",
			"metadata": map[string]any{"name": "ra", "namespace": "krateo-system"}, "spec": map[string]any{}}}
		err := e.kube.Create(ctx, ra)
		return err == nil || apierrors.IsAlreadyExists(err)
	})

	// The definition, deployed: its GVR recorded, its apiRef declared with static extras. The
	// ClusterRoleBinding is what Deploy writes and the Roles bind the same group as.
	cd := &compositiondefinitionsv1alpha1.CompositionDefinition{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "d"}}
	cd.Spec.Chart = &compositiondefinitionsv1alpha1.ChartInfo{Url: "oci://example/charts/fireworks", Version: "1.0.0"}
	cd.Spec.ApiRef = &compositiondefinitionsv1alpha1.ApiReference{Name: "ra", Namespace: "krateo-system"}
	cd.Spec.ApiRef.Extras = &apiextensionsv1.JSON{Raw: []byte(`{"tier":"gold"}`)}
	if err := e.kube.Create(ctx, cd); err != nil {
		t.Fatal(err)
	}
	cd.Status.ApiVersion, cd.Status.Kind, cd.Status.Resource = stzGVK.GroupVersion().String(), stzGVK.Kind, stzGVR.Resource
	cd.SetConditions(rtv1.Available())
	if err := e.kube.Status().Update(ctx, cd); err != nil {
		t.Fatal(err)
	}
	name := deploy.ApiRefRBACName(stzGVR)
	crb := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name},
		Subjects: []rbacv1.Subject{{Kind: rbacv1.GroupKind, Name: "krateo:cdc:fireworksapps-v1-0-0", APIGroup: rbacv1.GroupName}},
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: name}}
	if err := e.kube.Create(ctx, crb); err != nil {
		t.Fatal(err)
	}

	mgr, err := ctrl.NewManager(e.cfg.Config, ctrl.Options{Scheme: e.kube.Scheme(), Metrics: metricsserver.Options{BindAddress: "0"}})
	if err != nil {
		t.Fatal(err)
	}
	planner := newAPIRefPlanner(dyn, func() *restactionrbac.Client { return restactionrbac.New(sp.srv.URL) })
	kicks := make(chan event.GenericEvent, 8)
	planner.kick = func(k types.NamespacedName) {
		kicks <- event.GenericEvent{Object: &compositiondefinitionsv1alpha1.CompositionDefinition{ObjectMeta: metav1.ObjectMeta{Namespace: k.Namespace, Name: k.Name}}}
	}
	if err := setupApiRefRoles(mgr, dyn, planner, kicks, logging.NewNopLogger()); err != nil {
		t.Fatal(err)
	}
	mctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = mgr.Start(mctx) }()

	roleIn := func(ns string) *rbacv1.Role {
		r := &rbacv1.Role{}
		if err := e.kube.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, r); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			t.Fatal(err)
		}
		return r
	}
	// namespacesWithRoles lists every namespace holding the definition's Role, by label.
	namespacesWithRoles := func() []string {
		l := &rbacv1.RoleList{}
		if err := e.kube.List(ctx, l, client.MatchingLabels{deploy.ApiRefRBACLabel: name}); err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range l.Items {
			out = append(out, r.Namespace)
		}
		sort.Strings(out)
		return out
	}
	same := func(a, b []string) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}
	composition := func(ns, n string) {
		t.Helper()
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(stzGVK)
		u.SetNamespace(ns)
		u.SetName(n)
		u.SetLabels(map[string]string{labels.CompositionVersionLabel: stzVersion})
		if err := e.kube.Create(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	deleteComposition := func(ns, n string) {
		t.Helper()
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(stzGVK)
		u.SetNamespace(ns)
		u.SetName(n)
		if err := e.kube.Delete(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	configmapNames := func(r *rbacv1.Role) []string {
		for _, rule := range r.Rules {
			if len(rule.Resources) == 1 && rule.Resources[0] == "configmaps" {
				return rule.ResourceNames
			}
		}
		return nil
	}

	// CONTROL: with no composition there is no Role anywhere (and the definition's reconciler did run).
	time.Sleep(2 * time.Second)
	if got := namespacesWithRoles(); len(got) != 0 {
		t.Fatalf("control: Roles with no composition: %v", got)
	}

	// A composition appears: a Role in ITS namespace, with the pods read, bound to the CDC group.
	composition("team-a", "c1")
	eventually691(t, "a Role in team-a", func() bool { return roleIn("team-a") != nil })
	r := roleIn("team-a")
	if len(r.Rules) == 0 || r.Labels[deploy.ApiRefRBACLabel] != name || r.Labels[deploy.ApiRefRBACDefinitionLabel] != "d" {
		t.Fatalf("Role = %+v, want its rules and its labels", r)
	}
	rb := &rbacv1.RoleBinding{}
	if err := e.kube.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: name}, rb); err != nil {
		t.Fatal(err)
	}
	if rb.Subjects[0].Name != "krateo:cdc:fireworksapps-v1-0-0" || rb.RoleRef.Kind != "Role" || rb.RoleRef.Name != name {
		t.Errorf("RoleBinding = %+v", rb)
	}
	// The extras snowplow was sent are the shared builder's: the static ones, and the composition's three.
	ex, _ := sp.last.Load().(map[string]any)
	if ex["tier"] != "gold" || ex["compositionName"] != "c1" || ex["compositionNamespace"] != "team-a" || ex["compositionId"] == "" || ex["compositionId"] == nil {
		t.Errorf("extras sent to /rbac = %v", ex)
	}
	if got := namespacesWithRoles(); !same(got, []string{"team-a"}) {
		t.Fatalf("Roles in %v, want exactly [team-a]", got)
	}

	// A second namespace, and a second composition in the first: the Roles are the union.
	composition("team-b", "c2")
	composition("team-a", "c3")
	eventually691(t, "Roles in team-a and team-b, with team-a's the union of c1 and c3", func() bool {
		a, b := roleIn("team-a"), roleIn("team-b")
		return a != nil && b != nil && same(configmapNames(a), []string{"c1", "c3"})
	})
	if got := configmapNames(roleIn("team-b")); !same(got, []string{"c2"}) {
		t.Errorf("team-b's configmaps = %v, want only its own composition's", got)
	}

	// The last composition in a namespace leaves: its Role goes; the one with another composition stays.
	deleteComposition("team-a", "c1")
	eventually691(t, "team-a's Role shrinks to c3", func() bool { return same(configmapNames(roleIn("team-a")), []string{"c3"}) })
	deleteComposition("team-a", "c3")
	eventually691(t, "team-a's Role is removed with its last composition", func() bool { return roleIn("team-a") == nil })
	if roleIn("team-b") == nil {
		t.Fatal("team-b's Role must stay: c2 is still there")
	}

	// SNOWPLOW DOWN: cannot determine is not empty. A composition leaves and another arrives while it
	// is down: no Role is deleted and none is written from half an answer.
	sp.status.Store(http.StatusServiceUnavailable)
	before := sp.calls.Load()
	deleteComposition("team-b", "c2")
	composition("team-c", "c4")
	eventually691(t, "the reconciler tried and was refused", func() bool { return sp.calls.Load() >= before+2 })
	time.Sleep(3 * time.Second)
	if got := namespacesWithRoles(); !same(got, []string{"team-b"}) {
		t.Fatalf("with snowplow down the Roles are %v, want [team-b] exactly as before (team-b's composition is gone, but that is not known; team-c's is not granted yet)", got)
	}

	// It comes back: the retry converges on the truth.
	sp.status.Store(0)
	eventually691(t, "team-b's Role is removed and team-c's written once snowplow answers", func() bool {
		return same(namespacesWithRoles(), []string{"team-c"})
	})
}

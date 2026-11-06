//go:build integration
// +build integration

package compositiondefinitions

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/e2e-framework/klient/k8s"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
)

// fakeSnowplowRBAC answers GET /rbac. It starts as a cluster where the RESTAction does not exist.
type fakeSnowplowRBAC struct {
	server *httptest.Server
	exists atomic.Bool
	calls  atomic.Int64
}

func newFakeSnowplowRBAC(t *testing.T) *fakeSnowplowRBAC {
	t.Helper()
	f := &fakeSnowplowRBAC{}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f.calls.Add(1)
		if f.exists.Load() {
			_, _ = w.Write([]byte(`{"readSet":[]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404,"message":"restactions.templates.krateo.io \"ra\" not found"}`))
	}))
	t.Cleanup(f.server.Close)
	return f
}

// installAuthnMapping provides what Deploy applies before it reaches /rbac: authn owns this CRD and
// the namespace the mapping lands in, and neither exists on the test cluster.
func installAuthnMapping(ctx context.Context, t *testing.T, r *resources.Resources) {
	t.Helper()
	open := true
	crd := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "serviceaccounts.serviceaccount.authn.krateo.io"},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "serviceaccount.authn.krateo.io",
			Scope: apiextensionsv1.NamespaceScoped,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural: "serviceaccounts", Singular: "serviceaccount", Kind: "ServiceAccount", ListKind: "ServiceAccountList",
			},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name: "v1alpha1", Served: true, Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
					Type: "object", XPreserveUnknownFields: &open,
				}},
			}},
		},
	}
	if err := r.Create(ctx, crd); err != nil {
		t.Fatalf("creating the authn ServiceAccount CRD: %v", err)
	}
	t.Cleanup(func() { _ = r.Delete(context.Background(), crd) })

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "krateo-system"}}
	if err := r.Create(ctx, ns); err != nil {
		t.Fatalf("creating the authn namespace: %v", err)
	}
	t.Cleanup(func() { _ = r.Delete(context.Background(), ns) })
}

// assessApiRefRBACCondition drives the real controller, because the failure this exists to prevent is
// a condition documented everywhere and set by no code (#372). A unit test of the helper stays green
// with every call to it deleted.
func assessApiRefRBACCondition(ctx context.Context, t *testing.T, r *resources.Resources, f *fakeSnowplowRBAC) {
	t.Helper()
	installAuthnMapping(ctx, t, r)

	cd := &v1alpha1.CompositionDefinition{ObjectMeta: metav1.ObjectMeta{Name: "apiref-rbac", Namespace: namespace}}
	cd.Spec.Chart = &v1alpha1.ChartInfo{Url: "oci://ghcr.io/krateo-platformops/charts/core-provider", Version: "2.12.4"}
	cd.Spec.ApiRef = &v1alpha1.ApiReference{Name: "ra", Namespace: "ns"}
	if err := r.Create(ctx, cd); err != nil {
		t.Fatalf("creating the definition: %v", err)
	}
	t.Cleanup(func() { _ = r.Delete(context.Background(), cd) })

	await := func(what string, ok func(*v1alpha1.CompositionDefinition) bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Minute)
		got := &v1alpha1.CompositionDefinition{}
		for time.Now().Before(deadline) {
			if err := r.Get(ctx, cd.Name, cd.Namespace, got); err == nil && ok(got) {
				return
			}
			time.Sleep(2 * time.Second)
		}
		t.Fatalf("%s: never happened; /rbac called %d times; conditions: %+v", what, f.calls.Load(), got.Status.Conditions)
	}

	// The new-definition path, which Observe never reaches.
	await("False/RESTActionNotFound", func(c *v1alpha1.CompositionDefinition) bool {
		rc := c.GetCondition(conditionTypeApiRefRBACComplete)
		return rc.Status == metav1.ConditionFalse && rc.Reason == "RESTActionNotFound"
	})

	// The RESTAction appears. Nobody edits the definition.
	f.exists.Store(true)
	await("True/ReadSetResolved with no edit", func(c *v1alpha1.CompositionDefinition) bool {
		rc := c.GetCondition(conditionTypeApiRefRBACComplete)
		return rc.Status == metav1.ConditionTrue && rc.Reason == reasonReadSetResolved
	})

	// It disappears from a definition already deployed, which takes Observe's path.
	f.exists.Store(false)
	await("False again through Observe", func(c *v1alpha1.CompositionDefinition) bool {
		return c.GetCondition(conditionTypeApiRefRBACComplete).Status == metav1.ConditionFalse
	})

	// The apiRef goes, so a condition about it must go too.
	got := &v1alpha1.CompositionDefinition{}
	if err := r.Get(ctx, cd.Name, cd.Namespace, got); err != nil {
		t.Fatalf("reading the definition: %v", err)
	}
	patch := k8s.Patch{PatchType: "application/merge-patch+json", Data: []byte(`{"spec":{"apiRef":null}}`)}
	if err := r.Patch(ctx, got, patch); err != nil {
		t.Fatalf("removing the apiRef: %v", err)
	}
	await("the condition removed with the apiRef", func(c *v1alpha1.CompositionDefinition) bool {
		for _, cond := range c.Status.Conditions {
			if cond.Type == conditionTypeApiRefRBACComplete {
				return false
			}
		}
		return true
	})
}

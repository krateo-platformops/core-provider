// The local branch of ensureCompositionVersionPolicy, which has two properties that pull in
// opposite directions and are each dangerous to lose.
//
// It MUST migrate. The policy on the management cluster came from the chart, and before #173 both
// charts shipped the pre-#66 ApplyConfiguration mutation — which denies a composition write
// whenever an unrelated field holds a Quantity string under a numeric schema, with
// failurePolicy: Fail. The Go-side migration written to repair that was reachable only on remote
// targets, so the cluster it was written for was the one case it skipped.
//
// It MUST NOT create. The chart owns this object (on krateo-057: helm release `installer` in
// `krateo-system`, via the core-provider subchart). An object minted here carries none of helm's
// ownership metadata, and the next `helm upgrade` then fails with "invalid ownership metadata" —
// wedging the whole umbrella over an object the chart installs anyway. core-provider's ClusterRole
// grants get+update only, so a regression here fails as Forbidden in a real cluster rather than
// silently creating; this test is what catches it before that.
//
// And it must not be fatal. A best-effort migration that fails must not fail the reconcile, or a
// latent admission bug is traded for a certain wedge — the same call made for Delete's RBAC
// refresh in #167.
package compositiondefinitions

import (
	"context"
	"errors"
	"testing"

	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const (
	policyGroupVersion = "admissionregistration.k8s.io/v1"
	versionPolicyName  = "krateo-composition-version"
)

// policyScheme registers the policy GVKs as unstructured so the fake client can track them.
func policyScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	for _, kind := range []string{"MutatingAdmissionPolicy", "MutatingAdmissionPolicyBinding"} {
		gvk := schema.GroupVersionKind{Group: "admissionregistration.k8s.io", Version: "v1", Kind: kind}
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(gvk)
		s.AddKnownTypeWithName(gvk, u)
		s.AddKnownTypeWithName(gvk.GroupVersion().WithKind(kind+"List"), &unstructured.UnstructuredList{})
	}
	return s
}

// legacyChartPolicy is the policy as both charts shipped it before #173, complete with the helm
// ownership metadata that makes it the chart's object rather than ours.
func legacyChartPolicy() *unstructured.Unstructured {
	p := &unstructured.Unstructured{}
	p.SetAPIVersion(policyGroupVersion)
	p.SetKind("MutatingAdmissionPolicy")
	p.SetName(versionPolicyName)
	p.SetLabels(map[string]string{"app.kubernetes.io/managed-by": "Helm"})
	p.SetAnnotations(map[string]string{
		"meta.helm.sh/release-name":      "installer",
		"meta.helm.sh/release-namespace": "krateo-system",
	})
	p.Object["spec"] = map[string]any{
		"mutations": []any{map[string]any{
			"patchType": "ApplyConfiguration",
			"applyConfiguration": map[string]any{
				"expression": `Object{ metadata: Object.metadata{ labels: {"krateo.io/composition-version": request.requestKind.version} } }`,
			},
		}},
	}
	return p
}

func getPolicy(t *testing.T, c client.Client, kind string) (*unstructured.Unstructured, error) {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(policyGroupVersion)
	u.SetKind(kind)
	err := c.Get(context.Background(), client.ObjectKey{Name: versionPolicyName}, u)
	return u, err
}

func TestLocalTargetMigratesTheChartsLegacyPolicy(t *testing.T) {
	c := fakeclient.NewClientBuilder().WithScheme(policyScheme()).WithObjects(legacyChartPolicy()).Build()
	e := &external{remote: false, kube: c, log: logging.NewNopLogger()}

	if err := e.ensureCompositionVersionPolicy(context.Background()); err != nil {
		t.Fatalf("local ensure returned an error: %v", err)
	}

	got, err := getPolicy(t, c, "MutatingAdmissionPolicy")
	if err != nil {
		t.Fatalf("reading the policy back: %v", err)
	}
	muts, _, _ := unstructured.NestedSlice(got.Object, "spec", "mutations")
	if len(muts) != 1 {
		t.Fatalf("expected 1 mutation, got %d", len(muts))
	}
	m := muts[0].(map[string]any)
	if pt, _ := m["patchType"].(string); pt != "JSONPatch" {
		t.Errorf("patchType is %q, want JSONPatch — the local target did not migrate, which is #173: "+
			"the fix lands in the Go module and never reaches the cluster it was written for", pt)
	}

	// The migration edits in place, so helm's ownership must survive it. Losing these annotations
	// is the same wedge as creating the object ourselves, just arrived at differently.
	ann := got.GetAnnotations()
	if ann["meta.helm.sh/release-name"] != "installer" || ann["meta.helm.sh/release-namespace"] != "krateo-system" {
		t.Errorf("the migration dropped helm's ownership annotations (%v); the next helm upgrade "+
			"would fail with \"invalid ownership metadata\"", ann)
	}
}

// The assertion that matters most: an absent policy on a local target stays absent.
func TestLocalTargetNeverCreatesThePolicy(t *testing.T) {
	c := fakeclient.NewClientBuilder().WithScheme(policyScheme()).Build()
	e := &external{remote: false, kube: c, log: logging.NewNopLogger()}

	if err := e.ensureCompositionVersionPolicy(context.Background()); err != nil {
		t.Fatalf("local ensure returned an error for an absent policy: %v", err)
	}

	for _, kind := range []string{"MutatingAdmissionPolicy", "MutatingAdmissionPolicyBinding"} {
		if _, err := getPolicy(t, c, kind); !apierrors.IsNotFound(err) {
			t.Errorf("a %s now exists on the local target (err=%v). The chart owns this object; one "+
				"created here has no helm ownership metadata and the next `helm upgrade` fails with "+
				"\"invalid ownership metadata\", wedging the umbrella. A missing policy locally is an "+
				"incomplete chart install, to be fixed by helm.", kind, err)
		}
	}
}

// A failing migration must not fail the reconcile. The realistic failure is Forbidden, since the
// grant for this is new in #173 and a cluster running an older chart's ClusterRole will not have it.
func TestLocalTargetMigrationFailureIsNotFatal(t *testing.T) {
	forbidden := apierrors.NewForbidden(
		schema.GroupResource{Group: "admissionregistration.k8s.io", Resource: "mutatingadmissionpolicies"},
		versionPolicyName, errors.New("no RBAC grant"))

	c := fakeclient.NewClientBuilder().
		WithScheme(policyScheme()).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
				return forbidden
			},
		}).
		Build()
	e := &external{remote: false, kube: c, log: logging.NewNopLogger()}

	if err := e.ensureCompositionVersionPolicy(context.Background()); err != nil {
		t.Errorf("a failed policy migration failed the reconcile: %v\n"+
			"The migration is a stopgap for #173 and the chart is the real repair. Failing here "+
			"trades a latent admission bug for a certain reconcile wedge, which is the wrong "+
			"direction — see Delete's non-fatal RBAC refresh in #167.", err)
	}
}

// Remote targets must keep projecting the policy, create included: nothing else installs it there.
func TestRemoteTargetStillCreatesThePolicy(t *testing.T) {
	c := fakeclient.NewClientBuilder().WithScheme(policyScheme()).Build()
	e := &external{remote: true, kube: c, log: logging.NewNopLogger()}

	if err := e.ensureCompositionVersionPolicy(context.Background()); err != nil {
		t.Fatalf("remote ensure: %v", err)
	}

	for _, kind := range []string{"MutatingAdmissionPolicy", "MutatingAdmissionPolicyBinding"} {
		if _, err := getPolicy(t, c, kind); err != nil {
			t.Errorf("remote target has no %s after projection: %v — compositions created there "+
				"would never be stamped, and are then invisible to their cdc", kind, err)
		}
	}
}

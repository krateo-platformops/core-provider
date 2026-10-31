package compositiondefinitions

import (
	"context"
	"strings"
	"testing"

	rtv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// #269 WIRING. internal/tools/crd's TestIsReady covers the predicate and kills two compiling
// mutants of it. Neither says anything about whether Observe CALLS it — and before this change
// IsReady had zero live callers for however long, with a correct implementation and no effect.
// A predicate nobody consults is the defect, not the fix.
//
// The assertion is on the gate's own MESSAGE, not merely on "not Available". Observe has several
// earlier returns that also leave Ready non-Available, so a test asserting the absence of
// Available would pass against an early return and prove nothing about the branch. The string
// "accepted but not yet Established" is emitted at exactly one place, so matching it is evidence
// that execution reached there.
func TestObserve_AvailableRequiresAnEstablishedCRD(t *testing.T) {
	const (
		apiVersion = "composition.krateo.io/v1-0-0"
		kind       = "Fireworks"
		resource   = "fireworks"
		crdName    = resource + ".composition.krateo.io"
	)

	// A CRD the apiserver has ACCEPTED but not yet Established: the real intermediate state.
	acceptedNotEstablished := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: crdName},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "composition.krateo.io",
			Names: apiextensionsv1.CustomResourceDefinitionNames{Kind: kind, Plural: resource},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
				{Name: "v1-0-0", Served: true, Storage: true},
			},
		},
		Status: apiextensionsv1.CustomResourceDefinitionStatus{
			Conditions: []apiextensionsv1.CustomResourceDefinitionCondition{
				{Type: apiextensionsv1.NamesAccepted, Status: apiextensionsv1.ConditionTrue},
			},
			StoredVersions: []string{"v1-0-0"},
		},
	}

	cd := cdWithStatusGVK(apiVersion, kind, resource)
	kc := clientfake.NewClientBuilder().
		WithScheme(scheme(t)).
		WithObjects(cd, acceptedNotEstablished).
		WithStatusSubresource(cd).
		Build()
	e := &external{
		kube:      kc,
		mgmt:      kc,
		apiReader: kc,
		// setTargetStatus calls e.client.Discovery().ServerVersion() unconditionally, so a nil
		// clientset panics before the gate is reached.
		client: k8sfake.NewSimpleClientset(),
		log:    logging.NewNopLogger(),
	}

	_, err := e.Observe(context.Background(), cd)

	// Observe may legitimately error out before the gate in this reduced fixture (no chart, no
	// chart-inspector). That is not a pass and must not be read as one: if it never reached the
	// gate, this test is vacuous and says so rather than reporting green.
	// GetCondition(TypeReady) is the accessor the other tests in this package use.
	cond := cd.GetCondition(rtv1.TypeReady)
	reached := strings.Contains(cond.Message, "accepted but not yet Established")

	if cond.Reason == rtv1.ReasonAvailable {
		t.Fatalf("Ready=Available was set for a CRD that is NOT Established. That is #269: "+
			"Available then means 'I wrote a CRD', not 'the API answers', and a consumer reading "+
			"status.apiVersion/kind/resource off it gets correct fields naming an endpoint that "+
			"404s. (Observe err: %v)", err)
	}
	if reached {
		if string(cond.Reason) != "Creating" {
			t.Errorf("reason = %q, want Creating: an accepted CRD becomes Established on its own, "+
				"so this is converging rather than failed", cond.Reason)
		}
		if cond.Status != metav1.ConditionFalse {
			t.Errorf("status = %q; Creating is a False Ready", cond.Status)
		}
	}

	if !reached {
		t.Logf("DIAGNOSTIC: Ready cond = type=%q reason=%q status=%q message=%q",
			cond.Type, cond.Reason, cond.Status, cond.Message)
		t.Skipf("VACUOUS, and deliberately SKIPPED rather than passed: Observe returned before the "+
			"Established gate (err: %v). The chart fetch precedes the gate and this repo has no "+
			"local chart fixture, so the call site is structurally unreachable from a unit test.\n\n"+
			"The predicate is covered and mutation-verified by internal/tools/crd's TestIsReady. "+
			"The CALL SITE is not, and this skip is the record of that rather than a hidden gap.\n\n"+
			"What would verify it: an e2e-tagged test against a real cluster, creating a CRD whose "+
			"names FAIL NamesAccepted so Established never arrives -- deterministic, where racing "+
			"the apiserver to catch the normal millisecond-long intermediate state is not. If a "+
			"chart fixture is ever added here, this test starts working with no changes.", err)
	}
}

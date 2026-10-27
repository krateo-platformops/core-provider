package compositiondefinitions

import (
	"context"
	"errors"
	"testing"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// The Kind every definition in this file generates, and a second one to stand for "somebody
// else's chart".
var (
	refGK   = schema.GroupKind{Group: "composition.krateo.io", Kind: "SelfserviceKrateo"}
	otherGK = schema.GroupKind{Group: "composition.krateo.io", Kind: "FireworksApp"}
)

// resolverTo answers every chart with gk and records that it was asked.
func resolverTo(gk schema.GroupKind, calls *int) kindResolver {
	return func(context.Context, *compositiondefinitionsv1alpha1.ChartInfo) (schema.GroupKind, error) {
		*calls++
		return gk, nil
	}
}

// resolverFailing stands for the definition whose chart cannot be fetched at all — an unreachable
// registry, a chart pulled from under it, credentials that stopped working.
func resolverFailing(calls *int) kindResolver {
	return func(context.Context, *compositiondefinitionsv1alpha1.ChartInfo) (schema.GroupKind, error) {
		*calls++
		return schema.GroupKind{}, errors.New("pull oci://registry.invalid/chart: no such host")
	}
}

// resolverForbidden fails the test if it is called at all. It is how the cost guard is asserted:
// the question is whether a fetch HAPPENED, which is behaviour, not a mock's call count.
func resolverForbidden(t *testing.T) kindResolver {
	return func(context.Context, *compositiondefinitionsv1alpha1.ChartInfo) (schema.GroupKind, error) {
		t.Helper()
		t.Fatal("resolved a chart for a definition that already reported its Kind in status; " +
			"on a cluster with 83 definitions this is 82 chart fetches per delete")
		return schema.GroupKind{}, nil
	}
}

// onVersion gives cd a reconciled status on one version of refGK.
func onVersion(cd compositiondefinitionsv1alpha1.CompositionDefinition, version string) compositiondefinitionsv1alpha1.CompositionDefinition {
	cd.Status.ApiVersion = refGK.Group + "/" + version
	cd.Status.Kind = refGK.Kind
	return cd
}

// onKind gives cd a reconciled status on some other Kind entirely.
func onKind(cd compositiondefinitionsv1alpha1.CompositionDefinition, gk schema.GroupKind, version string) compositiondefinitionsv1alpha1.CompositionDefinition {
	cd.Status.ApiVersion = gk.Group + "/" + version
	cd.Status.Kind = gk.Kind
	return cd
}

// statusOnlyKeepsCRD is the gate this replaced: count the definitions whose STATUS GVK names this
// Group+Kind, and keep the CRD only if there is more than one. It appears here solely as the
// control arm — the point of the primary test is that it and referencesKind disagree, and that the
// old answer is the destructive one.
func statusOnlyKeepsCRD(cds []compositiondefinitionsv1alpha1.CompositionDefinition, gk schema.GroupKind) bool {
	n := 0
	for i := range cds {
		if cds[i].Status.CurrentGVK().GroupKind() == gk {
			n++
		}
	}
	return n > 1
}

// #214's destructive case, and the reason this gate was rewritten.
//
// A helm upgrade moves a Kind from one definition to another AND bumps it: create CD-new targeting
// chart 2.0.0 (which generates v2-0-0), delete CD-old targeting 1.0.0 (v1-0-0). The two happen
// within a second, so CD-new has no status yet.
//
// Both old gates missed it. controllerSuccessor asks about v1-0-0 specifically and CD-new declares
// v2-0-0, so there is no successor. The CRD gate counted status GVKs and CD-new has none, so it
// saw one definition and deleted the shared CRD — taking v1-0-0, v2-0-0, and every instance of
// EITHER version with it, including the ones CD-new was created to serve.
func TestReferencesKind_UnreconciledSiblingOnAnotherVersionKeepsTheCRD(t *testing.T) {
	oldCD := onVersion(ownerCD("krateo-system", "selfservice-krateo", 0), "v1-0-0")
	newCD := ownerCD("selfservice", "selfservice-krateo", 1) // created seconds ago, no status
	cds := []compositiondefinitionsv1alpha1.CompositionDefinition{oldCD, newCD}

	// Control arm: the gate as it stood. It must get this wrong, or the fix is not a fix.
	if statusOnlyKeepsCRD(cds, refGK) {
		t.Fatal("the status-only count kept the CRD here; then #214 was not the defect it was filed as " +
			"and this test is no longer measuring anything")
	}

	calls := 0
	keep, by, err := referencesKind(context.Background(), &oldCD, cds, refGK, resolverTo(refGK, &calls))
	if err != nil {
		t.Fatalf("referencesKind: %v", err)
	}
	if !keep {
		t.Fatalf("the CRD was deleted while %s/%s still needed the Kind: every instance of every "+
			"version of %s is destroyed, including the incoming definition's",
			newCD.Namespace, newCD.Name, refGK.String())
	}
	if calls != 1 {
		t.Errorf("resolved %d charts, want exactly 1 (only the sibling without a status)", calls)
	}
	t.Logf("CRD kept for %s", by)
}

// A definition whose first reconcile is FAILING is the one with no status and no readable chart.
// That failure must never become the reason a Kind's CRD and every instance of it are deleted, so
// an unresolvable chart keeps the CRD. Getting this wrong costs a CRD nobody serves — a leak,
// deletable by hand. Getting it the other way costs the instances.
func TestReferencesKind_UnresolvableChartKeepsTheCRD(t *testing.T) {
	self := onVersion(ownerCD("krateo-system", "selfservice-krateo", 0), "v1-0-0")
	broken := ownerCD("selfservice", "incoming", 1)
	cds := []compositiondefinitionsv1alpha1.CompositionDefinition{self, broken}

	calls := 0
	keep, by, err := referencesKind(context.Background(), &self, cds, refGK, resolverFailing(&calls))
	if err != nil {
		t.Fatalf("an unreadable chart must not fail the delete, it must hold the CRD: %v", err)
	}
	if !keep {
		t.Fatal("a definition whose chart could not be read was treated as absent, and the CRD went with it")
	}
	if calls != 1 {
		t.Errorf("resolved %d charts, want 1", calls)
	}
	t.Logf("CRD kept for %s", by)
}

// The control arm for the whole gate: with nothing else on the Kind, the CRD must still be
// deletable. A gate that answered "keep" unconditionally would pass every test above and leak a
// CRD and a cdc on every teardown.
func TestReferencesKind_LastDefinitionStillDeletesTheCRD(t *testing.T) {
	self := onVersion(ownerCD("krateo-system", "selfservice-krateo", 0), "v1-0-0")
	elsewhere := onKind(ownerCD("other", "fireworks", 1), otherGK, "v1-0-0")
	cds := []compositiondefinitionsv1alpha1.CompositionDefinition{self, elsewhere}

	keep, by, err := referencesKind(context.Background(), &self, cds, refGK, resolverForbidden(t))
	if err != nil {
		t.Fatalf("referencesKind: %v", err)
	}
	if keep {
		t.Fatalf("the last definition on %s could not delete its CRD (kept for %q): every teardown "+
			"would leave the Kind behind", refGK.String(), by)
	}
}

// A sibling that has already reported its Kind in status is trusted on it. Reconciled definitions
// on other Kinds are the overwhelming majority — 83 definitions on krateo-057, almost all on
// distinct Kinds — so consulting their charts would turn one delete into 82 registry pulls.
// resolverForbidden makes that a failure rather than a slow test.
func TestReferencesKind_ReconciledSiblingOnAnotherKindIsNotFetched(t *testing.T) {
	self := onVersion(ownerCD("krateo-system", "selfservice-krateo", 0), "v1-0-0")
	cds := []compositiondefinitionsv1alpha1.CompositionDefinition{self}
	for i := 0; i < 20; i++ {
		cds = append(cds, onKind(ownerCD("ns", "other-"+string(rune('a'+i)), i+1), otherGK, "v1-0-0"))
	}

	keep, _, err := referencesKind(context.Background(), &self, cds, refGK, resolverForbidden(t))
	if err != nil {
		t.Fatalf("referencesKind: %v", err)
	}
	if keep {
		t.Fatal("definitions on an unrelated Kind kept this Kind's CRD alive")
	}
}

// Definitions that are themselves going away, and remote ones that never shared this CRD, are not
// reasons to keep it. Two same-chart definitions deleted together must not each cite the other:
// both would decline to clean up and the CRD, the cdc and the instances would leak with nothing
// reconciling them. Same exclusion oldestLiveLocalSibling makes, for the same reason.
func TestReferencesKind_TerminatingAndRemoteSiblingsDoNotKeepIt(t *testing.T) {
	self := onVersion(ownerCD("krateo-system", "selfservice-krateo", 0), "v1-0-0")

	for _, tc := range []struct {
		name    string
		sibling compositiondefinitionsv1alpha1.CompositionDefinition
	}{
		{"terminating, reconciled on the same Kind", deleting(onVersion(ownerCD("ns", "going", 1), "v1-0-0"))},
		{"terminating, never reconciled", deleting(ownerCD("ns", "going-unreconciled", 1))},
		{"remote, reconciled on the same Kind", remoteCD(onVersion(ownerCD("ns", "spoke", 1), "v1-0-0"))},
		{"remote, never reconciled", remoteCD(ownerCD("ns", "spoke-unreconciled", 1))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cds := []compositiondefinitionsv1alpha1.CompositionDefinition{self, tc.sibling}
			// Forbidden for the excluded sibling too: an excluded definition should be skipped
			// before its chart is ever considered.
			keep, by, err := referencesKind(context.Background(), &self, cds, refGK, resolverForbidden(t))
			if err != nil {
				t.Fatalf("referencesKind: %v", err)
			}
			if keep {
				t.Fatalf("CRD kept for %q, which is not a live local definition on this Kind", by)
			}
		})
	}
}

// A sibling reconciled onto a DIFFERENT version of the same Kind was the one case the old count
// did get right, and it has to keep working: that is the ordinary shape of two definitions sharing
// a Kind across a bump.
func TestReferencesKind_ReconciledSiblingOnAnotherVersionKeepsTheCRD(t *testing.T) {
	self := onVersion(ownerCD("krateo-system", "selfservice-krateo", 0), "v1-0-0")
	sibling := onVersion(ownerCD("selfservice", "selfservice-krateo", 1), "v2-0-0")
	cds := []compositiondefinitionsv1alpha1.CompositionDefinition{self, sibling}

	if !statusOnlyKeepsCRD(cds, refGK) {
		t.Fatal("control arm: the old count should already have kept the CRD in this case")
	}

	keep, by, err := referencesKind(context.Background(), &self, cds, refGK, resolverForbidden(t))
	if err != nil {
		t.Fatalf("referencesKind: %v", err)
	}
	if !keep {
		t.Fatalf("a reconciled sibling on %s lost its CRD", sibling.Status.ApiVersion)
	}
	t.Logf("CRD kept for %s", by)
}

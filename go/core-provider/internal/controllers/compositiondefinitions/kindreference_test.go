package compositiondefinitions

import (
	"context"
	"errors"
	"fmt"
	"testing"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// The Kind every definition in this file generates, a second one to stand for "somebody else's
// chart", and the chart URLs that generate each.
var (
	refGK   = schema.GroupKind{Group: "composition.krateo.io", Kind: "SelfserviceKrateo"}
	otherGK = schema.GroupKind{Group: "composition.krateo.io", Kind: "FireworksApp"}
)

const (
	refURL     = "oci://ghcr.io/krateo-blueprints/charts/selfservice-krateo"
	otherURL   = "oci://ghcr.io/krateo-blueprints/charts/fireworks-app"
	privateURL = "oci://ghcr.io/acme-private/charts/github-provider-kog"
)

// resolverTo answers every chart with gk and records that it was asked.
func resolverTo(gk schema.GroupKind, calls *int) kindResolver {
	return func(context.Context, *compositiondefinitionsv1alpha1.ChartInfo) (schema.GroupKind, error) {
		*calls++
		return gk, nil
	}
}

// resolverFailing stands for a chart that cannot be fetched at all — a private registry, an
// unreachable host, credentials that stopped working.
func resolverFailing(calls *int) kindResolver {
	return func(context.Context, *compositiondefinitionsv1alpha1.ChartInfo) (schema.GroupKind, error) {
		*calls++
		return schema.GroupKind{}, errors.New("pull " + privateURL + ": denied")
	}
}

// resolverForbidden fails the test if it is called at all. It is how the no-fetch guarantees are
// asserted: the question is whether a fetch HAPPENED, which is behaviour, not a mock's call count.
func resolverForbidden(t *testing.T) kindResolver {
	return func(_ context.Context, c *compositiondefinitionsv1alpha1.ChartInfo) (schema.GroupKind, error) {
		t.Helper()
		t.Fatalf("resolved a chart (%v) that should have been decided without a fetch", c)
		return schema.GroupKind{}, nil
	}
}

// chartedCD is a definition with a spec chart and no status: created, never reconciled (or paused,
// or erroring).
func chartedCD(namespace, name string, ageRank int, url, version string) compositiondefinitionsv1alpha1.CompositionDefinition {
	cd := ownerCD(namespace, name, ageRank)
	cd.Spec.Chart = &compositiondefinitionsv1alpha1.ChartInfo{Url: url, Version: version}
	return cd
}

// reconciledOn is a definition reconciled onto gk at version, serving chart url. Its
// status.managed is what chartsOfKind reads to learn which charts generate gk.
func reconciledOn(namespace, name string, ageRank int, gk schema.GroupKind, url, version string) compositiondefinitionsv1alpha1.CompositionDefinition {
	cd := chartedCD(namespace, name, ageRank, url, version)
	cd.Status.ApiVersion = gk.Group + "/" + crdVersion(version)
	cd.Status.Kind = gk.Kind
	cd.Status.Managed.Group = gk.Group
	cd.Status.Managed.Kind = gk.Kind
	cd.Status.Managed.VersionInfo = []compositiondefinitionsv1alpha1.VersionDetail{{
		Version: crdVersion(version), Served: true,
		Chart: &compositiondefinitionsv1alpha1.ChartInfoProps{Url: url, Version: version},
	}}
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
	oldCD := reconciledOn("krateo-system", "selfservice-krateo", 0, refGK, refURL, "1.0.0")
	newCD := chartedCD("selfservice", "selfservice-krateo", 1, refURL, "2.0.0") // seconds old, no status
	cds := []compositiondefinitionsv1alpha1.CompositionDefinition{oldCD, newCD}

	// Control arm: the gate as it stood. It must get this wrong, or the fix is not a fix.
	if statusOnlyKeepsCRD(cds, refGK) {
		t.Fatal("the status-only count kept the CRD here; then #214 was not the defect it was filed as " +
			"and this test is no longer measuring anything")
	}

	// No fetch: oldCD's chart is already known to generate refGK and newCD names the same chart,
	// so specOfKind settles it. This is also why the fix survives newCD's chart being unreadable.
	keep, err := referencesKind(context.Background(), &oldCD, cds, refGK, resolverForbidden(t))
	if err != nil {
		t.Fatalf("referencesKind: %v", err)
	}
	if !keep.keep {
		t.Fatalf("the CRD was deleted while %s/%s still needed the Kind: every instance of every "+
			"version of %s is destroyed, including the incoming definition's",
			newCD.Namespace, newCD.Name, refGK.String())
	}
	t.Logf("CRD kept for %s", keep)
}

// The same move-and-bump, but the incoming definition's chart is served from a DIFFERENT url (a
// mirror, a fork, a registry migration), so specOfKind cannot see it. The resolver must then be
// consulted, and it answers exactly.
func TestReferencesKind_UnreconciledSiblingOnAMirrorIsResolved(t *testing.T) {
	oldCD := reconciledOn("krateo-system", "selfservice-krateo", 0, refGK, refURL, "1.0.0")
	mirrored := chartedCD("selfservice", "selfservice-krateo", 1, "oci://mirror.internal/selfservice-krateo", "2.0.0")
	cds := []compositiondefinitionsv1alpha1.CompositionDefinition{oldCD, mirrored}

	calls := 0
	keep, err := referencesKind(context.Background(), &oldCD, cds, refGK, resolverTo(refGK, &calls))
	if err != nil {
		t.Fatalf("referencesKind: %v", err)
	}
	if !keep.keep {
		t.Fatal("a mirrored chart of this Kind lost its CRD")
	}
	if keep.why != "chart" {
		t.Errorf("held for %q, want the resolver to be what decided it", keep.why)
	}
	if calls != 1 {
		t.Errorf("resolved %d charts, want exactly 1", calls)
	}
}

// acmp's finding on #217, and the reason the unreadable-chart rule is bounded.
//
// The first version of this gate held the CRD whenever ANY unreconciled sibling's chart could not
// be read. krateo-system/github-provider-kog on the demo has no status GVK and a private chart, so
// that rule would have kept EVERY Kind's CRD on the cluster, on every last-definition delete,
// logged only at Debug. One broken definition would have disabled CRD teardown platform-wide.
//
// Fail-safe has to mean "do not destroy on ignorance", not "never say no".
func TestReferencesKind_UnreadableChartDoesNotHoldAnUnrelatedKind(t *testing.T) {
	self := reconciledOn("krateo-system", "selfservice-krateo", 0, refGK, refURL, "1.0.0")
	broken := chartedCD("krateo-system", "github-provider-kog", 1, privateURL, "0.1.0")
	cds := []compositiondefinitionsv1alpha1.CompositionDefinition{self, broken}

	calls := 0
	keep, err := referencesKind(context.Background(), &self, cds, refGK, resolverFailing(&calls))
	if err != nil {
		t.Fatalf("an unreadable chart must not fail the delete: %v", err)
	}
	if keep.keep {
		t.Fatalf("one definition with an unreadable chart held %s's CRD (%s). Every Kind on the "+
			"cluster would be held the same way, on every last-definition delete: that is not a "+
			"fail-safe, it is a platform-wide teardown off switch", refGK.String(), keep)
	}
	if calls != 1 {
		t.Errorf("resolved %d charts, want 1 (the unreconciled sibling, once)", calls)
	}
}

// The other half of the bound: when the unreadable chart IS a known chart of this Kind, it still
// holds — and without a fetch, because specOfKind answers from status already in hand. This is
// what keeps the #214 fix working for a definition whose chart happens to be unreachable.
func TestReferencesKind_UnreadableChartOfThisKindStillHolds(t *testing.T) {
	self := reconciledOn("krateo-system", "selfservice-krateo", 0, refGK, refURL, "1.0.0")
	// Same chart, next version, private registry: unreadable, but demonstrably of this Kind.
	incoming := chartedCD("selfservice", "selfservice-krateo", 1, refURL, "2.0.0")
	cds := []compositiondefinitionsv1alpha1.CompositionDefinition{self, incoming}

	keep, err := referencesKind(context.Background(), &self, cds, refGK, resolverForbidden(t))
	if err != nil {
		t.Fatalf("referencesKind: %v", err)
	}
	if !keep.keep || keep.why != "spec" {
		t.Fatalf("keep=%v why=%q, want a hold decided from the known charts of the Kind with no fetch",
			keep.keep, keep.why)
	}
}

// A definition being deleted still generates its own Kind, so its chart belongs in that Kind's
// identity. Without it, an unreconciled sibling carrying the same chart at another version could
// not be recognised when NO other definition of the Kind has reconciled — the single-definition
// hand-over, which is the common one.
func TestReferencesKind_SelfsChartCountsTowardTheKindsIdentity(t *testing.T) {
	// self has never reconciled onto managed status either: only its spec chart identifies the Kind.
	self := chartedCD("krateo-system", "selfservice-krateo", 0, refURL, "1.0.0")
	self.Status.ApiVersion = refGK.Group + "/v1-0-0"
	self.Status.Kind = refGK.Kind
	incoming := chartedCD("selfservice", "selfservice-krateo", 1, refURL, "2.0.0")
	cds := []compositiondefinitionsv1alpha1.CompositionDefinition{self, incoming}

	keep, err := referencesKind(context.Background(), &self, cds, refGK, resolverForbidden(t))
	if err != nil {
		t.Fatalf("referencesKind: %v", err)
	}
	if !keep.keep {
		t.Fatal("the only other definition on the Kind was not recognised, so the CRD went with the delete")
	}
}

// The control arm for the whole gate: with nothing else on the Kind, the CRD must still be
// deletable. A gate that answered "keep" unconditionally would pass every positive test above and
// leak a CRD and a cdc on every teardown.
func TestReferencesKind_LastDefinitionStillDeletesTheCRD(t *testing.T) {
	self := reconciledOn("krateo-system", "selfservice-krateo", 0, refGK, refURL, "1.0.0")
	elsewhere := reconciledOn("other", "fireworks", 1, otherGK, otherURL, "1.0.0")
	cds := []compositiondefinitionsv1alpha1.CompositionDefinition{self, elsewhere}

	keep, err := referencesKind(context.Background(), &self, cds, refGK, resolverForbidden(t))
	if err != nil {
		t.Fatalf("referencesKind: %v", err)
	}
	if keep.keep {
		t.Fatalf("the last definition on %s could not delete its CRD (%s): every teardown would "+
			"leave the Kind behind", refGK.String(), keep)
	}
}

// A sibling that has already reported its Kind in status is trusted on it. Reconciled definitions
// on other Kinds are the overwhelming majority — 68 definitions on krateo-057, all with a status
// GVK, almost all on distinct Kinds — so consulting their charts would turn one delete into 67
// registry pulls. resolverForbidden makes that a failure rather than a slow test.
func TestReferencesKind_ReconciledSiblingOnAnotherKindIsNotFetched(t *testing.T) {
	self := reconciledOn("krateo-system", "selfservice-krateo", 0, refGK, refURL, "1.0.0")
	cds := []compositiondefinitionsv1alpha1.CompositionDefinition{self}
	for i := 0; i < 20; i++ {
		cds = append(cds, reconciledOn("ns", fmt.Sprintf("other-%d", i), i+1, otherGK, otherURL, "1.0.0"))
	}

	keep, err := referencesKind(context.Background(), &self, cds, refGK, resolverForbidden(t))
	if err != nil {
		t.Fatalf("referencesKind: %v", err)
	}
	if keep.keep {
		t.Fatal("definitions on an unrelated Kind kept this Kind's CRD alive")
	}
}

// Definitions that are themselves going away, and remote ones that never shared this CRD, are not
// reasons to keep it. Two same-chart definitions deleted together must not each cite the other:
// both would decline to clean up and the CRD, the cdc and the instances would leak with nothing
// reconciling them. Same exclusion oldestLiveLocalSibling makes, for the same reason.
func TestReferencesKind_TerminatingAndRemoteSiblingsDoNotKeepIt(t *testing.T) {
	self := reconciledOn("krateo-system", "selfservice-krateo", 0, refGK, refURL, "1.0.0")

	for _, tc := range []struct {
		name    string
		sibling compositiondefinitionsv1alpha1.CompositionDefinition
	}{
		{"terminating, reconciled on the same Kind", deleting(reconciledOn("ns", "going", 1, refGK, refURL, "2.0.0"))},
		{"terminating, never reconciled, same chart", deleting(chartedCD("ns", "going-unreconciled", 1, refURL, "2.0.0"))},
		{"remote, reconciled on the same Kind", remoteCD(reconciledOn("ns", "spoke", 1, refGK, refURL, "2.0.0"))},
		{"remote, never reconciled, same chart", remoteCD(chartedCD("ns", "spoke-unreconciled", 1, refURL, "2.0.0"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cds := []compositiondefinitionsv1alpha1.CompositionDefinition{self, tc.sibling}
			// Forbidden for the excluded sibling too: an excluded definition should be skipped
			// before its chart is ever considered.
			keep, err := referencesKind(context.Background(), &self, cds, refGK, resolverForbidden(t))
			if err != nil {
				t.Fatalf("referencesKind: %v", err)
			}
			if keep.keep {
				t.Fatalf("CRD kept for %s, which is not a live local definition on this Kind", keep)
			}
		})
	}
}

// A sibling reconciled onto a DIFFERENT version of the same Kind was the one case the old count
// did get right, and it has to keep working: that is the ordinary shape of two definitions sharing
// a Kind across a bump.
func TestReferencesKind_ReconciledSiblingOnAnotherVersionKeepsTheCRD(t *testing.T) {
	self := reconciledOn("krateo-system", "selfservice-krateo", 0, refGK, refURL, "1.0.0")
	sibling := reconciledOn("selfservice", "selfservice-krateo", 1, refGK, refURL, "2.0.0")
	cds := []compositiondefinitionsv1alpha1.CompositionDefinition{self, sibling}

	if !statusOnlyKeepsCRD(cds, refGK) {
		t.Fatal("control arm: the old count should already have kept the CRD in this case")
	}

	keep, err := referencesKind(context.Background(), &self, cds, refGK, resolverForbidden(t))
	if err != nil {
		t.Fatalf("referencesKind: %v", err)
	}
	if !keep.keep || keep.why != "status" {
		t.Fatalf("keep=%v why=%q, want a hold decided from the sibling's status", keep.keep, keep.why)
	}
}

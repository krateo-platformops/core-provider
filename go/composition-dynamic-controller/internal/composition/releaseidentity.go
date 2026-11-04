package composition

import (
	"fmt"

	"k8s.io/apimachinery/pkg/runtime/schema"

	helmconfig "github.com/krateo-platformops/plumbing/helm"

	"github.com/krateo-platformops/composition-dynamic-controller/internal/tools/processor"
)

// Label keys stamped on EVERY rendered object by plumbing's post-renderer
// (plumbing/helm/utils/postrenderer.go). They are copied here as literals rather than imported
// because plumbing declares them inline in Run() with no exported constants; if that changes,
// import them and delete these.
const (
	labelCompositionGroup     = "krateo.io/composition-group"
	labelCompositionResource  = "krateo.io/composition-resource"
	labelCompositionName      = "krateo.io/composition-name"
	labelCompositionNamespace = "krateo.io/composition-namespace"
)

// compositionIdentity is the part of a composition's identity that SURVIVES RECREATION, which is
// the only part a recreated CR can be matched on.
//
// WHY NOT THE RELEASE NAME. The name is <CR name>-<hash(UID[:8])>, and #322's first design matched
// it by shape: prefix plus a suffix of the right length. Diego pointed out what that cannot do.
// Two compositions may share a name in one namespace — different CompositionDefinitions are
// different CRDs, so `my-app` can exist as two kinds at once — and the shape test cannot tell
// their releases apart, because after recreation there is no UID left to check the suffix against:
//
//	A recreated, A's old release present  -> 2 candidates -> refuse, and A never converges again
//	A recreated, A's old release GONE     -> 1 candidate  -> A ADOPTS B'S RELEASE
//
// The second is a helm upgrade of B's release with A's chart: one composition taking another's
// workloads, which is the outcome ErrAmbiguousRelease exists to prevent, reached through the arm
// that does not fire. And the name can be overridden outright — SetReleaseName is write-once, so a
// krateo.io/release-name on the object wins and need not begin with the CR name at all, in which
// case the shape test filters out the composition's OWN release and mints a second one, leaving
// #303 unfixed for exactly those compositions.
//
// WHY THESE FOUR FIELDS. plumbing's post-renderer stamps eight labels on every rendered object and
// helm RECORDS that manifest in the release, so the identity is stored at install time rather than
// re-asserted by us at adoption time. Of the eight:
//
//	composition-group, -resource, -name, -namespace  stable across recreation  -> USED
//	composition-id (the UID)                         new on every recreation   -> must NOT be used
//	composition-installed-version                    CHANGES ON EVERY BUMP     -> must NOT be used
//	composition-kind                                 redundant with -resource
//	krateo-namespace                                 not part of the identity
//
// composition-id and composition-installed-version are the two that would look most like
// precision and would each defeat the whole purpose: a bump is precisely a version change, and a
// recreation is precisely a new UID.
//
// # Why excluding the version is load-bearing, and not merely convenient
//
// MEASURED ON a0-openstack-d4, pre-#322 (2.13.57), during an ordinary 0.3.32 -> 0.3.33 bump. The
// two halves of #303 are CAUSALLY COUPLED, and the coupling runs through the rendered selector:
//
//  1. the parent re-renders: the old CR is deleted, a new one created
//
//  2. the old CR's Delete detects a version-migration handover and SKIPS the uninstall, so the
//     old release's objects stay in the cluster (correct -- uninstalling mid-bump destroys
//     stateful children, #168)
//
//  3. the new CR mints a release name from its own new UID, and a chart's workload selector is
//     DERIVED FROM THE RELEASE NAME:
//
//     DaemonSet.apps "openvswitch" is invalid: spec.selector: Invalid value:
//     {"matchLabels":{"app.kubernetes.io/instance":"openvswitch-54xd86b6", ...}}:
//     field is immutable
//
//     The live object carries instance=openvswitch-d2z2v8c9 from the surviving release. The
//     selector is immutable, so the install is REFUSED.
//
//  4. so the second release does not merely sit beside the first -- it CANNOT WORK, and the
//     composition retries the upgrade forever (six revisions, none deployed)
//
// So adoption is not only about release COUNT. Adopting the survivor means the rendered selector
// is the one the live objects already carry, which is why there is no immutability error and no
// second release at all. **A changed release name is an unpatchable object**, and the version being
// absent from this identity is what lets the name stay the same across a bump.
//
// TestCompositionIdentity_ExcludesTheUIDAndTheVersion is therefore load-bearing for a reason its
// own comment did not state: not "the identity should be stable" as a principle, but "an unstable
// identity renames the release, and renaming the release renders a selector Kubernetes will not
// accept". Found by the installer session reading the full create error, which I had only seen
// truncated.
//
// The same error appears on a0-openstack-d3's mariadb, months older -- `spec.selector: field is
// immutable` on a self-heal apply, with Services bound to the failed release. Two clusters, one
// defect at different ages.
type compositionIdentity struct {
	Group     string
	Resource  string
	Name      string
	Namespace string
}

func (c compositionIdentity) String() string {
	return fmt.Sprintf("%s/%s %s/%s", c.Group, c.Resource, c.Namespace, c.Name)
}

// identityFor is the identity to look for, built from the CR itself. The GVR comes from the same
// pluralizer the post-renderer used, so the comparison is between two values produced the same way
// rather than between a stored label and a locally reconstructed plural.
func identityFor(gvr schema.GroupVersionResource, name, namespace string) compositionIdentity {
	return compositionIdentity{
		Group:     gvr.Group,
		Resource:  gvr.Resource,
		Name:      name,
		Namespace: namespace,
	}
}

// identityFromLabels reads an identity off one rendered object's labels, reporting false when the
// object does not carry a complete one.
//
// ALL FOUR OR NOTHING. A partial match is not a weaker match, it is a different claim: an object
// carrying only composition-name would match every composition of that name in the namespace,
// which is the exact confusion this type exists to remove.
func identityFromLabels(l map[string]string) (compositionIdentity, bool) {
	id := compositionIdentity{
		Group:     l[labelCompositionGroup],
		Resource:  l[labelCompositionResource],
		Name:      l[labelCompositionName],
		Namespace: l[labelCompositionNamespace],
	}
	// Group is legitimately empty for core-group objects, but a composition's group never is:
	// compositions live under composition.krateo.io. So an empty group here means the label was
	// absent, not that the group is "".
	if id.Group == "" || id.Resource == "" || id.Name == "" || id.Namespace == "" {
		return compositionIdentity{}, false
	}
	return id, true
}

// releaseCandidate is a stored release reduced to what the adoption decision needs.
type releaseCandidate struct {
	// Name is the release name as helm holds it.
	Name string
	// Identities are the distinct composition identities stamped on the release's recorded
	// objects. Normally exactly one; more than one would mean a release rendered for two
	// compositions, which nothing produces today and which must not be adopted either way.
	Identities []compositionIdentity
	// Decoded reports whether the release's manifest could be read at all. A release whose
	// manifest is empty or undecodable yields NO EVIDENCE, and no evidence must never read as
	// evidence of a match — see pickAdoptableRelease.
	Decoded bool
}

// Owns reports whether this release's recorded objects claim the given composition.
func (c releaseCandidate) Owns(want compositionIdentity) bool {
	for _, id := range c.Identities {
		if id == want {
			return true
		}
	}
	return false
}

// candidateFromRelease reduces a stored release to its recorded composition identities.
//
// The manifest is helm's own record of what it installed, so this reads a fact rather than
// recomputing one. Note that helm keeps lifecycle HOOKS out of Manifest by construction
// (releaseutil.SortManifests splits them), which is how #311 happened: a chart whose only objects
// are hooks records an empty manifest and therefore carries no identity at all. That case must
// surface as no-evidence, not as a non-match, which is why Decoded is reported separately.
func candidateFromRelease(rel *helmconfig.Release) releaseCandidate {
	cand := releaseCandidate{Name: rel.Name}

	objs, _, err := processor.DecodeMinRelease(rel)
	if err != nil {
		return cand
	}
	cand.Decoded = true

	seen := make(map[compositionIdentity]struct{}, 1)
	for i := range objs {
		id, ok := identityFromLabels(objs[i].GetLabels())
		if !ok {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		cand.Identities = append(cand.Identities, id)
	}
	return cand
}

package composition

import (
	"context"
	"fmt"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
	"github.com/krateo-platformops/plumbing/helm/v3"
	xcontext "github.com/krateo-platformops/unstructured-runtime/pkg/context"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	compositionMeta "github.com/krateo-platformops/composition-dynamic-controller/pkg/meta"
)

// adoptableStates are the release statuses whose objects may still be in the cluster, and therefore
// the ones worth adopting.
//
// AN EXPLICIT MASK IS LOAD-BEARING, AND MY FIRST STATEMENT OF WHY WAS WRONG. This comment used to
// say "ListDeployed alone is the default", which is true of neither layer:
//
//	plumbing helm/v3/config.go:113   applyListConfig OVERWRITES client.StateMask unconditionally,
//	                                 so an unset ListConfig.StateMask sets the mask to 0 -- and
//	                                 helm's filterStateMask skips every release when the mask is
//	                                 0, returning an EMPTY list with a nil error (plumbing#74).
//	helm action.NewList              sets ListDeployed|ListFailed. Its own comment says
//	                                 "The default is ListDeployed" and is wrong about its own code.
//
// So the real hazard is worse than a narrow default: omitting the mask here would have listed
// NOTHING, every time, and "no release exists" means install a second one beside the live one --
// the exact outage #303 is about. The plausible value is the destructive one.
//
// What the mask has to span is still the same, and the measured case proves it: the release that
// caused #303's outage was `failed` at revision 6, sitting beside a second `deployed` release at
// revision 2. A deployed-only list would have found the wrong one.
//
// And for anyone reaching for the CLI to look: a `failed` release is NOT hidden by default --
// `helm list` shows deployed AND failed, measured on v3.19.0 by the openstack-ironic-operator-kog
// session, and stated in helm's own `list --help`. What `--all` adds is `superseded`,
// `uninstalled`, `uninstalling` and the `pending-*` family, and `pending-install` is the one that
// matters here: a second install that never completed is invisible without it.
//
// Uninstalled and uninstalling are deliberately EXCLUDED: their objects are gone, helm cannot
// upgrade a release with no deployed revision, and adopting such a name would turn the install into
// a failure rather than an upgrade.
const adoptableStates = helmconfig.ListDeployed |
	helmconfig.ListFailed |
	helmconfig.ListPendingInstall |
	helmconfig.ListPendingUpgrade |
	helmconfig.ListPendingRollback |
	helmconfig.ListSuperseded |
	helmconfig.ListUnknown

// ErrAmbiguousRelease reports that more than one existing release could belong to this composition,
// so adoption cannot be decided.
//
// REFUSING IS THE SAFE ANSWER HERE AND MINTING IS NOT. Adopting the wrong release hands this
// composition another component's workloads, which is strictly worse than the split this adoption
// exists to prevent; and minting a fresh name would silently reproduce that split. An error is the
// only answer that is neither.
var ErrAmbiguousRelease = fmt.Errorf("more than one existing helm release could belong to this composition")

// ErrReleaseNameTaken reports that the name this composition would install under is already held by
// a release whose recorded objects belong to a DIFFERENT composition.
//
// This is the case Diego named: two compositions can share a name in one namespace, because
// different CompositionDefinitions are different CRDs. With safeReleaseName off the release name IS
// the CR name, so both compositions compute the same name and the second one to arrive would
// UPGRADE the first one's release — replacing its objects with a different chart's. With
// safeReleaseName on the same collision is reachable through an overridden krateo.io/release-name.
//
// Refusing is the only safe answer. Installing would destroy the other composition's workloads;
// minting a different name silently is the #303 split under a new cause. An error names both
// compositions so an operator can rename one.
var ErrReleaseNameTaken = fmt.Errorf("the helm release name this composition would use is already held by another composition")

// resolveReleaseName returns the release name this composition's helm release is stored under,
// ADOPTING an existing release for the same name and namespace rather than minting a new one from
// the CR's UID.
//
// # Why this exists
//
// Diego's statement of the requirement: *when a new version is served in a generated CRD,
// core-provider should not uninstall and reinstall but do a helm upgrade.*
//
// It did the opposite, and the decision was one line. CalculateReleaseName derives the name from
// the CR's UID, so a composition CR that is RECREATED — which is what happens when the thing
// declaring it emits a new version-derived apiVersion — gets a name no release holds:
//
//	composition.go  releaseName := <CR name>-<hash(UID[:8])>
//	composition.go  GetRelease(releaseName) -> nil
//	                ResourceExists: false
//	                -> the runtime routes to Create -> helm INSTALL of a SECOND release
//
// Nothing uninstalls the first. The component's objects then split across the two releases BY
// MUTABILITY: the workload's spec.selector is immutable so the old release stays `failed` and keeps
// owning the Services; the Services are mutable — helm would update them happily — but they belong
// to that failed release, so they never were.
//
// Measured on a0-openstack-d3: three Services selecting a release nothing carries, zero endpoints,
// beside a 1/1 Ready pod carrying the other release's labels. 100% of keystone token issuance
// failing for 40 hours, 13 CronJobs erroring every minute — with every object reporting Ready.
//
// # The identity, and the one this started with
//
// A recreated CR shares nothing with its predecessor except group, resource, namespace and name:
// the UID is new, and the write-once release-name label and origin-version annotation live on the
// object that was deleted.
//
// The first version of this took the next step and matched the release NAME, by shape — prefix plus
// a suffix of the generator's length — on the reasoning that the name encodes name+namespace and so
// IS the identity. Diego refuted it: two compositions can share a name in one namespace, because
// different CompositionDefinitions are different CRDs, so `db` can exist as a MariaDB and a
// PostgreSQL at once. A shape test cannot tell their releases apart, and after recreation there is
// no UID left to check a suffix against. The failure was not the refusal — it was that with the
// composition's own release gone, the sibling's was the SINGLE candidate and would have been
// adopted: a helm upgrade of one composition's release with another's chart. He also named the
// second hole: the name can be overridden, since SetReleaseName is write-once, so a matcher keyed
// on the CR name filters out the composition's own release and mints a second one.
//
// What replaced it is the identity plumbing's post-renderer already STAMPS on every rendered object
// and helm RECORDS in the release manifest — group, resource, name, namespace, deliberately
// without the UID or the served version. See compositionIdentity for why those two are excluded.
// Read rather than recomputed, and it never looks at the release's name.
//
// # The hot path pays nothing
//
// A CR that already carries the release-name label returns immediately, with no List. Only a CR
// without one — a genuine first reconcile, or a recreation — reaches helm's storage.
func (h *handler) resolveReleaseName(ctx context.Context, mg *unstructured.Unstructured) (string, error) {
	log := xcontext.Logger(ctx).
		WithValues("name", mg.GetName()).
		WithValues("namespace", mg.GetNamespace())

	// WRITE-ONCE WINS, and this ordering is the whole reason adoption is safe to add. An
	// established composition's label is authoritative: it was set on first reconcile and names the
	// release it has been upgrading ever since. Consulting helm for it would be both pointless and
	// a chance to change the answer.
	if existing := compositionMeta.GetReleaseName(mg); existing != "" {
		return existing, nil
	}

	minted := compositionMeta.CalculateReleaseName(mg, h.safeReleaseName)

	// NO EARLY RETURN FOR !safeReleaseName ANY MORE, and that deletion is the point of this change.
	// It used to return here on the reasoning that "the release name IS the CR name, so a recreated
	// CR already resolves to the same release and there is nothing to adopt". The first clause is
	// true and the conclusion does not follow: when the CR name is the release name, two
	// compositions sharing a name in one namespace resolve to the SAME release, and the second to
	// arrive upgrades the first one's release with its own chart. There is nothing to adopt and
	// something to REFUSE, and the early return skipped the check that could refuse it.

	hc, err := helm.NewClient(h.kubeconfig, append(h.helmCacheOpts(),
		helm.WithNamespace(mg.GetNamespace()),
		helm.WithCache(),
	)...)
	if err != nil {
		return "", fmt.Errorf("creating helm client to resolve the release name: %w", err)
	}
	defer hc.Close()

	releases, err := hc.ListReleases(ctx, &helmconfig.ListConfig{
		All:       true,
		StateMask: adoptableStates,
	})
	if err != nil {
		// RETURNED, NEVER DEGRADED TO "MINT A FRESH NAME". A list failure means we could not
		// determine whether a release already exists, and answering "no" installs a second one —
		// which is the defect. Refusing costs a requeue; guessing costs the outage.
		return "", fmt.Errorf("listing helm releases to resolve the release name: %w", err)
	}

	gvr, err := h.pluralizer.GVKtoGVR(mg.GroupVersionKind())
	if err != nil {
		// RETURNED, NOT DEGRADED. Without the GVR there is no identity to match on, and the
		// fallbacks are both destructive: matching on name alone is what adopts another
		// composition's release, and minting installs a second one.
		return "", fmt.Errorf("resolving the composition GVR to match recorded releases against: %w", err)
	}
	want := identityFor(gvr, mg.GetName(), mg.GetNamespace())

	candidates := make([]releaseCandidate, 0, len(releases))
	for _, rel := range releases {
		if rel == nil {
			continue
		}
		if rel.Namespace != "" && rel.Namespace != mg.GetNamespace() {
			// ListReleases is namespace-scoped by the client, but filter anyway: adopting across
			// namespaces would be the worst available mistake and the check is one comparison.
			continue
		}
		candidates = append(candidates, candidateFromRelease(rel))
	}

	adopted, err := pickAdoptableRelease(candidates, want, minted)
	if err != nil {
		return "", fmt.Errorf("%w (composition %s/%s)", err, mg.GetNamespace(), mg.GetName())
	}
	if adopted != minted {
		log.Info("Adopting the existing helm release for this composition rather than installing a second one",
			"adopted", adopted, "would_have_minted", minted)
	}
	return adopted, nil
}

// pickAdoptableRelease decides which release name to use, given the releases helm already holds
// reduced to the composition identities RECORDED in them.
//
// SEPARATED FROM THE LISTING ON PURPOSE. The decision is the part that can be wrong in a way that
// costs workloads, and entangled with a helm client it would only be testable against a live
// cluster — so the arms that matter (adopt, mint, refuse an ambiguity, refuse a taken name, and
// refuse on no evidence) would go untested. Everything above this function is I/O; everything in
// it is judgement.
func pickAdoptableRelease(candidates []releaseCandidate, want compositionIdentity, minted string) (string, error) {
	var owned []string
	var takenBy []compositionIdentity
	mintedPresentWithoutEvidence := false

	for _, c := range candidates {
		if c.Owns(want) {
			owned = append(owned, c.Name)
			continue
		}
		if c.Name != minted {
			continue
		}
		// A release is already stored under the exact name this composition would install as, and
		// its recorded objects do not claim this composition.
		if len(c.Identities) == 0 {
			mintedPresentWithoutEvidence = true
			continue
		}
		takenBy = append(takenBy, c.Identities...)
	}

	switch len(owned) {
	case 1:
		// The whole point. When the name equals minted this is a re-reconcile before the label was
		// persisted; when it differs, the CR was recreated and returning the recorded name makes
		// Observe report ResourceExists=true, so the runtime routes to Update and helm UPGRADES the
		// release that is already there instead of installing a second one beside it.
		return owned[0], nil
	case 0:
		// fall through to the no-owned-release cases below
	default:
		// Two stored releases both claiming this composition. Nothing produces that today, and
		// picking either would be a guess about which one holds the live objects. See
		// ErrAmbiguousRelease.
		return "", fmt.Errorf("%w: %v", ErrAmbiguousRelease, owned)
	}

	if len(takenBy) > 0 {
		return "", fmt.Errorf("%w: release %q is recorded as belonging to %v", ErrReleaseNameTaken, minted, takenBy)
	}

	if mintedPresentWithoutEvidence {
		// A release under this exact name carrying NO composition labels at all: installed before
		// the post-renderer stamped them, or a hooks-only chart whose recorded manifest is empty by
		// construction (helm splits hooks out of Manifest — core-provider#311).
		//
		// Returning minted is what happened before this adoption existed: Create would GetRelease
		// on this name, find it, and upgrade. So this arm is not a new risk, it is the old
		// behaviour kept deliberately for the one case where there is nothing better to go on —
		// and it is an arm rather than a fallthrough so that nobody later reads the absence of
		// evidence as evidence of absence.
		return minted, nil
	}

	// No release claims this composition and nothing holds its name: a genuine first install.
	return minted, nil
}

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
// ListDeployed ALONE IS THE DEFAULT AND WOULD MISS THE CASE THIS EXISTS FOR. The release that
// caused #303's measured outage was `failed` at revision 6, sitting beside a second `deployed`
// release at revision 2 — so a deployed-only list would have found the wrong one, or on a cluster
// with only the failed release, nothing at all, and minted a new name exactly as before.
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
// # Why name+namespace is the identity, and not something better
//
// A recreated CR shares NOTHING with its predecessor except kind, namespace and name: the UID is
// new, and the write-once release-name label and origin-version annotation live on the object that
// was deleted. So name+namespace is not a convenient identity, it is the only one available — and
// the release name encodes it. ReleaseNameCouldBelongTo is kept beside CalculateReleaseName and
// shares its length constant so the two cannot drift.
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

	if !h.safeReleaseName {
		// The release name IS the CR name, so a recreated CR already resolves to the same release
		// and there is nothing to adopt. Returning early also keeps the List off a path that cannot
		// benefit from it.
		return minted, nil
	}

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

	names := make([]string, 0, len(releases))
	for _, rel := range releases {
		if rel == nil {
			continue
		}
		if rel.Namespace != "" && rel.Namespace != mg.GetNamespace() {
			// ListReleases is namespace-scoped by the client, but filter anyway: adopting across
			// namespaces would be the worst available mistake and the check is one comparison.
			continue
		}
		names = append(names, rel.Name)
	}

	adopted, err := pickAdoptableRelease(names, mg.GetName(), minted, h.safeReleaseName)
	if err != nil {
		return "", fmt.Errorf("%w (composition %s/%s)", err, mg.GetNamespace(), mg.GetName())
	}
	if adopted != minted {
		log.Info("Adopting the existing helm release for this composition rather than installing a second one",
			"adopted", adopted, "would_have_minted", minted)
	}
	return adopted, nil
}

// pickAdoptableRelease decides which release name to use, given the names helm already holds.
//
// SEPARATED FROM THE LISTING ON PURPOSE. The decision is the part that can be wrong in a way that
// costs workloads, and entangled with a helm client it would only be testable against a live
// cluster — so the arms that matter (adopt, mint, refuse, and the near-miss name that must NOT be
// adopted) would go untested. Everything above this function is I/O; everything in it is judgement.
func pickAdoptableRelease(existing []string, crName, minted string, safeReleaseName bool) (string, error) {
	var candidates []string
	for _, name := range existing {
		if compositionMeta.ReleaseNameCouldBelongTo(name, crName, safeReleaseName) {
			candidates = append(candidates, name)
		}
	}

	switch len(candidates) {
	case 0:
		// No prior release: this really is a first install, and the minted name is correct.
		return minted, nil

	case 1:
		// When the single candidate EQUALS minted this is just a re-reconcile before the label was
		// persisted; when it differs, the CR was recreated and this is the fix — returning the
		// existing name makes Observe report ResourceExists=true, so the runtime routes to Update
		// and helm UPGRADES the release that is already there.
		return candidates[0], nil

	default:
		// Ambiguous. Neither adopting nor minting is safe, so refuse and name the candidates so an
		// operator can resolve it by hand. See ErrAmbiguousRelease.
		return "", fmt.Errorf("%w: %v", ErrAmbiguousRelease, candidates)
	}
}

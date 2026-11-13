package crd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	apiextensionsscheme "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/scheme"
	clientsetscheme "k8s.io/client-go/kubernetes/scheme"

	contexttools "github.com/krateo-platformops/core-provider/internal/tools/context"
	"github.com/krateo-platformops/core-provider/internal/tools/crd/generation"
	"github.com/krateo-platformops/core-provider/internal/tools/kube"
	"github.com/krateo-platformops/core-provider/internal/tools/kube/watcher"
	"github.com/krateo-platformops/core-provider/internal/tools/retry"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/gengo/namer"
	"k8s.io/gengo/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	crdRetryAttempts     = 10
	crdRetryInitialDelay = 100 * time.Millisecond
	crdRetryMaximumDelay = 100 * time.Millisecond
)

func InferGroupResource(gk schema.GroupKind) schema.GroupResource {
	kind := types.Type{Name: types.Name{Name: gk.Kind}}
	namer := namer.NewPrivatePluralNamer(nil)
	return schema.GroupResource{
		Group:    gk.Group,
		Resource: strings.ToLower(namer.Name(&kind)),
	}

}

func Uninstall(ctx context.Context, kube client.Client, gr schema.GroupResource) error {
	if err := registerEventually(); err != nil {
		return err
	}

	_, err := retry.Do[struct{}](ctx, retry.Config[struct{}]{
		Attempts:     crdRetryAttempts,
		InitialDelay: crdRetryInitialDelay,
		MaximumDelay: crdRetryMaximumDelay,
		Retryable:    isRetryableCRDError,
	}, func(context.Context) (struct{}, error) {
		obj := apiextensionsv1.CustomResourceDefinition{}
		err := kube.Get(ctx, client.ObjectKey{Name: gr.String()}, &obj, &client.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return struct{}{}, nil
			}

			return struct{}{}, err
		}

		err = kube.Delete(ctx, &obj, &client.DeleteOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return struct{}{}, nil
			}

			return struct{}{}, err
		}

		return struct{}{}, nil
	})
	return err
}

func Get(ctx context.Context, kube client.Reader, gr schema.GroupResource) (*apiextensionsv1.CustomResourceDefinition, error) {
	if err := registerEventually(); err != nil {
		return nil, err
	}

	res := apiextensionsv1.CustomResourceDefinition{}
	err := kube.Get(ctx, client.ObjectKey{Name: gr.String()}, &res, &client.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		} else {
			return nil, err
		}
	}

	// Typed reads do not always populate TypeMeta (notably with the direct, non-cached
	// client used for remote targets). Downstream apply derives the GVK from the object,
	// so set it explicitly to keep CRD updates working regardless of the client.
	res.SetGroupVersionKind(apiextensionsv1.SchemeGroupVersion.WithKind("CustomResourceDefinition"))

	return &res, nil
}

func Lookup(ctx context.Context, kube client.Reader, gvr schema.GroupVersionResource) (bool, error) {
	if err := registerEventually(); err != nil {
		return false, err
	}

	res := apiextensionsv1.CustomResourceDefinition{}
	err := kube.Get(ctx, client.ObjectKey{Name: gvr.GroupResource().String()}, &res, &client.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		} else {
			return false, err
		}
	}

	for _, el := range res.Spec.Versions {
		if el.Name == gvr.Version {
			return true, nil
		}
	}

	return false, nil
}

func isRetryableCRDError(err error) bool {
	if err == nil {
		return false
	}

	if errors.Is(err, context.Canceled) {
		return false
	}

	return true
}

// UpdateVersion updates the given CRD to set the given version spec. If the version
// does not exist, the CRD is created
// If the version exists, its specs are updated
// Before returning, the CRD is applied to the cluster and waited to be established with a watcher
// Returns an error if any occurs
func ApplyOrUpdateCRD(ctx context.Context,
	cli client.Client,
	dyn dynamic.Interface,
	newcrd *apiextensionsv1.CustomResourceDefinition,
) (schema.GroupVersionResource, error) {

	log := contexttools.LoggerFromCtx(ctx, logging.NewNopLogger())

	// Getting GVR from CRD
	gvr := schema.GroupVersionResource{
		Group:    newcrd.Spec.Group,
		Version:  newcrd.Spec.Versions[0].Name,
		Resource: newcrd.Spec.Names.Plural,
	}

	// Surface the per-instance served version (#234B) in kubectl. Done on newcrd before any path so
	// the create path applies it and the append path (AppendVersion) carries it onto the new version.
	generation.AddCompositionVersionColumn(newcrd)

	crd, err := Get(ctx, cli, gvr.GroupResource())
	if err != nil {
		return gvr, fmt.Errorf("error getting CRD: %w", err)
	}

	if crd == nil {
		log.Debug("Creating CRD", "gvr", gvr.String())
		_, err = kube.Apply(ctx, dyn, apiextensionsv1.SchemeGroupVersion.WithResource("customresourcedefinitions"), newcrd, kube.ApplyOptions{})
		if err != nil {
			return gvr, fmt.Errorf("error applying CRD: %w", err)
		}
		err = watcher.NewWatcher(
			dyn,
			apiextensionsv1.SchemeGroupVersion.WithResource("customresourcedefinitions"),
			1*time.Minute,
			IsReady).WatchResource(ctx, "", newcrd.Name)
		if err != nil {
			return gvr, fmt.Errorf("error waiting for CRD to be established: %w", err)
		}

		return gvr, nil
	}
	log.Debug("Updating CRD", "gvr", gvr.String())

	statusEqual, err := generation.StatusEqual(crd, newcrd)
	if err != nil {
		return gvr, fmt.Errorf("error comparing CRD status: %w", err)
	}
	// The status change and the version bump are INDEPENDENT: a single reconcile can carry
	// both (a statusDataTemplate edit landing in the same generation as a chart version bump,
	// or a core-provider upgrade that changes the embedded status.schema.json for every
	// definition at once). They were previously exclusive — the status branch returned before
	// the append could run, so ApplyOrUpdateCRD reported the new GVR with err == nil having
	// never added that version to the CRD, and the caller went on to build a CDC
	// Deployment/SA for it and migrate instances through an endpoint the apiserver does not
	// serve (#190). Both mutations are now accumulated and applied once.
	mutated := false

	// #207: repair a CRD whose vacuum storage version is gone from spec.versions while
	// status.storedVersions still names it — objects encoded at a version the CRD no longer
	// declares, so every read of the Kind fails with "invalid group/version".
	//
	// BEFORE the append below, deliberately. The append is the only other place vacuum is created,
	// and it is unreachable for a CRD that already carries the version being reconciled — which is
	// exactly the damaged CRD's situation. Repairing first means the single-storage-version
	// bookkeeping is settled before anything else touches spec.versions.
	//
	// A no-op in every state but that one. In particular it does NOT fire on a CRD that simply has
	// no vacuum: a never-bumped CRD legitimately has one version that is both served and storage,
	// and injecting vacuum there would demote it and strand vacuum in storedVersions. See
	// generation.RestoreOrphanedVacuum.
	if generation.RestoreOrphanedVacuum(crd) {
		log.Debug("restored the vacuum storage version orphaned in status.storedVersions",
			"crd", crd.Name, "storedVersions", crd.Status.StoredVersions)
		mutated = true
	}

	if !statusEqual {
		log.Debug("CRD status differs, updating status", "crd", crd.Name, "version", gvr.Version)
		// To avoid issues with changed in the dynamically generated part of the CRD (the spec), we only update the static part (the status) to any version that already exists
		err = generation.UpdateStatus(crd, newcrd.Spec.Versions[0])
		if err != nil {
			return gvr, fmt.Errorf("error updating CRD version: %w", err)
		}
		// Backfill the VERSION printer column onto any pre-existing served versions on the live CRD
		// (versions created before this feature only got it on new appends otherwise). Idempotent.
		generation.AddCompositionVersionColumn(crd)
		mutated = true
	}

	if generation.GVKExists(crd, schema.GroupVersionKind{
		Group:   newcrd.Spec.Group,
		Kind:    newcrd.Spec.Names.Kind,
		Version: gvr.Version,
	}) {
		log.Debug("CRD version already present, skipping append", "crd", crd.Name, "version", gvr.Version)
	} else {
		crd, err = generation.AppendVersion(*crd, *newcrd)
		if err != nil {
			return gvr, fmt.Errorf("error appending version to CRD: %w", err)
		}

		// The generated CRD's served versions are all schema-passthrough (the old conversion
		// webhook only copied metadata/spec/status verbatim), so the built-in None converter
		// is equivalent and needs no webhook to reach. Lossless storage across heterogeneous
		// version schemas is provided by the permissive "vacuum" storage version, not by
		// conversion. This works identically for local and remote targets.
		setNoneConversion(crd)

		generation.SetServedStorage(crd, gvr.Version, true, false)
		mutated = true
	}

	if !mutated {
		log.Debug("CRD version exists and is equal, skipping update", "crd", crd.Name, "version", gvr.Version)
		return gvr, nil
	}

	_, err = kube.Apply(ctx, dyn, apiextensionsv1.SchemeGroupVersion.WithResource("customresourcedefinitions"), crd, kube.ApplyOptions{})
	if err != nil {
		return gvr, fmt.Errorf("error applying CRD update: %w", err)
	}

	// WAITING ON servesVersion, NOT IsReady (#339). This is the UPDATE path: the CRD already
	// exists, so Established is already True and never goes False here -- passing IsReady made this
	// wait return immediately, having proven only that the object exists. It did not prove that the
	// version this reconcile just appended is one the apiserver will answer on, which is the single
	// fact every caller downstream assumes: the controller goes on to build a cdc Deployment for
	// that GVR, and installer's umbrella asks `inst.crdExists <kind> <new version>` with the answer
	// gating whether live compositions are pruned.
	//
	// The CREATE path above keeps IsReady deliberately. There, Established genuinely transitions
	// false->true and the CRD has exactly one version, so the two predicates agree; the asymmetry
	// is the point rather than an oversight.
	err = watcher.NewWatcher(
		dyn,
		apiextensionsv1.SchemeGroupVersion.WithResource("customresourcedefinitions"),
		1*time.Minute,
		servesVersion(gvr.Version)).WatchResource(ctx, "", crd.Name)
	if err != nil {
		// The reason is read off the object we have rather than inferred from the timeout, so the
		// log distinguishes "our apply never landed" from "the apiserver has the version and is
		// not serving it" -- two different problems that a timeout alone reports identically.
		live, gerr := Get(ctx, cli, gvr.GroupResource())
		why := "could not re-read the CRD to say why"
		if gerr == nil {
			if d := ServedVersionDisagreement(live, gvr.Version); d != "" {
				why = d
			} else {
				why = "the CRD now agrees, so the wait lost a race rather than finding a problem"
			}
		}
		return gvr, fmt.Errorf("error waiting for CRD to serve version %q (%s): %w", gvr.Version, why, err)
	}

	return gvr, nil
}

func setNoneConversion(crd *apiextensionsv1.CustomResourceDefinition) {
	crd.Spec.Conversion = &apiextensionsv1.CustomResourceConversion{
		Strategy: apiextensionsv1.NoneConverter,
	}
}

func IsReady(crd *apiextensionsv1.CustomResourceDefinition) bool {
	if crd != nil {
		for _, cond := range crd.Status.Conditions {
			if cond.Type == apiextensionsv1.Established && cond.Status == apiextensionsv1.ConditionTrue {
				return true
			}
		}
	}
	return false
}

// ServedVersionDisagreement reports why the live CRD does not yet serve `version`, or "" when it
// does. The returned string is a reason, never a bare bool, because the two ways this fails are
// different problems: a version absent from spec.versions means our own apply has not landed, and
// a version present but not served means the apiserver has it and is refusing to answer on it.
//
// WHY THIS IS NOT IsReady. Established says the CRD's names were accepted and the resource is
// servable. It is silent about WHICH versions are served, and for an existing CRD it is already
// True and NEVER GOES FALSE across an update -- so on a version bump the Established gate is
// satisfied for the entire window in which the apiserver does not yet answer on the new version
// (#339).
//
// WHAT THAT WINDOW COSTS, measured rather than imagined. installer's umbrella gates every
// composition on `inst.crdExists <kind> <component's new version>`, with the `$crd` term ANDed
// OUTSIDE the ($deps or $exists) escape -- so when the gate is false the escape is unreachable and
// the composition is pruned rather than left alone. The upstream openstack umbrella had the same
// shape and pruned 34 live CRs at a bump instant (openstack#166, 6 of 6 survivors carrying
// origin-version=v0-3-33). The installer session measured zero prunes across many pin bumps and
// said the right thing about it: "it did not happen on these rolls" is not "it cannot happen".
// This is the mechanism by which it can, and the reason their result was a race they were winning.
func ServedVersionDisagreement(live *apiextensionsv1.CustomResourceDefinition, version string) string {
	if live == nil {
		// NOT "agreement". A nil CRD cannot be shown to serve anything, and on a gate whose false
		// answer prunes live resources, "could not determine" must never read as "nothing to do".
		return "the CRD could not be read, so it cannot be shown to serve any version"
	}
	if !IsReady(live) {
		return "the CRD is not Established"
	}
	for _, v := range live.Spec.Versions {
		if v.Name != version {
			continue
		}
		if !v.Served {
			return fmt.Sprintf("version %q is present in spec.versions but served=false, so the "+
				"apiserver has it and will not answer on it", version)
		}
		return ""
	}
	return fmt.Sprintf("version %q is absent from spec.versions (the CRD declares %s), so the "+
		"apply has not landed yet", version, strings.Join(declaredVersions(live), ", "))
}

func declaredVersions(crd *apiextensionsv1.CustomResourceDefinition) []string {
	out := make([]string, 0, len(crd.Spec.Versions))
	for _, v := range crd.Spec.Versions {
		out = append(out, v.Name)
	}
	return out
}

// servesVersion builds a watcher predicate for ServedVersionDisagreement.
//
// A FACTORY RATHER THAN A SECOND IsReady: the watcher takes func(*CRD) bool, and the version to
// wait for is only known at the call site. Passing IsReady there is what made the update path's
// wait vacuous, and a predicate that cannot see the version cannot be anything else.
func servesVersion(version string) func(*apiextensionsv1.CustomResourceDefinition) bool {
	return func(crd *apiextensionsv1.CustomResourceDefinition) bool {
		return ServedVersionDisagreement(crd, version) == ""
	}
}

func registerEventually() error {
	if clientsetscheme.Scheme.IsGroupRegistered(apiextensionsv1.SchemeGroupVersion.Group) {
		return nil
	}

	return apiextensionsscheme.AddToScheme(clientsetscheme.Scheme)
}

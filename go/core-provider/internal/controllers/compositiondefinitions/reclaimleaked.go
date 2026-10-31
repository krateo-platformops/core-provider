package compositiondefinitions

import (
	"context"
	"fmt"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/krateo-platformops/core-provider/internal/controllers/compositiondefinitions/helpers/getters"
	crdclient "github.com/krateo-platformops/core-provider/internal/tools/crd"
)

// A per-version controller Deployment is named "<resource>-<version>-controller" by
// deploy.resourceNamer + getCDCDeploymentNN. Kept here as constants rather than imported so this
// file does not reach into the deploy package's unexported helpers; asserted against a real
// rendered name in the test.
const (
	leakedControllerSuffix = "-controller"
)

// leakedController is a cdc Deployment that core-provider created and then failed to retire.
type leakedController struct {
	Name    string
	Version string
}

// findLeakedControllers returns the per-version controller Deployments for this Kind that nothing
// can be using, and the reasons any candidate was KEPT.
//
// #270. A retirement that ran while 2.13.45's drain wedge was live aborted `Undeploy` partway: the
// Deployment delete had already succeeded or had not yet been issued, and the reconcile errored
// out. On 057 that left `installers-v0-3-480-controller` running at 2.13.45 two installer versions
// after it should have gone, invisible to core-provider's rewrite because the rewrite only touches
// controllers still in the desired set.
//
// It was inert — its CRD version no longer existed, so its informer watched nothing — but inertness
// is not something a cleanup may ASSUME. The predicate below establishes it.
//
// DELETING A CLAIMED CONTROLLER WOULD BE FAR WORSE THAN LEAVING DEBRIS, so this inherits
// prunableServedVersions' conservatism wholesale, including its blanket refusal:
//
//   - if ANY composition is unaccounted (no version label, or a label naming a version the CRD does
//     not carry), nothing is reclaimed at all. Such an instance is invisible to every label-keyed
//     check in this file, so it could be depending on any of these controllers (#189's gap).
//   - a version still present in the CRD's spec.versions is never a candidate, served or not. A
//     non-served version is still STORED data and its controller may be needed for a conversion.
//   - an instance labelled at a version the CRD no longer carries is covered BY the blanket refusal
//     rather than by a separate check: such a label is unaccounted by definition, so the two are the
//     same condition. A surviving mutant proved the separate check unreachable and it was removed.
//   - a version another CompositionDefinition references is kept, via the same predicate the
//     retirement path uses.
func (e *external) findLeakedControllers(ctx context.Context, gvk schema.GroupVersionKind, gvr schema.GroupVersionResource, selfName, selfNamespace string) (leaked []leakedController, kept []string, err error) {
	crd, err := crdclient.Get(ctx, e.kube, gvr.GroupResource())
	if err != nil {
		return nil, nil, fmt.Errorf("fetching CRD: %w", err)
	}
	if crd == nil {
		// No CRD means no reliable notion of which versions exist. Refuse rather than treat every
		// controller as leaked — that would delete the whole fleet for this Kind.
		return nil, []string{"ALL:no-crd"}, nil
	}

	known := make(map[string]bool, len(crd.Spec.Versions))
	knownList := make([]string, 0, len(crd.Spec.Versions))
	for _, v := range crd.Spec.Versions {
		known[v.Name] = true
		knownList = append(knownList, v.Name)
	}

	// THE BLANKET REFUSAL, first and unconditionally. An instance with no usable version label is
	// invisible to every check below, so its controller cannot be identified — and reclaiming
	// anything while one exists risks removing the controller it depends on.
	unaccounted, err := getters.GetUnaccountedCompositions(ctx, e.dynamic, gvr, knownList)
	if err != nil {
		return nil, nil, fmt.Errorf("checking for compositions with no usable version label: %w", err)
	}
	if unaccounted != nil && len(unaccounted.Items) > 0 {
		return nil, []string{fmt.Sprintf("ALL:unaccounted-instances=%d", len(unaccounted.Items))}, nil
	}

	var deps appsv1.DeploymentList
	if err := e.kube.List(ctx, &deps, client.InNamespace(selfNamespace)); err != nil {
		// A failed LIST is not evidence of absence, and here the destructive reading would be to
		// treat an unreadable namespace as having nothing to keep. Refuse.
		return nil, nil, fmt.Errorf("listing deployments in %s: %w", selfNamespace, err)
	}

	prefix := gvr.Resource + "-"
	for i := range deps.Items {
		name := deps.Items[i].Name
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, leakedControllerSuffix) {
			continue
		}
		version := strings.TrimSuffix(strings.TrimPrefix(name, prefix), leakedControllerSuffix)
		if version == "" {
			continue
		}
		if version == gvk.Version {
			kept = append(kept, version+":is-current")
			continue
		}
		if known[version] {
			// Present in the CRD, served or not. A non-served version is still stored data.
			kept = append(kept, version+":version-still-on-crd")
			continue
		}

		// NO per-version instance check here, and its absence is deliberate.
		//
		// I wrote one — GetOwnedCompositionsByVersionLabel for this version — and a surviving
		// compiling mutant proved it is UNREACHABLE. Any composition labelled at a version absent
		// from the CRD is, by definition, "unaccounted" under GetUnaccountedCompositions (its label
		// names a version not in knownList), so the blanket refusal above has already returned.
		// The two conditions are the same condition.
		//
		// Dead code shaped like a safeguard is worse than no code: it invites the next reader to
		// believe this function checks something it does not. The blanket refusal IS the #189
		// protection, and it is strictly stronger — it refuses to reclaim ANYTHING while such an
		// instance exists, rather than sparing one version's controller.

		referenced, rerr := e.versionReferencedByAnotherDefinition(ctx, schema.GroupVersionKind{
			Group: gvk.Group, Kind: gvk.Kind, Version: version,
		}, selfName, selfNamespace)
		if rerr != nil {
			return nil, nil, fmt.Errorf("checking references for %s: %w", version, rerr)
		}
		if referenced {
			kept = append(kept, version+":referenced-by-another-definition")
			continue
		}

		leaked = append(leaked, leakedController{Name: name, Version: version})
	}

	sort.Slice(leaked, func(i, j int) bool { return leaked[i].Name < leaked[j].Name })
	sort.Strings(kept)
	return leaked, kept, nil
}

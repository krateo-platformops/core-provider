package compositiondefinitions

import (
	"context"
	"fmt"

	crdclient "github.com/krateo-platformops/core-provider/internal/tools/crd"
	"github.com/krateo-platformops/core-provider/internal/tools/deploy"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// danglingSweep is what one sweep for #414's dangling controller bindings saw and did.
type danglingSweep struct {
	// Found is every binding that names only a retired controller's absent ServiceAccount.
	Found []deploy.DanglingBinding
	// Kept is every candidate refused, with the reason (its ServiceAccount exists, helm owns it).
	Kept []string
	// Removed is what was deleted: empty in a dry run, which only reports Found.
	Removed []string
	// DryRun is true when nothing was deleted because removal is not enabled.
	DryRun bool
}

// sweepDanglingControllerRBAC finds, and when removal is enabled removes, the RoleBindings and
// ClusterRoleBindings whose every subject is a retired controller version's ServiceAccount, absent
// and off the Kind's CRD (#414, "retire by subject"; the rules are in deploy.FindDanglingControllerBindings).
//
// It runs after the leaked-controller reclaim, so a controller that reclaim just retired has its
// bindings repointed (ResubjectDerivedRBAC) BEFORE this looks: anything left naming the retired
// ServiceAccount is a binding the repoint deliberately does not touch.
//
// The versions come from the LIVE CRD. Without it there is no reliable notion of which versions are
// retired, so a missing or unreadable CRD is an error, never "every version is retired".
//
// Removal is off unless enabled (CORE_PROVIDER_REMOVE_DANGLING_CONTROLLER_RBAC): a dry run reports
// what it would remove, so the first run on a cluster can be checked against a census first.
func (e *external) sweepDanglingControllerRBAC(ctx context.Context, gvr schema.GroupVersionResource, placements []string) (danglingSweep, error) {
	crd, err := crdclient.Get(ctx, e.kube, gvr.GroupResource())
	if err != nil {
		return danglingSweep{}, fmt.Errorf("fetching CRD: %w", err)
	}
	if crd == nil {
		return danglingSweep{}, fmt.Errorf("CRD %s not found: cannot tell which controller versions are retired", gvr.GroupResource())
	}
	known := make([]string, 0, len(crd.Spec.Versions))
	for _, v := range crd.Spec.Versions {
		known = append(known, v.Name)
	}
	found, kept, err := deploy.FindDanglingControllerBindings(ctx, deploy.DanglingOptions{
		DynamicClient: e.dynamic,
		Resource:      gvr.Resource,
		KnownVersions: known,
		Placements:    placements,
	})
	if err != nil {
		return danglingSweep{}, err
	}
	out := danglingSweep{Found: found, Kept: kept, DryRun: !e.removeDanglingRBAC}
	if len(found) == 0 || out.DryRun {
		return out, nil
	}
	out.Removed, err = deploy.RemoveDanglingControllerBindings(ctx, e.dynamic, found)
	return out, err
}

// danglingNames renders bindings for a log line.
func danglingNames(found []deploy.DanglingBinding) []string {
	out := make([]string, 0, len(found))
	for _, f := range found {
		out = append(out, f.String())
	}
	return out
}

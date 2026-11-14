package crd

import (
	"context"
	"fmt"

	"github.com/krateo-platformops/core-provider/internal/tools/crd/generation"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// DropVersions removes the versions in drop from crd.Spec.Versions IN MEMORY, after trimming them
// out of status.storedVersions ON THE APISERVER, and reports whether spec.versions changed. The
// caller applies the spec. vacuum (the storage version) is never dropped.
//
// Why storedVersions first. The apiserver forbids removing a version from spec.versions while it is
// still listed in status.storedVersions (a version that was, at some point, a storage version, e.g.
// the very first served version before vacuum became the permanent storage version), and the whole
// spec update is rejected. Trimming is safe for the versions a caller may pass here: every
// composition is rewritten through the current served endpoint by the migration, so all data is
// persisted at vacuum and none remains at a version the prune predicate found unused.
//
// The ONE implementation of the removal, shared by the standalone prune (pruneStaleServedVersions)
// and by the CRD write that adds a version (ApplyOrUpdateCRDDropping, #484), so the two cannot
// remove versions in different ways.
func DropVersions(ctx context.Context, cli client.Client, crd *apiextensionsv1.CustomResourceDefinition, drop map[string]bool) (bool, error) {
	if len(drop) == 0 {
		return false, nil
	}
	var keptStored []string
	for _, sv := range crd.Status.StoredVersions {
		if sv == "vacuum" || !drop[sv] {
			keptStored = append(keptStored, sv)
		}
	}
	if len(keptStored) != len(crd.Status.StoredVersions) {
		crd.Status.StoredVersions = keptStored
		if err := cli.Status().Update(ctx, crd); err != nil {
			return false, fmt.Errorf("trimming storedVersions before dropping served versions: %w", err)
		}
	}
	return generation.RemoveStaleVersions(crd, drop), nil
}

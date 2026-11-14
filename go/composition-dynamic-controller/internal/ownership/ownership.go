// Package ownership decides whether this composition controller holds a coordination.k8s.io Lease
// before it reconciles (core-provider docs/design/cdc-pooling.md, step 2).
//
// core-provider renders the Lease name, <version>.<plural>.<group>, into the controller's ConfigMap
// (COMPOSITION_CONTROLLER_LEASE_NAME) and grants it in a Role in the controller's namespace. The
// Lease names the Kind x version, so every controller for it competes for the same one: the next pod
// of a rolling update, or a pool taking the Kind over, waits until this one has drained and released
// it. unstructured-runtime's controller.Ownership does the waiting; this package only maps the
// controller's environment onto it.
package ownership

import (
	"errors"
	"fmt"

	"github.com/krateo-platformops/unstructured-runtime/pkg/controller"
	"github.com/krateo-platformops/unstructured-runtime/pkg/controller/builder"
)

// Option returns the builder option that makes the controller hold leaseName, in the pod's own
// namespace, as the pod.
//
// An empty leaseName returns nil and no error: core-provider before step 2 renders no Lease, and the
// controller then runs exactly as it did. A Lease name WITH an empty pod namespace or name is an
// error, never a silent fall back to no Lease: the guard was asked for, and running without it would
// let a second controller reconcile alongside this one. Both come from the downward API in
// assets/cdc/deployment.yaml (POD_NAMESPACE, POD_NAME), so they are missing only from a Deployment
// that core-provider did not render.
func Option(leaseName, podNamespace, podName string) (builder.FuncOption, error) {
	if leaseName == "" {
		return nil, nil
	}
	if podNamespace == "" || podName == "" {
		return nil, fmt.Errorf("COMPOSITION_CONTROLLER_LEASE_NAME is %q, but POD_NAMESPACE (%q) or POD_NAME (%q) is empty: "+
			"cannot hold the ownership lease, and refusing to reconcile without it", leaseName, podNamespace, podName)
	}
	return builder.WithOwnership(podNamespace, leaseName, podName), nil
}

// Lost reports whether Run ended because the controller lost its Lease. Its workers were stopped at
// once; the process should exit, so the restarted controller waits for the Lease again.
func Lost(err error) bool { return errors.Is(err, controller.ErrOwnershipLost) }

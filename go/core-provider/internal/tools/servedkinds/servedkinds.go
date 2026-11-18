// Package servedkinds is core-provider's startup check that the API server serves the Kinds it
// reconciles (#589).
//
// Those Kinds are CRDs, and the CRDs ship in a SEPARATE chart, core-provider-crds, not in the
// core-provider chart. A core-provider installed without them used to come up and then fail
// inside controller-runtime: source.Kind logs "if kind is a CRD, it should be installed before
// calling Start" on every retry and the manager exits only when the cache sync times out. That
// message names neither the chart nor the version to install. Check runs before the manager
// starts and fails with one error that names both.
//
// The check asks the manager's own RESTMapper, so "not served" means exactly what the manager
// would hit: meta.NoKindMatchError. Any other mapper error (an unreachable or refusing API server)
// is returned as a failure to CHECK, never reported as missing CRDs: a wrong instruction to
// install a chart that is already installed is worse than the plain error.
package servedkinds

import (
	"errors"
	"fmt"
	"strings"

	"github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// CRDsChart is the chart that ships every Kind in Required.
const CRDsChart = "core-provider-crds"

// Required lists every Kind core-provider watches that is a CRD from CRDsChart. A guard test in
// package main requires it to equal the Kinds generated under crds/, so a new CRD cannot be added
// without being checked here.
var Required = []schema.GroupVersionKind{
	v1alpha1.CompositionDefinitionGroupVersionKind,
	v1alpha1.SchemeGroupVersion.WithKind("KubernetesTarget"),
}

// NotServedError says which Required Kinds the API server does not serve, and what to install.
type NotServedError struct {
	Missing []schema.GroupVersionKind
	// Version is this core-provider's version. core-provider-crds is released with it, at the
	// same version.
	Version string
}

func (e *NotServedError) Error() string {
	names := make([]string, 0, len(e.Missing))
	for _, gvk := range e.Missing {
		names = append(names, gvk.Kind+"."+gvk.Group+"/"+gvk.Version)
	}
	return fmt.Sprintf("the API server does not serve %s: these CRDs ship in the %s chart, not in core-provider; install %s at version %s, the same version as this core-provider",
		strings.Join(names, ", "), CRDsChart, CRDsChart, e.Version)
}

// Check returns nil when mapper resolves every Kind in kinds, a *NotServedError naming the ones it
// does not serve, or an error saying the check itself could not run.
func Check(mapper meta.RESTMapper, kinds []schema.GroupVersionKind, version string) error {
	var missing []schema.GroupVersionKind
	for _, gvk := range kinds {
		_, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		switch {
		case err == nil:
		case meta.IsNoMatchError(err):
			missing = append(missing, gvk)
		default:
			return fmt.Errorf("cannot check whether the API server serves %s: %w", gvk, err)
		}
	}
	if len(missing) > 0 {
		return &NotServedError{Missing: missing, Version: version}
	}
	return nil
}

// IsNotServed reports whether err is, or wraps, a *NotServedError.
func IsNotServed(err error) bool {
	var ns *NotServedError
	return errors.As(err, &ns)
}

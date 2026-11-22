package deploy

import (
	"fmt"
	"os"
	"strings"

	"github.com/krateo-platformops/core-provider/internal/tools/objects"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
)

// #588. Every object core-provider creates for a definition takes its name from the generated GVR:
// the CRD version is crdgen.NormalizeVersionName of the chart's Chart.yaml version (#591), and the dedicated
// controller's objects are "<plural>-<crd version>-…". Kubernetes caps those names, and before this
// check a chart whose plural and version were too long together got its CRD written, then failed
// (or silently misbehaved) on the controller's objects, leaving a served Kind nothing reconciles.
//
// spec.chart.version used to be capped at 20 bytes at admission (upstream 6d09d37, 2023, beside an
// immutability rule since removed). That cap measured the wrong field: the names derive from the
// FETCHED chart's Chart.yaml version, and spec.chart.version may be a semver range the repo getter
// resolves. And it bounded only one of the two inputs. The real limits, checked here once both
// inputs are known, before any write:
//
//   - the CRD version is a DNS-1035 label (apiextensions validation). crdgen normalizes the
//     Chart.yaml version into one (crdgen.NormalizeVersionName: lowercase, every run of other
//     characters becomes '-', so 1.0.0-RC1 -> v1-0-0-rc1 and 1.0.0+build -> v1-0-0-build), and
//     core-provider derives the chart's GVK through the same call (#591).
//     So in practice only its LENGTH can fail, at more than 63;
//     the full rule is checked anyway, so a caller that passes an un-normalized version is caught;
//   - a dedicated controller's Deployment is selected by app.kubernetes.io/name = its name, and the
//     shipped template renders that label as `trunc 63 .name`. A longer name is cut silently: two
//     versions of a Kind can share a selector, or the cut ends on '-' and the Deployment is refused.
//     So the untruncated name must be a valid label value, at most 63;
//   - the controller Service (rendered only when the chart mounts its template, i.e.
//     cdc.metrics.enabled) is a DNS-1035 label, at most 63. Its name comes from the template itself,
//     so it is rendered here from the same file the write uses, never reconstructed.
//
// A pool member's controller is its pool's Deployment, so the dedicated-controller names do not
// apply to it; a definition leaving its pool changes its spec, and is checked again.

// NameViolation is one name core-provider would derive that the API server would refuse, or that the
// controller templates would silently truncate.
type NameViolation struct {
	// What the name is, for the reader.
	What string
	// Name is the derived name, untruncated.
	Name string
	// Problems are the validator's own messages.
	Problems []string
}

func (v NameViolation) String() string {
	return fmt.Sprintf("%s %q (%d characters): %s", v.What, v.Name, len(v.Name), strings.Join(v.Problems, "; "))
}

// DerivedNameViolations returns every name derived from gvr that would not survive the write.
// dedicated is whether the definition runs its own controller (not a pool member). serviceTemplatePath
// is the controller Service template; the Service is checked exactly when the write would create it
// (the same os.Stat test the deploy path makes).
func DerivedNameViolations(gvr schema.GroupVersionResource, dedicated bool, serviceTemplatePath string) ([]NameViolation, error) {
	var out []NameViolation
	if errs := validation.IsDNS1035Label(gvr.Version); len(errs) > 0 {
		out = append(out, NameViolation{
			What: "the CRD version (\"v\" + the chart's Chart.yaml version, dots as dashes)", Name: gvr.Version, Problems: errs,
		})
	}
	if !dedicated {
		return out, nil
	}
	nn := getCDCDeploymentNN(types.NamespacedName{Name: resourceNamer(gvr.Resource, gvr.Version)})
	if errs := validation.IsValidLabelValue(nn.Name); len(errs) > 0 {
		out = append(out, NameViolation{
			What: "the controller Deployment's name, which is also its app.kubernetes.io/name selector label", Name: nn.Name, Problems: errs,
		})
	}
	if _, err := os.Stat(serviceTemplatePath); err == nil {
		var svc corev1.Service
		if err := objects.CreateK8sObject(&svc, gvr, nn, serviceTemplatePath); err != nil {
			return nil, fmt.Errorf("rendering the controller Service to check its name: %w", err)
		}
		if errs := validation.IsDNS1035Label(svc.Name); len(errs) > 0 {
			out = append(out, NameViolation{What: "the controller Service's name", Name: svc.Name, Problems: errs})
		}
	}
	return out, nil
}

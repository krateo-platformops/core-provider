package composition

import (
	"errors"
	"fmt"
	"strings"

	"github.com/krateo-platformops/composition-dynamic-controller/internal/conditionreason"
	helmconfig "github.com/krateo-platformops/plumbing/helm"
	"github.com/krateo-platformops/plumbing/managementpolicy"
	"github.com/krateo-platformops/unstructured-runtime/pkg/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// core-provider#597: krateo.io/management-policy, as Observe honours it.
//
// The runtime gates the DISPATCH of Create and Update (unstructured-runtime#110 Phase 0), but this
// controller's Observe runs the helm reconcile itself, so it upgraded a release and healed its drift
// whatever the policy said (measured: TestEnvtest_597_ObserveWritesUnderAWithheldUpdate). Observe now
// reads the same decision the runtime does, once per pass:
//
//   - create withheld, release absent: Ready=False/CreateWithheldByPolicy, nothing installed.
//   - update withheld: the reconcile runs with UpgradeConfig.WriteNothing, so the release is observed
//     and never written. The derived-RBAC refresh still runs, because without it this controller can
//     lose the access it needs to observe and later delete (Diego, 2026-11-21).
//   - what would have been written is reported in Ready: UpdateWithheldByPolicy replaces only the
//     healthy verdict; a failing rollup keeps its reason and its message notes the withheld update
//     (Diego, 2026-11-21).

// policyValue is the composition's krateo.io/management-policy as written, for messages.
func policyValue(mg *unstructured.Unstructured) string {
	return mg.GetAnnotations()[meta.AnnotationKeyManagementPolicy]
}

// policyPhrase names the policy for a message: the annotation and its value, and, for a value that
// does not parse, why it acts on nothing (D2).
func policyPhrase(mg *unstructured.Unstructured, d managementpolicy.Decision) string {
	s := fmt.Sprintf("%s %q", meta.AnnotationKeyManagementPolicy, policyValue(mg))
	if d.Policy.Invalid != "" {
		s += fmt.Sprintf(" (it does not parse, so it acts as observe only: %s)", d.Policy.Invalid)
	}
	return s
}

// createWithheldMessage is CreateWithheldByPolicy's message: which policy withheld the install and how
// to allow it (frontend's request: a deliberate refusal, so the message must say what would change it).
func createWithheldMessage(mg *unstructured.Unstructured, d managementpolicy.Decision) string {
	return fmt.Sprintf("the release is not installed, and %s withholds create, so nothing is installed; "+
		"to allow it, add create to the set (for example \"observe,create\") or remove the annotation",
		policyPhrase(mg, d))
}

// updateWithheldMessage says what an update would have done, from a WriteNothing reconcile, or "" when
// nothing would change (then the composition is simply Available). installed is the stored release's
// chart version, wanted the definition's.
func updateWithheldMessage(mg *unstructured.Unstructured, d managementpolicy.Decision, res *helmconfig.ReconcileResult, installed, wanted string) string {
	if res == nil || !res.WouldChange {
		return ""
	}
	head := fmt.Sprintf("%s withholds update, so the installed chart %s is frozen", policyPhrase(mg, d), installed)
	if res.DesiredStateChange != "" {
		return fmt.Sprintf("%s: the definition wants chart %s, and the change (%s) is not applied", head, wanted, res.DesiredStateChange)
	}
	var parts []string
	for _, p := range []struct {
		verb    string
		names   []string
		omitted int
	}{
		{"create", res.WouldCreateObjects, res.WouldCreateObjectsOmitted},
		{"delete", res.WouldDeleteObjects, res.WouldDeleteObjectsOmitted},
		{"patch", res.WouldPatchObjects, res.WouldPatchObjectsOmitted},
	} {
		if len(p.names) == 0 && p.omitted == 0 {
			continue
		}
		s := fmt.Sprintf("would %s %s", p.verb, strings.Join(p.names, ", "))
		if p.omitted > 0 {
			s += fmt.Sprintf(" and %d more", p.omitted)
		}
		parts = append(parts, s)
	}
	if len(parts) == 0 {
		return head + ": drift is not corrected"
	}
	return fmt.Sprintf("%s: drift is not corrected (%s)", head, strings.Join(parts, "; "))
}

// driftUnassessable reports whether a WriteNothing reconcile error means only that drift could not be
// measured: a webhook denied the dry run, or the stored manifest needs a repair that would be a write.
// Not a reconcile failure: nothing was written and nothing will be (Diego, 2026-11-21: the same
// UpdateWithheldByPolicy reason, with the message saying why).
func driftUnassessable(err error) bool {
	return errors.Is(err, helmconfig.ErrDryRunRefused) || errors.Is(err, helmconfig.ErrStoredManifestRepairWithheld)
}

// unassessableMessage is UpdateWithheldByPolicy's message when drift could not be assessed.
func unassessableMessage(mg *unstructured.Unstructured, d managementpolicy.Decision, installed string, err error) string {
	return fmt.Sprintf("%s withholds update, so the installed chart %s is frozen; drift could not be assessed: %v",
		policyPhrase(mg, d), installed, err)
}

// withUpdateWithheld applies a withheld update to the Ready outcome (Diego, 2026-11-21): it replaces
// only Available, so a failing or not-yet-healthy rollup keeps its reason, and its message says that
// updates are withheld. withheld == "" leaves out unchanged.
func withUpdateWithheld(out readyOutcome, withheld string) readyOutcome {
	if withheld == "" {
		return out
	}
	if out.reason == "Available" {
		return readyOutcome{reason: conditionreason.ReasonUpdateWithheldByPolicy, message: withheld}
	}
	out.message += "; and " + withheld // the reason is the rollup's, unchanged
	return out
}

// installedChartVersion is the stored release's chart version: what is running, which is what
// status.helmChartVersion must say while an update is withheld.
func installedChartVersion(rel *helmconfig.Release) string {
	if rel == nil {
		return ""
	}
	return rel.ChartVersion
}

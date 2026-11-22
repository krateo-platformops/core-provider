package compositiondefinitions

import (
	"context"
	"fmt"
	"strings"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/cdreason"
	"github.com/krateo-platformops/core-provider/internal/controllers/compositiondefinitions/helpers/getters"
	rtv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// core-provider#614: MigratedValuesValid. Diego, 2026-11-22: report it on the CompositionDefinition,
// never block or undo the migration, never touch Ready.
//
// WHEN IT RUNS. Only a migration can put a refused value on an instance: a user write that changes a
// field is validated in full, and a schema change at the same chart version is applied only when
// provably looser (#534). So the check runs
//   - in Update, right after a migration pass that moved at least one instance (migrateAndCheck), and
//   - in Observe, while the condition is present and not True, so it clears once the values are
//     fixed or the refusing instances are gone (recheckMigratedValues). True is final until the next
//     migration: nothing else can reopen it, so a healthy definition pays nothing per pass.
//
// It is ABSENT until a migration has moved an instance: before that there is no migrated value to
// vouch for, and Unknown would read as "something could not be checked".
//
// COST. One dry-run create per instance the definition owns on the served version, after a
// migration, and on each Observe only while some instance is refused or unchecked. Dry runs write
// nothing to etcd.

const (
	maxInstancesInMessage = 10
	maxFieldsPerInstance  = 5
)

// migrateAndCheck is Update's migration step: every instance this definition owns on an older
// served version is moved onto gvk.Version, and if anything moved, the instances on that version are
// validated and MigratedValuesValid is set on cr. A migration error is returned unchanged; the check
// never returns one (its own failures are reported on the condition).
func (e *external) migrateAndCheck(ctx context.Context, cr *compositiondefinitionsv1alpha1.CompositionDefinition,
	gvr schema.GroupVersionResource, gvk schema.GroupVersionKind, log logging.Logger) error {
	owner := getters.DefinitionRef{Name: cr.Name, Namespace: cr.Namespace}
	migrateOpts := e.migrationLossOptions(ctx, gvr, log)
	moved := 0
	for _, vi := range cr.Status.Managed.VersionInfo {
		if vi.Version == gvk.Version {
			continue
		}
		// List + re-stamp THROUGH the current served endpoint (gvr, whose version is
		// gvk.Version): the composition-version policy stamps the request's served version,
		// so writing via the current endpoint makes it agree with the relabel instead of
		// re-stamping the old version. Scoped to THIS definition so a shared CRD's other
		// definitions (legitimately on an older version) are never touched.
		n, err := getters.UpdateCompositionsVersion(ctx, e.dynamic, gvr, vi.Version, gvk.Version, owner, migrateOpts)
		moved += n
		if err != nil {
			if moved > 0 {
				e.checkMigratedValues(ctx, cr, gvr, log)
			}
			return fmt.Errorf("error migrating compositions from version %s: %w", vi.Version, err)
		}
	}
	if moved > 0 {
		e.checkMigratedValues(ctx, cr, gvr, log)
	}
	return nil
}

// recheckMigratedValues is Observe's half: it re-validates only while the condition says something
// is wrong (False or Unknown).
func (e *external) recheckMigratedValues(ctx context.Context, cr *compositiondefinitionsv1alpha1.CompositionDefinition,
	gvr schema.GroupVersionResource, log logging.Logger) {
	c, present := migratedValuesValid(cr)
	if !present || cdreason.MigratedValuesAllValid.Is(c) {
		return
	}
	e.checkMigratedValues(ctx, cr, gvr, log)
}

// migratedValuesValid returns the condition and whether it is present. GetCondition alone cannot
// tell: for an absent type it returns {Type, Status: Unknown}, which would read as "unchecked" and
// make every definition re-validate on every Observe.
func migratedValuesValid(cr *compositiondefinitionsv1alpha1.CompositionDefinition) (rtv1.Condition, bool) {
	for _, c := range cr.Status.Conditions {
		if c.Type == cdreason.TypeMigratedValuesValid {
			return c, true
		}
	}
	return rtv1.Condition{}, false
}

func (e *external) checkMigratedValues(ctx context.Context, cr *compositiondefinitionsv1alpha1.CompositionDefinition,
	gvr schema.GroupVersionResource, log logging.Logger) {
	owner := getters.DefinitionRef{Name: cr.Name, Namespace: cr.Namespace}
	res, err := getters.ValidateOwnedInstances(ctx, e.dynamic, gvr, owner)
	cond := migratedValuesCondition(res, err)
	prev, _ := migratedValuesValid(cr)
	cr.SetConditions(cond)
	// One Event per new finding, not one per Observe while it stands.
	if cdreason.MigratedValuesRefused.Is(cond) && (prev.Reason != cond.Reason || prev.Message != cond.Message) {
		log.Info("Instances hold values the served version refuses (validation ratcheting let the migration through)",
			"version", gvr.Version, "refused", len(res.Refused))
		if e.rec != nil {
			e.rec.Eventf(cr, nil, corev1.EventTypeWarning, "MigratedValuesRefused", "VersionMigration", "%s", cond.Message)
		}
	}
}

// migratedValuesCondition turns one validation into the condition. Refused wins over unchecked: a
// refused instance is a fact, an unchecked one only an absence of evidence, and the message counts
// both. A list failure is Unknown, never True.
func migratedValuesCondition(res getters.ValidationResult, listErr error) rtv1.Condition {
	if listErr != nil {
		return cdreason.MigratedValuesUnchecked.WithMessage(fmt.Sprintf(
			"the instances on version %s could not be listed, so none was checked: %v", res.Version, listErr))
	}
	if len(res.Refused) > 0 {
		msg := fmt.Sprintf("%d of %d instance(s) on version %s hold values that version refuses; a write that changes those fields is refused until they satisfy it: %s",
			len(res.Refused), res.Checked, res.Version, refusedList(res.Refused))
		if len(res.Unchecked) > 0 {
			msg += fmt.Sprintf("; %d more could not be checked", len(res.Unchecked))
		}
		return cdreason.MigratedValuesRefused.WithMessage(msg)
	}
	if len(res.Unchecked) > 0 {
		return cdreason.MigratedValuesUnchecked.WithMessage(fmt.Sprintf(
			"%d of %d instance(s) on version %s could not be checked (the dry run failed for a reason that is not a spec validation error): %s",
			len(res.Unchecked), res.Checked, res.Version, uncheckedList(res.Unchecked)))
	}
	return cdreason.MigratedValuesAllValid.WithMessage(fmt.Sprintf(
		"all %d instance(s) on version %s satisfy its schema", res.Checked, res.Version))
}

func refusedList(rs []getters.RefusedInstance) string {
	shown := rs
	if len(shown) > maxInstancesInMessage {
		shown = shown[:maxInstancesInMessage]
	}
	parts := make([]string, 0, len(shown))
	for _, r := range shown {
		fields := r.Fields
		more := ""
		if len(fields) > maxFieldsPerInstance {
			fields = fields[:maxFieldsPerInstance]
			more = ", ..."
		}
		parts = append(parts, fmt.Sprintf("%s/%s (%s%s)", r.Namespace, r.Name, strings.Join(fields, ", "), more))
	}
	out := strings.Join(parts, ", ")
	if len(rs) > len(shown) {
		out += ", ..."
	}
	return out
}

func uncheckedList(us []getters.UncheckedInstance) string {
	shown := us
	if len(shown) > maxInstancesInMessage {
		shown = shown[:maxInstancesInMessage]
	}
	parts := make([]string, 0, len(shown))
	for _, u := range shown {
		parts = append(parts, u.Namespace+"/"+u.Name)
	}
	out := strings.Join(parts, ", ")
	if len(us) > len(shown) {
		out += ", ..."
	}
	return out
}

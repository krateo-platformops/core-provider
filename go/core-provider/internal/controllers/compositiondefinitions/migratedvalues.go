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
//
// STRUCTURED FORM (core-provider#623): status.migratedValues carries the same finding as data. One
// builder, migratedValuesReport, produces both: it caps the check result once (the API package's
// MigratedValuesMaxInstances and MigratedValuesMaxFields, which the CRD's maxItems also enforce) and
// renders the message FROM those capped entries, so the message and the field cannot name different
// instances, fields or order.

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
	cond, values := migratedValuesReport(res, err)
	prev, _ := migratedValuesValid(cr)
	cr.SetConditions(cond)
	cr.Status.MigratedValues = values
	// One Event per new finding, not one per Observe while it stands.
	if cdreason.MigratedValuesRefused.Is(cond) && (prev.Reason != cond.Reason || prev.Message != cond.Message) {
		log.Info("Instances hold values the served version refuses (validation ratcheting let the migration through)",
			"version", gvr.Version, "refused", len(res.Refused))
		if e.rec != nil {
			e.rec.Eventf(cr, nil, corev1.EventTypeWarning, "MigratedValuesRefused", "VersionMigration", "%s", cond.Message)
		}
	}
}

// migratedValuesReport turns one validation into the condition and its structured form. Refused wins
// over unchecked: a refused instance is a fact, an unchecked one only an absence of evidence, and the
// message counts both. A list failure is Unknown, never True, and has no structured form: with no
// list there are no instances to name, and counts of zero would read as an all-clear. True clears the
// structured form.
func migratedValuesReport(res getters.ValidationResult, listErr error) (rtv1.Condition, *compositiondefinitionsv1alpha1.MigratedValues) {
	if listErr != nil {
		return cdreason.MigratedValuesUnchecked.WithMessage(fmt.Sprintf(
			"the instances on version %s could not be listed, so none was checked: %v", res.Version, listErr)), nil
	}
	if len(res.Refused) == 0 && len(res.Unchecked) == 0 {
		return cdreason.MigratedValuesAllValid.WithMessage(fmt.Sprintf(
			"all %d instance(s) on version %s satisfy its schema", res.Checked, res.Version)), nil
	}
	values := migratedValues(res)
	if values.RefusedCount > 0 {
		msg := fmt.Sprintf("%d of %d instance(s) on version %s hold values that version refuses; a write that changes those fields is refused until they satisfy it: %s",
			values.RefusedCount, res.Checked, res.Version, refusedList(values))
		if values.UncheckedCount > 0 {
			msg += fmt.Sprintf("; %d more could not be checked", values.UncheckedCount)
		}
		return cdreason.MigratedValuesRefused.WithMessage(msg), values
	}
	return cdreason.MigratedValuesUnchecked.WithMessage(fmt.Sprintf(
		"%d of %d instance(s) on version %s could not be checked (the dry run failed for a reason that is not a spec validation error): %s",
		values.UncheckedCount, res.Checked, res.Version, uncheckedList(values))), values
}

// migratedValues caps a check result into status.migratedValues: the first instances in check order,
// the first fields of each, and the uncapped counts beside them. It is the only place the caps apply.
func migratedValues(res getters.ValidationResult) *compositiondefinitionsv1alpha1.MigratedValues {
	v := &compositiondefinitionsv1alpha1.MigratedValues{
		Version:        res.Version,
		RefusedCount:   len(res.Refused),
		UncheckedCount: len(res.Unchecked),
	}
	for i, r := range res.Refused {
		if i == compositiondefinitionsv1alpha1.MigratedValuesMaxInstances {
			break
		}
		fields := r.Fields
		if len(fields) > compositiondefinitionsv1alpha1.MigratedValuesMaxFields {
			fields = fields[:compositiondefinitionsv1alpha1.MigratedValuesMaxFields]
		}
		v.Refused = append(v.Refused, compositiondefinitionsv1alpha1.MigratedValuesRefusedInstance{
			Namespace: r.Namespace, Name: r.Name, Fields: append([]string(nil), fields...), FieldCount: len(r.Fields)})
	}
	for i, u := range res.Unchecked {
		if i == compositiondefinitionsv1alpha1.MigratedValuesMaxInstances {
			break
		}
		v.Unchecked = append(v.Unchecked, compositiondefinitionsv1alpha1.MigratedValuesInstance{Namespace: u.Namespace, Name: u.Name})
	}
	return v
}

// refusedList renders the capped refused entries for the message; ", ..." marks a cap that hid
// something (fields of one instance, or instances).
func refusedList(v *compositiondefinitionsv1alpha1.MigratedValues) string {
	parts := make([]string, 0, len(v.Refused))
	for _, r := range v.Refused {
		more := ""
		if r.FieldCount > len(r.Fields) {
			more = ", ..."
		}
		parts = append(parts, fmt.Sprintf("%s/%s (%s%s)", r.Namespace, r.Name, strings.Join(r.Fields, ", "), more))
	}
	out := strings.Join(parts, ", ")
	if v.RefusedCount > len(v.Refused) {
		out += ", ..."
	}
	return out
}

// uncheckedList renders the capped unchecked entries for the message, without their error text so the
// message stays stable across passes.
func uncheckedList(v *compositiondefinitionsv1alpha1.MigratedValues) string {
	parts := make([]string, 0, len(v.Unchecked))
	for _, u := range v.Unchecked {
		parts = append(parts, u.Namespace+"/"+u.Name)
	}
	out := strings.Join(parts, ", ")
	if v.UncheckedCount > len(v.Unchecked) {
		out += ", ..."
	}
	return out
}

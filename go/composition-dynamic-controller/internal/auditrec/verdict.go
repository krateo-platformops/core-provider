// Package auditrec turns the cdc's helm writes into controller audit records (core-provider#423,
// AUDIT-4): one record per child a helm operation created, changed, deleted, kept or failed on,
// from plumbing's per-object verdict (helm.ObjectVerdict), never from a re-derivation in the cdc.
//
// The design is docs/design/audit-controller-records.md. The schema is krateo-platformops/audit
// v0.6.0: writer "controller", object_ref = the child written, cause_ref = the composition
// reconciled, on every record of the reconcile.
package auditrec

import (
	"github.com/krateo-platformops/audit"
	helmconfig "github.com/krateo-platformops/plumbing/helm"
)

// Scope is the logger's instrumentation scope, the population the audit schema registers for the
// composition-dynamic-controller (audit v0.6.0 schemaPopulations): "<module path>/audit".
const Scope = "github.com/krateo-platformops/composition-dynamic-controller/audit"

// The krateo.action literals, one per helm operation the cdc runs. A literal is the emit site's own
// (audit docs/schema.md: "the emit site's logical action ... Not the verb"); the schema's examples
// use helm.upgrade and helm.uninstall, and these follow that form.
const (
	ActionInstall   = "helm.install"
	ActionUpgrade   = "helm.upgrade"
	ActionReconcile = "helm.reconcile"
	ActionUninstall = "helm.uninstall"
	// ActionRollback is a rollback's writes: Observe's clearing of a release stuck pending past the
	// grace, and Delete's. A rollback is not an upgrade, so it has its own literal (snowplow,
	// 2026-11-12).
	ActionRollback = "helm.rollback"
)

// Excluded-write reasons: a verdict the installer#413 predicate drops is COUNTED by reason, never
// recorded by identity.
const (
	ExcludedMetadataOnly = "metadata_only"
)

// Event is the audit record for one child verdict, and whether it is recorded at all. A verdict the
// predicate excludes returns ok=false with its excluded-counter reason.
//
// The mapping (docs/design/audit-controller-records.md, decided 2026-11-11):
//
//	created        create, POST,   success
//	deleted        delete, DELETE, success
//	specChanged    v.Verb (patch|update), PATCH|PUT, success
//	compareFailed  as specChanged: a write whose effect is unknown is recorded (Q1)
//	metadataOnly   excluded, counted metadata_only (".spec changes" test)
//	kept           delete, no request, denied, kept_resource_policy
//	deleteNotDone  delete, DELETE, failure
//	deleteUnknown  delete, DELETE, failure: nobody can say the delete happened
//	createFailed   create, POST,   failure
//	updateFailed   v.Verb, PATCH|PUT, failure
//
// The record carries no Message: the body is a closed set, and every fact is a key (the schema's
// own kept-deletion example sets none). No upstream error text ever reaches it.
func Event(action string, v helmconfig.ObjectVerdict, cause audit.CauseRef, preview audit.Preview) (ev audit.Event, ok bool, excluded string) {
	ev = audit.Event{
		Action:     action,
		Namespace:  v.Namespace,
		Group:      v.Group,
		Version:    v.Version,
		Resource:   v.Resource,
		Kind:       v.Kind,
		ObjectName: v.Name,
		ObjectUID:  v.UID,
		Preview:    preview,
		Cause:      cause,
	}
	switch v.Change {
	case helmconfig.ChangeMetadataOnly:
		return audit.Event{}, false, ExcludedMetadataOnly
	case helmconfig.ChangeKept:
		// No request was sent: no method, no status. The reason is the key.
		ev.Verb, ev.Outcome, ev.DenialReason = "delete", audit.OutcomeDenied, audit.DenialReasonKeptResourcePolicy
		return ev, true, ""
	case helmconfig.ChangeCreated:
		ev.Verb, ev.Outcome = "create", audit.OutcomeSuccess
	case helmconfig.ChangeCreateFailed:
		ev.Verb, ev.Outcome = "create", audit.OutcomeFailure
	case helmconfig.ChangeDeleted:
		ev.Verb, ev.Outcome = "delete", audit.OutcomeSuccess
	case helmconfig.ChangeDeleteNotDone, helmconfig.ChangeDeleteUnknown:
		ev.Verb, ev.Outcome = "delete", audit.OutcomeFailure
	case helmconfig.ChangeSpec, helmconfig.ChangeCompareFailed:
		ev.Verb, ev.Outcome = writeVerb(v.Verb), audit.OutcomeSuccess
	case helmconfig.ChangeUpdateFailed:
		ev.Verb, ev.Outcome = writeVerb(v.Verb), audit.OutcomeFailure
	default:
		// A change kind this mapping does not know is recorded, not dropped: an unrecorded write is
		// the failure AUDIT-4 exists to prevent. Its verb is the one helm sent.
		ev.Verb, ev.Outcome = writeVerb(v.Verb), audit.OutcomeSuccess
	}
	ev.HTTPMethod = httpMethod(ev.Verb)
	return ev, true, ""
}

// writeVerb is the verb helm sent for a write to an existing child: "patch", or "update" when it
// replaced the object (Force). An empty verb is a patch, helm's default.
func writeVerb(v string) string {
	if v == "update" {
		return "update"
	}
	return "patch"
}

// httpMethod is the HTTP method a Kubernetes write verb is sent with.
func httpMethod(verb string) string {
	switch verb {
	case "create":
		return "POST"
	case "update":
		return "PUT"
	case "patch":
		return "PATCH"
	case "delete":
		return "DELETE"
	}
	return ""
}

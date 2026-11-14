---
type: Decision
title: "Controller audit records from the cdc and core-provider (AUDIT-4)"
description: What the composition-dynamic-controller and core-provider record as controller audit records - populations, actors, record and exclusion rules, action literals, counters and coverage - as shipped in 2.13.86-2.13.94.
resource: ghcr.io/krateo-platformops/composition-dynamic-controller
tags: [design, audit, cdc, core-provider]
status: implemented
timestamp: 2026-11-13T00:00:00Z
---

# Controller audit records from the cdc and core-provider (core-provider#423, AUDIT-4)

Status: **design of record**, measured against core-provider `main` at 22fbe70 (release 2.13.94).
Decisions Q1–Q3 were taken by Diego on 2026-11-11; the rest of this document describes what shipped
(cdc in 2.13.86 via #459; core-provider's own writes in 2.13.87 via #460/#464; authorization metadata
as content in 2.13.87 via #465/#467). Pins: cdc on audit **v0.13.0** and plumbing **v1.67.0**;
core-provider on audit **v0.13.0** and plumbing **v1.60.0**. The action literals and coverage rows are
registered in audit **v0.14.0** (snowplow, audit#36).

## Populations

| Scope | Writer | Who |
|---|---|---|
| `github.com/krateo-platformops/composition-dynamic-controller/audit` | controller | the cdc's helm writes to a Composition's children |
| `github.com/krateo-platformops/core-provider/audit` | controller | core-provider's own writes: RBAC, credential Secrets, authn mappings, the spoke mirror, hub deletes, migration rewrites |

Populations are split by scope, never by ServiceName: the cdc's ServiceName is per version.

**Actor.** Both writers use audit's own helpers, so the rule lives in one place:
- the cdc: `audit.ServiceAccountActor(namespace, serviceAccount)`, from its `-saNamespace` / `-saName`
  flags (env `COMPOSITION_CONTROLLER_SA_NAMESPACE` / `_SA_NAME`; `internal/auditrec/setup.go`);
- core-provider: `audit.ControllerActor()`, reading `POD_NAMESPACE` / `POD_SERVICE_ACCOUNT`, which the
  core-provider Deployment sets from the downward API (`tools/auditrec/setup.go`).

When the identity is unset, no actor is given and every record is marked actor-incomplete, never
guessed; the cdc also logs it at startup.

## Fields every record carries (controller column)

- required: `user.name` (the actor), `krateo.action`, `krateo.audit.verb` (the verb SENT),
  `krateo.audit.outcome`;
- `krateo.audit.object_ref.*` = the object written. `uid` only when the object is known, so a create
  derived without a read omits it rather than guessing;
- `krateo.audit.cause_ref.*` = what was reconciled: the Composition for the cdc, the
  CompositionDefinition for core-provider (`auditrec.CauseOfDefinition`), on every record of the
  reconcile;
- `krateo.audit.denial_reason`: only `kept_resource_policy`, and only on a denied record;
- `krateo.audit.preview`: read from the target object's Namespace on the cluster written to, once per
  Namespace per operation; unreadable (deleted included) is Unknown and marked, never false.

The body is a closed set chosen by the emit site, never `err.Error()`, helm's joined error, or any
field value.

## The cdc: one record per child, from plumbing's per-object verdict

All **seven** helm write sites report a per-object verdict (plumbing `ObjectVerdict`, uncapped):
Observe's Reconcile and rollback, Create's Install and Upgrade, Update's Reconcile, and Delete's
rollback and Uninstall. Action literals (`internal/auditrec`): `helm.install`, `helm.upgrade`,
`helm.reconcile`, `helm.uninstall`, `helm.rollback`.

| `ObjectVerdict.Change` | Record? | verb | outcome | excluded-counter reason |
|---|---|---|---|---|
| `created` | yes | `create` | success | |
| `deleted` | yes | `delete` | success | |
| `specChanged` | yes | `Verb` (`patch`, or `update` under Force) | success | |
| `metadataOnly` | **no** | | | `metadata_only` |
| `compareFailed` | yes (Q1) | `Verb` | success | |
| `kept` | yes | `delete` | **denied**, `kept_resource_policy`; no body | |
| `deleteNotDone` | yes | `delete` | failure | |
| `createFailed` | yes | `create` | failure | |
| `updateFailed` | yes | `Verb` | failure | |
| `deleteUnknown` | yes | `delete` | failure (nobody can say it happened) | |
| (absent: unchanged child) | no | | | `unchanged` |
| hook object | no | | | `hook` |

**What counts as content.** A write that changes only metadata is `metadataOnly`, with one exception:
the audit schema's authorization annotations and labels (`audit.AuthorizationAnnotations()`,
`audit.AuthorizationLabels()`) are content, because changing one changes what an identity may do. The
cdc declares both on every helm upgrade, reconcile and rollback, through single constructors
(`upgradeConfig`, `rollbackConfig`) so no site can omit them (#465, plumbing v1.63.0/v1.64.0).

**Not attempted.** `ObjectsNotAttempted > 0` (helm stopped at a failed create) gives no record: a child
helm never sent a request for is not a write, and the failed create that stopped helm is the record.
The count goes to `composition_children_not_attempted_total{action}`.

**Counters** (scope above): `composition_audit_excluded_total{action,reason}` with reason
`metadata_only` | `unchanged` | `hook`; `composition_children_not_attempted_total{action}`;
`composition_audit_incomplete_operations_total{action}` (an operation whose verdicts may not name every
child). `unchanged` is counted only when the release the operation left is known.

## core-provider: one record per write it sent

Action literals (`internal/tools/auditrec`):

| Action | Sites |
|---|---|
| `rbac.apply` / `rbac.delete` | deploy's ClusterRole / ClusterRoleBinding / Role / RoleBinding / ServiceAccount install and teardown; the RESTAction RBAC; chart-inspector's spoke ServiceAccount and RBAC; the binding re-subject on a version bump; the dangling-RBAC sweep; the spoke chart-inspector cleanup |
| `secret.apply` | the CompositionDefinition's referenced Secrets projected onto a spoke |
| `authn.mapping.apply` | the per-definition and core-provider's own `serviceaccount.authn.krateo.io` mapping |
| `composition.mirror` | the spoke mirror's create, update and garbage-collect delete |
| `composition.hub.delete` | hub composition deletes on CompositionDefinition teardown |
| `composition.migrate` | the migration's rewrite of each composition through the target version |

There is no `secret.delete`: no site deletes a credential Secret.

**Record rules,** from plumbing objectclient's write report (`ApplyResult` / `UninstallResult`) and
`content.DiffersWith` (the same comparison as the cdc's verdicts, with the authorization annotations
and labels as content):

| What happened | What is recorded |
|---|---|
| created | a record |
| updated, content changed | a record |
| updated, metadata only | counted (`metadata_only`), no record |
| unchanged: an Update the apiserver persisted nothing for (resourceVersion unmoved) | counted (`unchanged`), no record |
| refused | a failure record with the apiserver's status code |
| deleted | a record |
| absent or NotFound: someone else's delete | nothing |
| nothing sent | nothing |
| dry run (any `DryRun` request, by op) | nothing |

Counter: `core_provider_audit_excluded_total{action,reason}`. **The migration exception:** its read
goes through the narrower target version, which already prunes the fields it discards, so the
fieldloss report (not the objects it holds) decides that content changed, and a pruning rewrite is
always recorded. The recorder and cause are put on the context (`auditrec.WithScope`) at the top of the
CompositionDefinition controller's Observe, Create, Update and Delete, and of the mirror's Reconcile.

## Coverage (audit v0.14.0's rule)

audit v0.14.0 counts a row as **audited** only when every outcome it lists is checked by
`audittest.AssertConformant` on real records. Proven today: the cdc's install, heal and uninstall
successes (`composition_test.go` via `audit_e2e_test.go`); core-provider's deploy RBAC
create/unchanged/update/delete on a real apiserver (`audit_envtest_test.go`); and that row's
**failure** outcome (`audit_refusal_envtest_test.go`: an RBAC-authorizing apiserver refuses a create
and an update by escalation prevention and a delete for a missing verb, each recorded failure / 403,
the create with no uid, the update and delete with the live object's); and the cdc's **prune**
(the heal step requires the FocusConfig the render drops to be one helm.reconcile / delete / success
record with its uid, after a precondition that it existed); and the cdc uninstall row's **failure**
(a ValidatingAdmissionPolicy refuses one child's DELETE: one helm.uninstall / delete / failure record
naming the live child, which stays live; the retry after the policy is lifted goes through, #497);
and a **kept deletion** (a render that drops a child marked helm.sh/resource-policy: keep is one
helm.reconcile / delete / denied / kept_resource_policy record, and the child stays live; plumbing
v1.74.0, which stopped the reconcile recording that one decision twice); and an
**authorization-label-only** child change (`audit_authlabel_e2e_test.go`: a chart served from the
test renders a ConfigMap whose labels come from values; changing only `authn.krateo.io/principal`
is one recorded write, and changing only an ordinary label lands and records nothing). Checked
against audit v0.15.0. Every outcome #423 listed is now proven on a real path.

## Decisions (Diego, 2026-11-11)

- **Q1 `compareFailed`: record it.** helm applied the child but the before/after comparison failed.
  An audit gap is worse than an extra row, and Reconcile already treats it as a change.
- **Q2 the silent sites: plumbing reports the verdict first.** Install, Upgrade and Uninstall (and,
  found during implementation, Rollback) report `OperationResult` verdicts (plumbing v1.53.0), so #423
  records every site uniformly rather than listing a gap.
- **Q3 install-through-Upgrade inside Reconcile: moot.** helm's Upgrade action never installs, so that
  path failed on every chart; plumbing v1.53.0 makes a Reconcile with no stored release a real Install.

## History

- 2026-11-11: draft for decision (Q1–Q3).
- 2026-11-12: cdc implementation (#459, 2.13.86): seven sites, counters, preview, audit v0.9.0.
- 2026-11-12/13: core-provider's own writes (#460 mirror resync prerequisite, #464), audit v0.12.0's
  actor helper; authorization metadata as content (#465/#467, plumbing v1.63.0/v1.64.0, audit v0.13.0),
  2.13.87.
- 2026-11-13: rewritten as the design of record against main 22fbe70 (2.13.94); coverage per audit
  v0.14.0.
- 2026-11-14: audit v0.15.0 in both modules; core-provider's RBAC-set failure outcome proven on a real
  apiserver.
- 2026-11-14: the cdc's prune record required (#495).
- 2026-11-14: the cdc uninstall row's failure outcome proven on kind, with #497's fix (#499).
- 2026-11-14: the kept deletion proven on kind; one decision, one record (plumbing v1.74.0, #172).
- 2026-11-14: the authorization-label-only change proven on kind, with a plain-label control.

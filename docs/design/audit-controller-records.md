# Controller audit records from the cdc and core-provider (core-provider#423, AUDIT-4)

Status: Q1 and Q2 DECIDED by Diego, 2026-11-11 (see Open questions); Q3 follows Q2. Updated
2026-11-12 after implementation: see "Changes since the decision" at the end. Measured against core-provider `15bace0` (cdc on plumbing v1.45.0+),
plumbing v1.48.0, audit v0.5.0 (`schema/v1.yaml`). The target is audit **v0.6.0** (snowplow,
tagged 2026-11-11 evening), which registers the two populations below. `content_sha256` is
optional and waits on plumbing#124.

## Populations (registered in audit v0.6.0)

| Scope | Writer | Who |
|---|---|---|
| `github.com/krateo-platformops/composition-dynamic-controller/audit` | controller | the cdc's helm writes to a Composition's children |
| `github.com/krateo-platformops/core-provider/audit` | controller | core-provider's `*.krateo.io` spec writes, and its RBAC and credential-Secret writes |

The emitter is `audit.New(logger, audit.WithWriter(audit.WriterController), audit.WithActor("system:serviceaccount:<ns>:<name>", audit.ClassController))`.
The cdc's ServiceAccount reaches the process through the downward API. The cdc's ServiceName is
per-version, so populations are split by scope, never by ServiceName.

## Fields every record carries (v0.5.0, controller column)

- required: `user.name` (the actor above), `krateo.action`, `krateo.audit.verb` (the verb SENT),
  `krateo.audit.outcome`;
- `krateo.audit.object_ref.*` = the CHILD written. `uid` is required only "when the object is
  known", so a create derived without a read omits it rather than guessing;
- `krateo.audit.cause_ref.*` = the Composition reconciled, on EVERY record of the reconcile,
  including when the object written is the Composition itself (same uid, the reconciled
  generation). Confirmed by snowplow, 2026-11-11;
- `krateo.audit.denial_reason`: only `kept_resource_policy`, and only on a denied record;
- `dry_run` is not applicable to a controller. The cdc's dry-run renders write nothing and emit
  nothing.

## The cdc: one record per child, from plumbing's per-object verdict

`helm.ReconcileResult.Objects` (plumbing#106, in v1.42.0) is uncapped, one `ObjectVerdict` per child
the merge created, deleted, changed, kept or failed on. The installer#413 predicate, with the
2026-11-09 rulings ("creates and deletes pass the .spec test"; condition 3 on the rendered desired
state), maps each verdict like this:

| `ObjectVerdict.Change` | Record? | verb | outcome | excluded-counter reason |
|---|---|---|---|---|
| `created` | yes | `create` | success | |
| `deleted` | yes | `delete` | success | |
| `specChanged` | yes | `Verb` (`patch`, or `update` under Force) | success | |
| `metadataOnly` | **no** | | | `metadata_only` |
| `compareFailed` | yes (Q1: decided) | `Verb` | success | |
| `kept` | yes | `delete` | **denied**, `kept_resource_policy`; no body (the reason is the key, audit schema.md "Kept deletions") | |
| `deleteNotDone` | yes | `delete` | failure | |
| `createFailed` | yes | `create` | failure | |
| `updateFailed` | yes | `Verb` | failure | |
| `deleteUnknown` | yes | `delete` | failure (nobody can say it happened) | |
| (absent: unchanged child) | no | | | `unchanged` |

`ObjectsNotAttempted > 0` (helm stopped at a failed create) gives **no record** (snowplow,
2026-11-12): a child helm never sent a request for is not a write, and the failed create that
stopped helm is the record. The count goes to `composition_children_not_attempted_total{action}`,
operational state rather than an audit gap. (This replaces "one reconcile-level failure record with
the count in a closed-set field": audit v0.9.0 has no such key, and snowplow declined to add one.)

The body is a closed set chosen by the emit site, never `err.Error()`, helm's joined error, or any
field value. `ObjectVerdict` already carries names and the resource only.

## The gap: three of the cdc's five helm write sites report no per-object outcome

| Site (`internal/composition/composition.go` at 15bace0) | call | per-object verdict |
|---|---|---|
| Observe `:598` | `Reconcile` | **yes** (`Objects`), except on the install-through-Upgrade path (`ObjectsComplete=false`) |
| Update `:1187` | `Reconcile` | **yes** |
| Create `:972` | `Upgrade` | **no**: returns `*Release` (the manifest) |
| Create `:989` | `Install` | **no**: returns `*Release` |
| Delete `:1484` | `Uninstall` | **no**: returns `error` only |

Install, Upgrade and Uninstall are exactly the writes AUDIT-4 is about: a Composition's children
being created, rewritten or removed. Without a verdict the cdc cannot know:

- for **Uninstall**, which children helm deleted and which it KEPT under
  `helm.sh/resource-policy: keep`. A kept one must be a denied record, and it is indistinguishable
  from a deleted one without the verdict;
- for **Upgrade**, which children it created, patched or pruned: a full release diff, unknown;
- for **Install**, on a FAILED install, which children were created before the failure.

**Not a workaround:** re-reading the cluster around each call, or diffing manifests in the cdc,
would reconstruct in the caller what helm decided, with its own matching rules. That is the
reconstruction plumbing#106 was built to avoid for Reconcile.

**Proposal:** plumbing gains the same verdict for the other three calls, under one type. It's a
plumbing change (owner: me), sized like #106. See question Q2.

## core-provider sites (from the issue, paths to re-verify at implementation time)

- the authn mapping `serviceaccount.authn.krateo.io` (create, update);
- spoke mirror Composition create / update / delete, and the hub delete;
- migration spec loss;
- RBAC objects and credential Secrets: named exception to installer#413 condition 1, with
  condition 2 = "rules / subjects / data changed";
- **first, `compositionmirror.go`'s update-on-every-resync must be fixed** so that "differs from
  the previous reconcile" means something (the issue's prerequisite).

## Tests (the issue's acceptance, per emit site)

`audittest.NewRecorder` + `AssertConformant` for every shape (success, failure, denied);
`AssertRequiredKeysDetected` (the falsifier); a body arm with a sentinel identity and a sentinel
token-shaped string fed into every input the site sees; the recorded marker while a span is active
(the cdc's reconcile root, unstructured-runtime v1.13+); an unchanged-spec arm (0 rows, counter +1);
a kept-deletion arm.

## Open questions

- **Q1 `compareFailed`: DECIDED, record it.** helm applied the child, but the before/after comparison failed. Record it
  (a write that may have been a spec change; fail safe toward a row), or exclude it and count
  `compare_failed`? Recommendation: **record**. An audit gap is worse than an extra row, and
  Reconcile already treats it as a change.
- **Q2 the three silent sites: DECIDED, (a).** plumbing reports the verdict from Install,
  Upgrade and Uninstall first, then #423 records all five sites uniformly. (a) plumbing reports `[]ObjectVerdict` from Install, Upgrade and
  Uninstall before #423 emits there (recommended). (b) #423 emits from Reconcile only, and the
  three sites stay unrecorded and are listed in coverage.md as a known gap until (a).
- **Q3 install-through-Upgrade inside Reconcile** (`ObjectsComplete=false`, `Objects` empty). With
  (a), plumbing fills it the same way. With (b), it is a known gap too.

## Changes since the decision (2026-11-12, implementation)

- **Seven write sites, not five.** Rollback writes children too, at Observe (clearing a release
  stuck pending past the grace) and Delete (clearing a stuck uninstall). Its action literal is
  `helm.rollback` (snowplow, 2026-11-12). Rollback reported no verdict; plumbing#139/#140 (v1.53.0)
  makes it return an `OperationResult`.
- **Reconcile's two Upgrade paths.** The real Upgrade after a detected change kept only its release;
  v1.53.0 folds its verdicts into `ReconcileResult` (plus a new `HooksExcluded`). The
  no-stored-release path ("install-through-Upgrade", Q3) **never installed**: helm's Upgrade action
  does not install, so it failed on every chart. v1.53.0 makes it a real Install, so Q3 is moot.
- **Counters** (scope `.../composition-dynamic-controller/audit`):
  `composition_audit_excluded_total{action,reason}` with reason `metadata_only` | `unchanged` |
  `hook`; `composition_children_not_attempted_total{action}`;
  `composition_audit_incomplete_operations_total{action}` (an operation whose verdicts may be
  short). `unchanged` is counted only when the release the operation left is known.
- **Preview** is read from the child's Namespace on the cluster helm writes to, once per Namespace
  per operation; unreadable (deleted included) is Unknown and marked, never false.
- **Schema** is audit v0.9.0.

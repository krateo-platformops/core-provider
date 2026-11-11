# Controller audit records from the cdc and core-provider (core-provider#423, AUDIT-4)

Status: DRAFT for decision. Measured against core-provider `15bace0` (cdc on plumbing v1.45.0+),
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
| `compareFailed` | **open question Q1** | `Verb` | success | (`compare_failed` if excluded) |
| `kept` | yes | `delete` | **denied**, `kept_resource_policy`; body `kept: resource-policy` | |
| `deleteNotDone` | yes | `delete` | failure | |
| `createFailed` | yes | `create` | failure | |
| `updateFailed` | yes | `Verb` | failure | |
| `deleteUnknown` | yes | `delete` | failure (nobody can say it happened) | |
| (absent: unchanged child) | no | | | `unchanged` |

`ObjectsNotAttempted > 0` (helm stopped at a failed create) gives ONE reconcile-level failure record
that states the count in a closed-set field. It is not one record per unattempted child, since no
request was sent for them.

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

- **Q1 `compareFailed`.** helm applied the child, but the before/after comparison failed. Record it
  (a write that may have been a spec change; fail safe toward a row), or exclude it and count
  `compare_failed`? Recommendation: **record**. An audit gap is worse than an extra row, and
  Reconcile already treats it as a change.
- **Q2 the three silent sites.** (a) plumbing reports `[]ObjectVerdict` from Install, Upgrade and
  Uninstall before #423 emits there (recommended). (b) #423 emits from Reconcile only, and the
  three sites stay unrecorded and are listed in coverage.md as a known gap until (a).
- **Q3 install-through-Upgrade inside Reconcile** (`ObjectsComplete=false`, `Objects` empty). With
  (a), plumbing fills it the same way. With (b), it is a known gap too.

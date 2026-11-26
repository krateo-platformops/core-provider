---
type: API
title: core-provider — API
description: The contract — the CompositionDefinition and KubernetesTarget CRDs, the generated composition CRDs, the chart-inspector HTTP API, and the metrics endpoint.
resource: compositiondefinitions.core.krateo.io
tags: [crd, compositiondefinition, kubernetestarget, chart-inspector]
timestamp: 2026-08-07T00:00:00Z
---

# API

Source of truth:
[`go/core-provider/apis/compositiondefinitions/v1alpha1/types.go`](../go/core-provider/apis/compositiondefinitions/v1alpha1/types.go);
generated manifests in [`go/core-provider/crds/`](../go/core-provider/crds)
(drift-gated in PR CI). Both CRDs are **namespaced**, group `core.krateo.io`,
version `v1alpha1`.

## `CompositionDefinition`

The input: "serve this chart as a Kubernetes API".

### `spec`

| Field | Type | Meaning |
|---|---|---|
| `chart.url` | string (required) | OCI ref, repo URL or direct tgz URL. |
| `chart.version` | string (max 20 chars) | Chart version; becomes the CRD version `v<dots-as-dashes>`. |
| `chart.repo` | string (max 20 chars) | Repo-style chart name when `url` is a classic Helm repo. |
| `chart.credentials` | `{username, passwordRef{namespace,name,key}}` | Private-repo credentials; the referenced Secret is watched — rotation re-reconciles. |
| `chart.insecureSkipVerifyTLS` | bool | Skip TLS verification on fetch. |
| `deploy.targetRef.name` | string | A `KubernetesTarget` in this object's OWN namespace; set = deploy the generated CRD + CDC + RBAC onto that spoke. Omitted = local. |
| `apiRef.{name,namespace}` | strings | A snowplow `RESTAction` resolved each reconcile (under the CDC's own authn identity); result exposed to the projection as `.api`. |
| `apiRef.extras` | free-form JSON | Static values merged into the RESTAction's jq root; per-instance context (`compositionId`, …) merges over them, request-wins. |
| `statusDataTemplate[]` | `{forPath, expression, type?, schema?, preserveUnknownFields?}` | Projected status fields: `${ jq }` over `self`/`spec`/`status`/`helm`/`api`, written under `.status` at `forPath`. `type` XOR `schema` XOR `preserveUnknownFields`; validated at reconcile (reserved baseline keys rejected). |
| `upgradePolicy` | `Automatic` \| `Manual` \| `Paused` | Instance migration on a chart-version bump. `Automatic` (default) migrates eagerly; `Manual` waits for the `krateo.io/upgrade-to-version: <version>` annotation; `Paused` freezes migration. |
| `controller.{workers,resyncInterval}` | int / duration | Per-Kind CDC tuning; overrides the chart-global `cdc.workers`/`cdc.resyncInterval` without restarting other controllers. |
| `controller.scaleToZero` | bool | Run this Kind's controller at 0 replicas while it has no instances, and wake it on the first one (#393). Off when unset. **Local definitions only:** set together with a remote `deploy.targetRef` it is rejected at admission (remote is #442). A controller **shared** by several definitions of the same Kind and version scales to zero only when every one of them opts in. Costs one metadata-only watch on the Kind's instances. |
| `controller.scaleToZeroHoldDown` | duration (default `10m`) | How long the Kind must have no instances before its controller goes to 0, counted from the first time core-provider sees it empty, so a delete followed by a re-create doesn't bounce it. |
| `controller.pool` | string (DNS-1123 label) | The shared controller pool this Kind's controller runs in (`cdc-pool-<pool>`), instead of a Deployment of its own. Local definitions only, and not with `scaleToZero`. Absent: a dedicated controller. See [the pool design](design/cdc-pooling.md). |

### `status`

Conditioned status plus: `apiVersion`/`kind`/`resource` last applied — **the GVK a
blueprint must pin, see below** — `managed.versionInfo[]` (served CRD versions and
their CDC digests), `packageUrl`, `digest` (the FNV idempotency digest —
[overview](./overview.md)), and `target`
(`mode` Local/Remote, `connectionStatus` Healthy/Down, target k8s `version`,
kubeconfig Secret `resourceVersion`), and `controller` (below). A definition with an `apiRef` also carries
`ApiRefRBACComplete`: `True/ReadSetResolved`, or `False` with one of the reasons below.
Partial RBAC is never written. While it is `False` the definition makes no progress, so a
version bump is held too; the running controller and its compositions are untouched. It
clears by itself once the read-set resolves.

**`status.controller` names the Deployment that runs this definition's controller (#580). Read it;
do not derive `<resource>-<version>-controller`.** A Kind in a pool (`spec.controller.pool`) has no
such Deployment: its controller runs in `cdc-pool-<pool>`.

```
status.controller.mode         Dedicated | Pool
status.controller.namespace    krateo-system        # on the cluster status.target names
status.controller.deployment   pages-v1-0-0-controller | cdc-pool-pages
status.controller.pool         pages                # mode Pool only
```

A follower (another definition owns its Kind×version) names its owner's controller, which is the one
reconciling its instances. It is recorded when the definition's deployment is observed up to date, so
it describes what is deployed, not what the spec is moving to. `Ready` and `ControllerActive` below
are read from this same Deployment.

**`status.owner` names the definition a follower defers to (#616).** Read it to link a follower to its
owner; do not parse the owner out of a condition message.

```
status.owner.namespace   krateo-system
status.owner.name        widget-a
```

Set only on a **follower**: another definition targets the same Kind and version, generated its CRD
and runs the controller that reconciles this definition's instances. **Absent** on the owner itself
and on a definition with no follower relation, and **cleared** when a follower becomes the owner (for
example, when the owner is deleted). Recorded each time the definition is observed, from the same
election that names the owner in `ValuesSchemaAuthored`, `Available` and `Ready` messages.

<a id="observedgeneration"></a>
**`status.observedGeneration` is the generation the `Ready` beside it was determined from (#644).** A
`Ready` kept from an earlier spec never advances it. Compare it with `metadata.generation`:

| | Meaning |
|---|---|
| lower than `metadata.generation` | `Ready` is about an **earlier** spec. The definition has not been reconciled since the spec changed, or the pass failed before it could determine `Ready` (`Unknown`/`NotObserved`), or it is paused, or its update is withheld by `krateo.io/management-policy`, or an update is in progress (a new version waiting out a prune grace, an `Update` that fails, controller resources just applied). It does not say which, nor that anything is broken |
| equal, `Ready=True` | the spec is applied: `status.apiVersion` is the version its chart yields, and `Ready` is that version's controller |
| equal, `Ready=False` | this spec was judged and is not served yet, or is refused: the reason is about **this** spec. `status.apiVersion` may still name the version before |
| absent | **nothing**: the writer does not report it (an older core-provider, or an older `core-provider-crds` chart, which prunes it), or no pass has determined `Ready` yet. Absent is not "behind": in jq `null < 2` is true, so test for it first |

```
kubectl get compositiondefinition -A -o json | jq -r '.items[]
  | select(.metadata.deletionTimestamp == null and .status.observedGeneration != null
           and .status.observedGeneration < .metadata.generation)
  | "\(.metadata.namespace)/\(.metadata.name) \(.status.observedGeneration) < \(.metadata.generation)"'
```

What it does not say. It moves only with the **spec**: an unchanged definition reads equal while
core-provider is down, an annotation (`krateo.io/upgrade-to-version`, `krateo.io/values-schema`,
`krateo.io/management-policy`) moves no generation, and neither does a new core-provider that has not
reconciled the definition yet. The conditions other than `Ready` are guaranteed to be about this spec
only beside `Ready=True`. A deletion moves `metadata.generation` by itself and the field stays where it
was, so ignore it while `deletionTimestamp` is set. `kubectl get -o wide` shows both numbers
(`GENERATION`, `OBSERVED`). `kubectl wait` reads it: see
[gotchas](./internals/gotchas.md#kubectl-wait-on-a-definition-waits-for-its-current-spec).

<a id="migratedvaluesvalid"></a>
**`MigratedValuesValid` says whether every instance on the served version still satisfies that
version's schema after a version migration (#614).** A migration rewrites only an instance's
version label, and the apiserver ratchets validation on update, so an instance can move onto a
version whose schema refuses a value it already holds (a field whose kind changed). core-provider
checks each instance with a server-side **dry-run create** of a copy through the served version: a
create is validated in full and persists nothing.

| Status / reason | Meaning |
|---|---|
| absent | no migration has moved an instance since this was introduced |
| `True` / `AllValid` | every instance passed after the last migration; nothing reopens it until the next one |
| `False` / `ValuesRefusedByNewVersion` | the message names up to 10 instances (`ns/name (spec.field, ...)`); a write that changes those fields is refused until they satisfy the new schema |
| `Unknown` / `ValidationIncomplete` | none refused, but some could not be judged (the dry run failed for a reason other than a spec validation error) |

It runs after an Update pass that migrated at least one instance, and on every Observe while it is
`False` or `Unknown`, so it clears once the values are fixed or the instances are gone. **It never
changes `Ready`** and never blocks or undoes the migration. A `Warning` Event
`MigratedValuesRefused` fires once per new finding.

**`status.migratedValues` is the same finding as data (#623).** Read it instead of parsing the
message: it is built from the same check, in the same call, and names the same instances and fields
in the same order.

```
status.migratedValues.version          v2-0-0     # the served version checked
status.migratedValues.refusedCount     12         # uncapped
status.migratedValues.uncheckedCount   0          # uncapped
status.migratedValues.refused          [{namespace, name, fields: [spec.a, ...], fieldCount}]
status.migratedValues.unchecked        [{namespace, name}]
```

Both lists hold at most 10 instances and `fields` at most 5 entries, the same caps as the message. A
count larger than its list (`refusedCount > len(refused)`, `fieldCount > len(fields)`) means the cap
hid the rest. To tell a composition whether it is affected, look up its own `namespace`/`name` in
`refused`; beyond the cap, fall back to the counts. The field is **absent** while the condition is
absent and when the instance list itself could not be read (the condition is `Unknown` and says why),
and it is **cleared** when the condition becomes `True`.

**`ControllerActive`** (every local definition, #393) says whether anything is reconciling this
Kind right now, so a gate branches on a condition rather than matching a reason string. Its
reasons are shared verbatim with oasgen-provider (plumbing's `conditions` package). Messages carry
no counts, so the condition changes only when the state does:

| Controller | `Ready` | `ControllerActive` |
|---|---|---|
| running | `True/Available` | `True/Running` |
| at 0, opted into `scaleToZero` | `True/ScaledToZero` "idle: no instances since <t>; wakes on the first" | `False/ScaledToZero` |
| at 0, **not** opted in (e.g. a manual `kubectl scale`) | `False/Unavailable`: nothing reconciles the Kind | `False/Unavailable` |
| Deployment unreadable | unchanged | `Unknown/NotObserved` |

A follower definition (one sharing another's controller) keeps its own Ready and reports
`ControllerActive` for the shared controller. While a controller is at 0 its instances get no
status at all: the wake is the definition's `ControllerActive` going `False` → `True`. Waking
takes the scale patch (measured ~120ms after the instance is created, on envtest) plus pod
scheduling and start. `status.scaleToZero` records the last decision: `lastSeen` (only while the
Kind is empty), `replicas`, `reason` (`Active`, `IdleHoldDown`, `Idle`, `Unknown`) and `heldBy`
(sharing definitions that have not opted in).

**`status.servedVersionsPendingPrune`** (#454) lists the served versions of the Kind's CRD that
have become prune-eligible (no definition references them, no instance carries their label, no
stored helm revision names them), each with `eligibleSince`. A version is removed from the CRD only
after it has stayed eligible for **5 minutes**, each version on its own clock. A chart that renders
against the CRDs (the installer umbrella lists them, then `lookup`s each served version) therefore
never looks up a version pruned mid-render: before this, a prune landing in that gap failed the
whole render, and through chart-inspector's same render left `RBACComplete` unevaluated. While a
version waits, the definition stays up to date; it is pruned on the first reconcile after its
window. The window is 4x the slowest Installer reconcile pass measured on krateo-057 (74.5s).

| Reason | Meaning |
|---|---|
| `RESTActionNotFound` | snowplow says the RESTAction does not exist |
| `RESTActionForbidden` | The caller may not read it. It may also not exist, so this never means missing |
| `StageUnresolvable` | A stage cannot be enumerated without dispatching it (snowplow 422) |
| `InvalidApiRef` | snowplow refused the request, such as extras naming a reserved key |
| `Unauthorized` | No usable identity reached snowplow |
| `SnowplowUnavailable` | snowplow could not be reached or ran out of time |
| `SnowplowError` | Anything else, including cyclic stages and a 404 that is not snowplow's own |
| `SnowplowNotConfigured` | `apiRefRBAC.enabled` is off in the chart while the definition declares an `apiRef` |

### `status.writtenBy`: which binary, and which release's reason table (#669)

Every CompositionDefinition and every composition names the binary that made its **last status
write**, readable by anyone who can read the object (no Deployment read needed):

```
status.writtenBy.name      core-provider | composition-dynamic-controller
status.writtenBy.version   2.13.135        # the bare core-provider repository tag; dev on an unstamped build
status.writtenBy.commit    3f9c...         # unknown on an unstamped build
```

| `name` | on | its conditions are listed in |
|---|---|---|
| `core-provider` | a CompositionDefinition | [`cd-ready-reasons.json`](cd-ready-reasons.json) |
| `composition-dynamic-controller` | a composition | [`ready-reasons.json`](ready-reasons.json) |

Read the table at the tag `version` names. When `version` is not a tag (`dev`: a build that was
not stamped) or `writtenBy` is absent (the last write came from a release older than the field,
or a composition CRD that predates it prunes it), read the table at `HEAD` and say that you did.

- **The last status write only, not the author of every condition.** A condition that write did
  not rewrite (a condition type a newer version no longer writes) can still carry an earlier
  binary's text. Per-condition provenance, if it is ever needed, belongs on the condition.
- **Not separate writers:** provider-runtime runs inside core-provider and unstructured-runtime
  inside the composition-dynamic-controller, so their writes carry the binary's name. Two writes
  do not rewrite it, and the next ordinary pass does: unstructured-runtime's own writes, which
  patch `status.conditions` alone, and provider-runtime's write on a
  pass that ends before core-provider's code runs (a paused definition, `krateo.io/paused`: it
  keeps the previous binary's `writtenBy` until it is resumed).
- **Never a write of its own.** It is set on the object the writer is about to send anyway: a
  pass that changes nothing else sends the stored value back and the apiserver records nothing
  (resourceVersion unchanged). After a roll, the new binary's first status write carries its
  version.
- **A remote composition's hub copy** carries the spoke controller's `writtenBy`, copied with the
  rest of its status by core-provider's mirror: the conditions are the spoke controller's.
- `version` is written as the build carries it: release images are built with the bare git tag;
  it is never rewritten to look like one.

### The GVK a blueprint must pin

**Read it from the status; never re-derive it.**

```
status.apiVersion   composition.krateo.io/v0-4-3
status.kind         EventSite
status.resource     eventsites          # already pluralized
```

The derivation is real — `"v" + chart-version with "." → "-"` — but it is the **full
`major.minor.patch`**, so a *patch* bump moves the served apiVersion: chart `0.4.3` is
`v0-4-3`, not `v0-4`. A blueprint that hardcodes the apiVersion is correct only until
the next patch release of its own chart.

**Getting it wrong fails silently.** Controllers are per-version and label-scoped on
`krateo.io/composition-version`, so a CR emitted at a version no controller watches is
reconciled by **nobody** — no error on the CR, no event, no condition, nothing in any
log. The CR simply sits there. The apiVersion is not a reliable owner key; see
[composition-version-management](./design/composition-version-management.md) §2 and §6.

**The plural is not derivable and the `status` is the only source.** It comes from
`plumbing/kubeutil/plurals`, not from `strings.ToLower(kind) + "s"` — a fleet join that
assumed the naive rule silently dropped **32 of 83** rows. Read `status.resource`.

**Gate on the CRD, not on the CompositionDefinition's `Ready`.** `Ready=True` means the
CRD has been written; it does not mean the API answers. Until #269 shipped in 2.13.46 the
two could differ by the window between a CRD being admitted and becoming `Established`,
and even now the honest check is the CRD itself:

```
kubectl wait --for=condition=Established crd/<status.resource>.composition.krateo.io
```

`status.apiVersion`, `status.kind` and `status.resource` are written consecutively in one
status update, so there is no partial write to race: if one is present they all are.

**Where this bites hardest: vendored charts.** A namespaced child whose rendered manifest
omits `metadata.namespace` is the trigger for the published-path defect (#79), and the
pattern is third-party or vendored chart content rather than Krateo-authored blueprints —
`ecr.services.k8s.aws/v1alpha1 Repository` from the AWS ACK controller was one bad entry
out of 93 on a customer cluster, and krateo-observability's clickstack chart omits it on
secrets, configmaps, services and a deployment. So the exposure scales with how much
vendored content a blueprint pulls in, not with blueprint count — which means the
normalisation has to hold rather than the convention.

Do not compute `resource` either. Only `kind` comes from a string rule
(`flect.Pascalize` over `ToGolangName`); the plural is **resolved**, by
`plumbing/kubeutil/plurals` behind `internal/tools/pluralizer`, so it is not derivable
from the kind by any local rule. `strings.ToLower(kind) + "s"` disagrees with it often
enough to drop 32 of 83 rows from a fleet-wide join without erroring.

Old versions do not linger as a safety net — stale served versions are pruned once no
instance uses them (conservatively: never the current or `vacuum` version, never one
another definition references, and nothing at all is pruned while any instance's
version label is unaccounted for).

## `KubernetesTarget`

A registered spoke: `spec.kubeconfigRef` (`{namespace, name, key}` of a Secret
holding a complete kubeconfig — the rotation seam, ESO-friendly). Its own controller
probes the target periodically and fills `status`: `connectionStatus`
(Healthy/Down), the target's Kubernetes `version`,
`kubeconfigSecretResourceVersion`, `lastProbeTime`, and a Ready condition.
Optional `spec.authnUrl` (`^https?://...`): the authn **inside** the target that a
composition controller running there calls with its projected token (only that
target's authn can validate a token its apiserver minted). Unset, the controller is
given `http://authn.<authn namespace>.svc.cluster.local:8082`, the namespace
core-provider writes the controller's authn mapping into (#251).
Referenced by name from `CompositionDefinition.spec.deploy.targetRef` and resolved
in the referencing object's own namespace (no cross-namespace targeting).

## The generated CRDs (per chart)

For each definition the engine generates a CRD: group `composition.krateo.io`,
version `v<chart-version-dashed>`, kind = Pascalized chart name; `spec` = the chart's
`values.schema.json`; `status` = baseline keys (among them `writtenBy`, described under the
definition's status above) + any `statusDataTemplate` paths. One
CRD per (group, kind) carries **many served versions** plus a permissive `vacuum`
storage version; conversion is always `None`; instances get the
`krateo.io/composition-version` label stamped by the chart-shipped
`MutatingAdmissionPolicy`. Instances of these CRDs are Compositions, reconciled by
the per-version CDC.

## HTTP surfaces

- **Engine `:8080`** — controller-runtime metrics only. No content API; core-provider
  is a controller, not an HTTP service. OTLP export is opt-in
  ([configuration](./configuration.md)); catalog in
  [`go/core-provider/telemetry/metrics-reference.md`](../go/core-provider/telemetry/metrics-reference.md).
- **chart-inspector `:8081`** — `GET /resources?compositionName=…&compositionNamespace=…&compositionDefinitionName=…&compositionDefinitionNamespace=…&compositionVersion=…&compositionResource=…`
  server-side-renders the chart (`helm template --server`, lookups evaluated against
  the live cluster) and returns the resources an install would touch — the CDC's
  RBAC generator consumes it. Swagger UI at `/swagger/`. OpenAPI:
  [`go/chart-inspector/docs/swagger.yaml`](../go/chart-inspector/docs/swagger.yaml),
  narrative in [`go/chart-inspector/README.md`](../go/chart-inspector/README.md).
- **Consumed, not exposed**: snowplow `GET /rbac` (the `apiRef` read-set) and authn
  `POST /serviceaccount/login` (the CDC token exchange) — see the
  [apiRef how-to](./how-to/apiref-status-projection-authn.md).

---
type: Usage
title: core-provider — usage
description: How to install and consume core-provider — the installer pin, direct helm install from OCI, the Kubernetes 1.36 floor, and authoring your first CompositionDefinition.
resource: oci://ghcr.io/krateo-platformops/charts/core-provider
tags: [install, helm, oci, kubernetes-1.36]
timestamp: 2026-08-07T00:00:00Z
---

# Usage

## Requirements

- **Kubernetes >= 1.36** on the management cluster **and on every remote target**.
  Since 2.0.0 core-provider hosts no admission webhooks: generated CRDs use `None`
  conversion and the `krateo.io/composition-version` label is stamped by a
  `MutatingAdmissionPolicy` (GA in `admissionregistration.k8s.io/v1` at 1.36).
- Charts you manage must ship a **`values.schema.json`** and be a single-root tgz —
  the CRD is generated from that schema. A chart without it reads
  `Ready=False/ValuesSchemaMissing`, **unless the definition opts in to inference**
  with the annotation `krateo.io/values-schema: infer` (#568). The schema is then
  inferred from the chart's `values.yaml` files (plumbing `valuesschema`, rules v1):
  typed-open, every values.yaml leaf a `default`, no authored constraints.
  - The definition reads `ValuesSchemaAuthored=False/InferredFromValues`, naming the
    rules version and the paths that accept any value, and
    `SchemaFullyEnforced=Unknown/SchemaInferred`. Ready is unaffected.
  - An authored `values.schema.json` always wins over the annotation.
  - Any other annotation value is not an opt-in: the definition stays
    `ValuesSchemaMissing`, and the message names the value.
  - Rules, CLI vs opt-in, and limitations: [values-schema inference](./values-schema-inference.md).
- **Defaults come from `values.yaml`, not from the schema** (#686). The generated CRD
  defaults each field from the chart's root `values.yaml`, because that is what
  `helm install` uses: helm never applies a `values.schema.json` `default`. So a
  Composition and a plain `helm install` of the chart agree by construction.
  - A property whose path has a scalar, list or map in `values.yaml` defaults to that
    value, whatever the schema's `default` says.
  - A schema `default` with no `values.yaml` key at its path gets **no** CRD default
    (helm would not apply it either). The same holds for defaults inside `items` and
    `additionalProperties`.
  - An object keeps crdgen's `default: {}` only where a descendant has a
    `values.yaml`-sourced default; an object with none gets no `default`.
  - A `values.yaml` value the schema refuses (type, enum, bound) is not made a
    default, since the apiserver would refuse the whole CRD.
  - `SchemaDefaultsMatchValues` says where the two disagree: `True/Aligned`, or
    `False/SchemaDefaultsDiffer` naming each `path: schema=<v> values.yaml=<v>` or
    `path: schema default, no values.yaml key` (capped, `+N more`). Ready is unaffected.
    Maintainers: align `values.schema.json`'s defaults with `values.yaml`.
  - Compositions that already exist keep the values they store; defaulting fills
    omitted fields only. A served CRD version whose generated spec schema differs from
    the served one **only in `default` keys** (the `{}` object defaults included) is
    rewritten in place at upgrade, without a chart-version change, and
    `SpecSchemaCurrent` reads `True` afterwards. Reason: a field absent from a stored
    composition is already filled from values.yaml by helm (the cdc passes the spec as
    values, helm merges values.yaml under it), and values stored under the old defaults
    stay as stored, so a defaults-only rewrite cannot change an existing composition's
    render. Any other difference keeps the `GeneratedSchemaNotLooser` rule.
- The `apiRefRBAC` feature (on by default) expects the **authn** component's
  serviceaccount login strategy (CRD `serviceaccount.authn.krateo.io`) and
  **snowplow** in-cluster; set `apiRefRBAC.enabled=false` on clusters without them
  ([configuration](./configuration.md)).

## Via the Krateo installer (the normal path)

The installer's umbrella pins the `core-provider` chart and runs it as a composition
of the platform itself. You do not install it by hand; you get it (and its upgrades)
by moving the installer pin. Fresh installer deploys need the bootstrap flag
(`bootstrap.coreProvider.enabled=true`) so the engine exists before the umbrella's
compositions are reconciled — that flag belongs to the installer chart, not this one.

## Direct install (standalone)

The `core-provider` chart does **not** contain the two owned CRDs
(`compositiondefinitions.core.krateo.io`, `kubernetestargets.core.krateo.io`). They ship
in the `core-provider-crds` chart, published from this repo (`helm/core-provider-crds/`,
`oci://ghcr.io/krateo-platformops/charts/core-provider-crds`) and released **at the same
version** as `core-provider` on every tag (#157). Install it first, at the same version:

```sh
# VERSION: the latest release, https://github.com/krateo-platformops/core-provider/releases/latest
helm upgrade --install core-provider-crds oci://ghcr.io/krateo-platformops/charts/core-provider-crds \
  --version "$VERSION" --namespace krateo-system --create-namespace
helm upgrade --install core-provider oci://ghcr.io/krateo-platformops/charts/core-provider \
  --version "$VERSION" --namespace krateo-system
```

Pin both to **one** resolved version. Two independently resolved charts are only
coincidentally in step.

Without the CRDs the engine refuses to start (#589). It logs one error naming the
missing Kinds, the `core-provider-crds` chart and the version to install, then exits,
and recovers on the restart after the CRDs are installed. A `CompositionDefinition`
applied meanwhile fails with
`no matches for kind "CompositionDefinition" in version "core.krateo.io/v1alpha1"`.
The chart's `NOTES.txt` says the same after every `helm install`.

The chart deploys the engine **and** a chart-inspector Deployment/Service, ships the
CDC asset templates (mounted into the engine pod under `/tmp/assets/…`), and applies
the composition-version `MutatingAdmissionPolicy`.
Upgrades: `helm upgrade` with the new `--version`; the CDC and chart-inspector image
tags track the chart `appVersion`, so every spawned controller rolls to the matching
build on the next reconcile.

## First composition

Apply a `CompositionDefinition` referencing any schema-bearing chart:

```yaml
apiVersion: core.krateo.io/v1alpha1
kind: CompositionDefinition
metadata:
  name: core-provider
  namespace: krateo-system
spec:
  chart:
    url: oci://ghcr.io/krateo-platformops/charts/core-provider
    version: "2.12.4"
```

(This is the self-hosting pattern the Krateo installer uses: core-provider managing
its own chart as a composition.) When it reports `Ready=True`, a new API exists —
`coreproviders.composition.krateo.io/v2-12-4` — and a CDC Deployment named
`coreproviders-v2-12-4-controller` runs in the definition's namespace. Creating an
instance of that API installs the chart with the instance's spec as values. See
[examples](./examples.md) for runnable manifests, including a remote-target pair.

## Local render (no cluster)

The chart templates cleanly offline — useful for CI and review:

```sh
helm template core-provider ./helm/core-provider \
  --namespace krateo-system --set image.tag=2.12.4
```

(The packaged `Chart.yaml` carries `CHART_VERSION`/`APP_VERSION` placeholders that CI
substitutes at release; when rendering from a source checkout, pass an explicit
`image.tag` or substitute the placeholders first, as the lint workflow does.)

## Pausing a composition

Two annotations on a composition, with different meanings. Measured on 2.13.72 (one composition,
GKE), except the pending condition, which is new in this release:

| | `krateo.io/paused: "true"` | `krateo.io/gracefully-paused: "true"` |
|---|---|---|
| takes effect | at once, before the controller acts | after the **next change is applied** (#381) |
| until it takes effect | n/a | every change goes through: a deleted workload is recreated, a manual `kubectl scale` is reverted |
| while pending | n/a | `Synced=False/GracefulPausePending` |
| once in effect | `Ready=True/ReconcilePaused`, `Synced=False/ReconcilePaused` | `Ready=True/ReconcileGracefullyPaused`, `Synced=False/ReconcileGracefullyPaused` |
| holds a manual scale to 0 | yes (sampled for 150 s, across two resyncs) | not while pending |
| keeps a deleted workload deleted | yes (watched for 150 s) | not while pending |

**To stop reconciliation for maintenance, use `krateo.io/paused`.** `krateo.io/gracefully-paused` lets
the change in flight finish first; on a composition with nothing in flight it waits for the next change
and applies it. Removing either annotation resumes reconciliation; the controller then restores what
the chart renders.

## Limiting what the controller may do: `krateo.io/management-policy`

A set of verbs on a composition, from `observe`, `create`, `update` and `delete`, for example
`krateo.io/management-policy: observe,create`. `observe` is always implied, and the historical values
`default`, `observe-create-update`, `observe-delete` and `observe` are aliases for their sets. The
definition is plumbing's `managementpolicy`, shared with the runtimes (unstructured-runtime#110). A
value that does not parse acts as `observe` only. Measured on envtest (`TestEnvtest_597_*`, #597):

| the composition | `create` withheld | `update` withheld |
|---|---|---|
| release not installed | nothing is installed; `Ready=False/CreateWithheldByPolicy`, whose message names the policy and how to allow create; a parent counts it **failed** | — |
| chart version or values changed | — | **not applied**; the installed chart is frozen; `Ready=True/UpdateWithheldByPolicy` names the installed and the wanted chart version; `status.helmChartVersion` stays at the **installed** version |
| a child changed out of band | — | **not corrected**; `Ready=True/UpdateWithheldByPolicy` names the children it would create, delete or patch |

- **Only the healthy verdict is replaced.** If a child is unhealthy, Ready keeps that reason and its
  message adds that updates are withheld. A parent counts `UpdateWithheldByPolicy` healthy.
- **While update is withheld, nothing is written to the release:** the controller runs helm's own
  merge as a server-side dry run (plumbing `UpgradeConfig.WriteNothing`). Its derived RBAC is still
  refreshed, so it keeps the access it needs to observe and delete.
- **Drift that cannot be measured** (a child kind's webhook denies the dry run, or the stored release
  names a kind the cluster no longer serves) is not a failure: Ready's message says why, under
  `UpdateWithheldByPolicy` or, when a child is unhealthy, after the rollup's own reason.
- **A release stuck in a helm operation** past its grace period (or whose controller is gone) is **not**
  rolled back: the rollback is a write. `Ready=False/RecoveryWithheldByPolicy` names the helm status and
  how long it has been pending, and a parent counts it failed. It lasts until a human recovers the
  release, or the policy allows update. Within the grace, `PendingHelmOperation` is unchanged.
- `krateo.io/deletion-policy: orphan` keeps the release on delete, whatever the management policy
  allows; so does a deletion-policy value that is neither `delete` nor `orphan`.

## A first install that was interrupted or failed

A **first install** can end without ever deploying:

- *interrupted*: the composition controller loses its lease or its pod (for example during an unrequested GKE control-plane resize), and the release is left `pending-install`;
- *failed*: a hook exits non-zero, or a graceful stop of the controller (a core-provider roll, a node drain) cancels helm.

The chart's post-install hooks that had not run yet then never would: retried as an upgrade, only **upgrade** hooks run, and before #636 a composition read `Ready=True/Available` that way. The controller instead installs the release **again, as an install**, with the composition's current values and chart version:

1. an interrupted install is first recorded as failed, once nobody is running it: the pod or process that started it is gone, or it has been pending past the grace (`COMPOSITION_CONTROLLER_PENDING_GRACE`, 16m);
2. the hook Jobs the attempts recorded are deleted, and the controller waits until they and their pods are gone (at most 60s per pass; otherwise the pass fails and the next one waits again);
3. the release is installed over the failed revision, running every install hook. **Nothing is uninstalled**: objects and volumes the earlier attempt created are kept. An object or a field the new values no longer render is removed, as an upgrade would (`helm.sh/resource-policy: keep` is honoured, and an object that is not this release's is left alone). One limit: a retry that is itself killed while it applies the objects may leave behind a **field** it added to an object that was already there, if a later retry's values no longer render that field.

A Normal `InterruptedInstallRetried` or `FailedInstallRetried` Event says so at each retry, and `status.installRecovery` keeps it: `kind` (`Retried`), `lastAt`, `count` (cumulative, never reset; one per revision retried, however many passes it takes to start), `reason` (`Interrupted` or `Failed`), `message` (the same for a person) and `revision` (removed once deployed). A non-zero count says the chart's install hooks may have run more than once. A composition whose CRD predates the field does not keep it; the recovery runs anyway and the controller's log says so once.

While an install keeps failing it is retried at the controller's error backoff, with no cap. `Ready` is never `True`. After the first two consecutive failures it reads `False/Creating`, with helm's error, and `False/HelmOperationInProgress` while an attempt runs. From the **third** it reads `False/InstallFailing`, which a parent counts as failed, and it stays there through the retries (the message then also says one is running) until one succeeds: `the first install has failed N times (M automatic retries so far); last error: ...`. N is `status.installFailures.count` (consecutive failed attempts; removed once a revision is deployed, with `lastAt` and `lastError`, which can be read there while a retry runs), M is `status.installRecovery.count`. They differ: an interrupted attempt is retried but is not a failed one. **Interruptions** are capped at **3** per composition (`krateo.io/interrupted-install-reinstalls` counts them and is removed once the release deploys): after that the cause is not transient, and it falls back to the manual path below with a Warning `InterruptedInstallRetryCapped` Event. The release keeps the controller's usual history (`HELM_MAX_HISTORY`, 3 by default, at least 2 here).

A first render the chart or the values refuse, or RBAC the API server refuses, is a verdict: `Ready=False/Unavailable` with the error, as on an installed composition (#646). Any other failure before the install starts (a kind not served yet, a chart-inspector that fails or does not answer, RBAC that could not be applied for another reason) reads `False/Creating`, with the error, and keeps `False/InstallFailing` if installs had already failed that often. `ChartRenderable` and `RBACComplete` say which it is. None of them is a failed install: nothing is counted.

### When it applies

Only to a release that is **proven** never to have been deployed, and that **this controller installed**:

- *Never deployed*: the release history accounts for every revision from the first, and all failed (the latest may be the interrupted one). The mere absence of a deployed revision is not proof: helm prunes old revisions, so a release that was live once can be left with only failed ones. A retry that keeps failing pushes the first revision out of the history, so the controller keeps the proof on the composition as `krateo.io/release-never-deployed`, removed once a revision is deployed. Do not write it by hand.
- *Installed by this controller*: `status.helmOperation` records that revision's install, or (for a failed one) the mark above, which is only written once that is known. A release installed by hand, which a composition's `krateo.io/release-name` label can point at, is never retried.

Anything else keeps the behaviour from before: an upgrade for a failed release; for an interrupted one, a rollback to its last deployed revision, or to itself with `krateo.io/hooks-unverified` (`Ready=False/HooksMayNotHaveRun`) when there is none.

Not retried either:

- a **deleting** composition: its release is uninstalled by the delete;
- by a controller that is **retiring** (the definition moved to another chart version): the current version's controller does it;
- when the management policy withholds **update**: nothing is written to the release, and `Ready` reads `False/HooksMayNotHaveRun` (failed) or `False/RecoveryWithheldByPolicy` (interrupted);
- on the **manual path**: the annotation below, the cap, or a policy that withholds **create**.

### Keeping it manual

For a chart whose install hooks are not safe to run twice, set `krateo.io/interrupted-install-recovery: manual` on the CompositionDefinition (it applies to all its compositions) or on one composition. Any value of that annotation counts: an unrecognised value is never read as permission, and a definition whose annotations cannot be read counts as having it set.

The release is then not installed again. `krateo.io/hooks-unverified` is written first, so `Ready` reads `False/HooksMayNotHaveRun`, and:

- an interrupted install at revision 1 is rolled back to itself, which keeps what it created;
- a failed install, or an interrupted *retry* (revision 2 and above), is **upgraded** to the composition's values. That upgrade runs the chart's upgrade hooks only, and does not clear the mark. It may also delete an object whose name the first attempt rendered and the values no longer do, **even if something else owns that object now**: helm's upgrade does not check ownership.

Verify what the install hooks do, then remove `krateo.io/hooks-unverified`; a later upgrade also clears it.

### Downgrading

Before downgrading to core-provider 2.13.128, let every first install finish: no release may have a latest revision that is `pending-install`. That version recovers an interrupted install by **uninstalling** it, and it proves less about a release's history before doing so. Earlier versions do not uninstall.

## What a definition's chart creates: `status.contents`

A CompositionDefinition's status says what its chart renders **at the chart's default values**
(core-provider#599). The portal's blueprint page uses it:

```yaml
status:
  contents:
    chartVersion: 0.3.0     # the chart this was rendered from
    values: default         # always: a composition's own values can change the counts
    kinds:                  # by count desc, then apiVersion, then kind; at most 50
      - {apiVersion: rds.services.k8s.aws/v1alpha1, kind: DBInstance, count: 1}
      - {apiVersion: ec2.services.k8s.aws/v1alpha1, kind: Subnet, count: 3}
    omitted: 0              # kinds not listed because the list is capped
    unknown: ""             # set INSTEAD of kinds when the chart cannot render at its defaults
```

- It is the **release manifest** `helm install` would record: rendered templates, subcharts included,
  with helm's dependency conditions applied. **Hooks** (helm keeps them out of the manifest) and the
  **`crds/`** files (installed apart from the release) are not counted.
- core-provider renders the chart itself, offline, with helm's library: no cluster call, no
  chart-inspector. `lookup` returns nothing, as in `helm template`.
- A chart that needs a value it does not default, whose defaults violate its `values.schema.json`, or
  whose declared dependency is not vendored, gets `unknown` with the reason, never an empty list.
- After a chart version change, the previous contents stay, with their own `chartVersion`, until the
  new chart is readable and its render replaces them.
- It never affects Ready.
- Beside it, `status.chart` records the chart's `Chart.yaml` maintainers (name and url, never email: status is dumped wholesale, read the chart for contact
  details) and version, read in the same pass (core-provider#671): see [api.md](./api.md#statuschart-who-maintains-the-blueprint-671).

## Is a definition's `Ready` about its current spec?

Compare `status.observedGeneration` with `metadata.generation` (core-provider#644). Lower means the
`Ready` shown is about an earlier spec, for example because core-provider has not reconciled the
definition since its chart version was bumped. After a bump, wait for both:

```
kubectl wait compositiondefinition/<name> -n <namespace> --for=condition=Ready --timeout=600s
kubectl get compositiondefinition/<name> -n <namespace> -o wide   # GENERATION and OBSERVED are equal
```

`kubectl wait` already ignores a condition while the field is behind. What each case means, and what the
field does not say, is in [api.md](./api.md#observedgeneration).

## Consuming the docs at the right version

Image tag == chart version == repo tag (one version line). Read this bundle at the
tag matching the running build; [llms.txt](./llms.txt) carries the pin.

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

## Consuming the docs at the right version

Image tag == chart version == repo tag (one version line). Read this bundle at the
tag matching the running build; [llms.txt](./llms.txt) carries the pin.

---
type: Architecture
title: core-provider — runtime behavior
description: The reconcile lifecycle (Observe/Create/Update/Delete), what is deployed per version, the digest contract, status projection, version migration, and remote seeding. Code-traced.
resource: ghcr.io/krateo-platformops/core-provider
tags: [internals, reconcile, digest, cdc]
timestamp: 2026-08-07T00:00:00Z
---

# core-provider — runtime behavior

What the running engine does: the CRDs it owns, the reconcile lifecycle, what it
deploys, and its integration contracts. Traced against the current tree (paths
relative to `go/core-provider/`; cited by file + symbol — line numbers drift).

## CRDs it owns

- **`CompositionDefinition`** (group `core.krateo.io`, namespaced): the input — a
  Helm-chart reference (`spec.chart`), optional remote target
  (`spec.deploy.targetRef`), status projection (`spec.statusDataTemplate`,
  `spec.apiRef`), migration policy (`spec.upgradePolicy`) and per-Kind CDC tuning
  (`spec.controller`). See `apis/compositiondefinitions/v1alpha1/types.go` and
  `crds/core.krateo.io_compositiondefinitions.yaml`.
- **`KubernetesTarget`** (`core.krateo.io`, **namespaced**): names a Secret key
  holding a target cluster's kubeconfig
  (`crds/core.krateo.io_kubernetestargets.yaml`). Resolved in the SAME namespace as
  the referencing `CompositionDefinition` (no cross-namespace targeting), which lets
  it be created through snowplow's force-namespaced `/call` write path. Its own
  controller (`internal/controllers/kubernetestargets/`) probes reachability and
  fills `status` ([architecture](./architecture.md)).

Per `CompositionDefinition`, core-provider **generates a third CRD** at runtime — the
one derived from the referenced chart (group `composition.krateo.io`, version
`v<chart-version-dashed>`, kind = Pascalized chart name;
`internal/tools/chart/chart.go`, `ChartGroupVersionKind`). Instances of THAT
generated CRD are "Compositions".

## The reconcile lifecycle (`internal/controllers/compositiondefinitions/compositiondefinitions.go`)

The provider-runtime reconciler drives the standard Observe / Create / Update /
Delete cycle. Each method first fetches+unpacks the chart, derives the GVK, and
reads `values.schema.json`.

### Observe (`external.Observe`)

1. Sets `status.target` by probing the target cluster's discovery endpoint —
   `Healthy`+k8s version or `Down` (`setTargetStatus`), and ensures the
   composition-version `MutatingAdmissionPolicy` exists on the target
   (`ensureCompositionVersionPolicy`, `internal/tools/policy`).
2. Resolves the GVR via the pluralizer; if the CRD's plural isn't discoverable yet
   it falls back to the GVR computed from the generated CRD. For a deleted CR with
   no resolvable GVR it reports the external resource gone.
3. If the CRD or the requested version is missing →
   `ResourceExists:false/true, UpToDate:false` with an `Unavailable` condition.
4. Compares the status sub-schema (`crdutils.StatusEqual`) — drift → not-up-to-date;
   re-injects `statusDataTemplate` fields the same way Create does.
5. Counts existing Compositions (`getters.GetCompositions`) and prunes served CRD
   versions no definition references anymore (`pruneStaleServedVersions` — the old
   version's inert controller is retired with it).
6. Runs `deploy.Deploy` in **server dry-run** (`DryRunServer:true`) to compute the
   digest of what *would* be rendered, and `deploy.Lookup` to compute the digest of
   what *is* deployed. If either differs from `status.digest` → not-up-to-date.
7. Otherwise refreshes status and sets `Available`.

### Create (`external.Create`)

Generates the CRD, `ApplyOrUpdateCRD`, then `deploy.Deploy` (real apply), and stores
the returned digest in `status.digest`. When `spec.statusDataTemplate` is set, after
`GenerateCRD` it runs `ValidateStatusFields` then `InjectStatusFields` to extend the
generated CRD's **status** schema with each declared `forPath` before applying; a
validation error (empty/duplicate `forPath`, collision with a reserved baseline
status key, type/schema conflict, or an unparseable `${ jq }`) fails the reconcile.
For a remote target it first seeds the spoke (`seedRemoteTargetIfNeeded` →
`deploy.SeedRemoteTarget`) and mirrors the generated CRD back onto the hub
(`applyHubCompositionCRD`) so hub-side tooling can resolve the Kind.

### Update (`external.Update`)

Same as Create, plus version migration gated by **`spec.upgradePolicy`**
(`cr.MigrationApproved(version)`): under `Automatic` (or unset) it migrates eagerly;
under `Manual` only when the `krateo.io/upgrade-to-version` annotation names the
target version; under `Paused` never. When migration is approved and the chart GVK
changed, it **undeploys the CDC for the old version** (keeping the CRD,
`SkipCRD:true`) and **rewrites live Compositions to the new apiVersion**
(`getters.UpdateCompositionsVersion` — owner-scoped, written through the new
endpoint so the new label-scoped controller selects them). Under Manual/Paused the
old version's controller is kept so coexisting instances stay served.

### Delete (`external.Delete`)

Sets `Deleting`. If this is the **last** `CompositionDefinition` for that version,
it deletes that version's Compositions first and waits for them to be gone
(returning an error to requeue while any remain — the still-running CDC finalizes
them; `deploy.ErrCompositionStillExist` also requeues). It only deletes the CRD when
**no other** `CompositionDefinition` shares the group/kind (`SkipCRD` otherwise),
then `deploy.Undeploy`. For remote targets, discovery runs against the **spoke**
(the generated CRD lives there) and the seeded assets are torn down ref-counted
(`teardownRemoteSeedIfNeeded`) after the last definition for that spoke goes.

## What it deploys per version (the CDC) — `internal/tools/deploy/`

`Deploy` renders and applies, hashing each object into one FNV digest:

1. **RBAC**: a ServiceAccount, ClusterRole/ClusterRoleBinding, Role/RoleBinding
   (`createRBACResources`); plus a Secret-scoped Role/RoleBinding when the chart
   uses private-repo credentials.
2. **JSON-schema ConfigMap** holding the chart's `values.schema.json`.
3. **CDC config ConfigMap** carrying the SA name/namespace plus the
   status-projection config: `COMPOSITION_CONTROLLER_STATUS_DATA_TEMPLATE` (the
   encoded `statusDataTemplate`) and
   `COMPOSITION_CONTROLLER_API_REF_NAME`/`_NAMESPACE`/`_EXTRAS` (the `apiRef`),
   empty when not declared; per-definition `spec.controller` tuning rides as args.
4. **Deployment** running the CDC image the chart wired
   (`ghcr.io/krateo-platformops/composition-dynamic-controller`, tag tracking the
   chart `appVersion`) with `-group/-version/-resource/-namespace` args (template
   mounted from the chart at `/tmp/assets/cdc-deployment/`). **Only when `apiRef`
   is declared** it also mounts a projected `authn`-audience ServiceAccount token at
   `/var/run/secrets/krateo.io/serviceaccount/token` (1h expiry; volume and mount
   gated on `api_ref_name` in the asset template).
5. **Service** — only if the service template file exists on disk.
6. **authn allowlist mapping** — only when `apiRef` is declared: a
   `serviceaccount.authn.krateo.io/ServiceAccount` named
   `cdc-<compositionNamespace>-<saName>`, created in the authn operator namespace
   (`COMPOSITION_AUTHN_NAMESPACE`), referencing the per-composition CDC SA and
   granting the group `krateo:cdc:<resource>-<apiVersion>` (`authnmapping.go`). It
   is hashed into the digest so Deploy/Lookup agree, and removed on `Undeploy`
   (not-found / missing-CRD tolerated so teardown never blocks).
7. **`apiRef` read-set RBAC** — when `apiRefRBAC` is enabled: snowplow's
   `GET /rbac` enumerates the (group,version,resource,namespace,verb) rows the
   RESTAction's in-cluster calls touch; `restactionrbac.go` turns them into a
   ClusterRole/Binding for the per-composition group. A snowplow 422 (unresolvable
   stage) surfaces as the `ApiRefRBACIncomplete` condition — **partial RBAC is
   never written**.

In non-dry-run mode it waits for the Deployment to be Ready, restarts it so it picks
up the new ConfigMap, then waits again. Resource names follow
`<plural>-<version>-controller`, `-configmap`, `-jsonschema-configmap`
(`resourceNamer`).

## The idempotency / digest contract

core-provider is **digest-driven**, which is what keeps it from churning stateful
components on no-op reconciles: `status.digest` is an order-stable FNV hash over
every rendered object's identity + spec. Observe compares the *would-render* digest
(dry-run `Deploy`) AND the *live* digest (`Lookup`) against `status.digest`; only a
real difference triggers Create/Update. A reconcile that changes nothing computes an
identical digest and is a no-op.

## Generated-CRD behavior

- **Multi-version**: when a new chart version is applied for an existing kind,
  `ApplyOrUpdateCRD` (`internal/tools/crd/crd.go`) appends the new version (served)
  and demotes the others to served-not-storage, with a permissive **`vacuum`**
  version as the single storage version for lossless cross-version storage
  (`generation.go`, `AppendVersion`). Served versions no definition references are
  pruned on Observe.
- **No conversion webhook**: generated CRDs always use `Strategy: None`
  (`setNoneConversion`). The per-object `krateo.io/composition-version` label is
  stamped in-apiserver by the **`MutatingAdmissionPolicy`** — shipped by the chart
  locally and projected by the engine onto remote targets (`internal/tools/policy`).
- **Status-only updates**: if only the status schema differs, core-provider updates
  just the status sub-schema across versions to avoid disturbing the dynamically
  generated spec.

## Status projection & `apiRef` (since 2.3.0)

core-provider does NOT evaluate the projection itself — it (a) widens the generated
CRD's status schema so the declared `forPath`s are valid status properties, and
(b) ships the config to the CDC, which evaluates each `${ jq }` over a combined root
(`self`/`spec`/`status`/`helm`/`api`) every reconcile and writes the results under
`.status`:

- **Schema injection**: `InjectStatusFields` adds each declared `forPath` (nested
  objects allowed) under the status schema of every served version, the leaf typed
  from `schema` → `preserveUnknownFields` → scalar `type` → string fallback. A
  `forPath` whose top segment is a reserved baseline status key (`conditions`,
  `digest`, `previousDigest`, `managed`, `helmChartUrl`, `helmChartVersion`,
  `observedGeneration`) is rejected (`statusfields.go`).
- **authn handshake (only with `apiRef`)**: the CDC presents its projected
  `authn`-audience token to authn (`POST /serviceaccount/login`), which exchanges it
  for a short-lived service JWT — but only for SAs on its allowlist; the mapping
  core-provider auto-creates supplies that entry and the issued group
  `krateo:cdc:<resource>-<apiVersion>`. The CDC then resolves the `RESTAction` via
  snowplow under that identity. The group's RBAC is auto-generated when
  `apiRefRBAC` is enabled (step 7 above); with it disabled the platform operator
  must author the binding — see
  [the how-to](../how-to/apiref-status-projection-authn.md).

## Local vs remote targets

With no `spec.deploy.targetRef`, everything (generated CRD, RBAC, CDC) is deployed
into the management cluster (`DeploymentModeLocal`). With a `targetRef`,
`clusterkube.Remote` reads the namespaced `KubernetesTarget` (resolved in the
`CompositionDefinition`'s OWN namespace) → its kubeconfig Secret (a full kubeconfig,
or the token+server(+ca.crt) shape ESO mints) → builds target-cluster clients; the
`CompositionDefinition` and secrets stay local. The spoke is **seeded** so its CDC
is self-sufficient (embedded `CompositionDefinition` CRD, chart-inspector, admission
policy, shadow definition — `remoteseed.go`), and the `compositionmirror` controller
reflects hub instances ↔ spoke status. `status.target` reports `mode`,
`connectionStatus`, the target's k8s `version`, and the kubeconfig Secret's
`resourceVersion` for rotation traceability.

## Integration contracts (endpoints)

- **`:8080`** — controller-runtime metrics server. No `/call`-style content API;
  core-provider is a controller, not an HTTP service.
- **OTLP export** — opt-in per signal via `OTEL_ENABLED` / `OTEL_TRACING_ENABLED` /
  `OTEL_LOGS_ENABLED`; service name `core-provider`. Catalog and example queries in
  `telemetry/metrics-reference.md`.
- **chart-inspector** — called (by the CDC, and projected onto spokes) to compute
  chart RBAC; **snowplow `GET /rbac`** and **authn** — the `apiRef` authorization
  chain ([configuration](../configuration.md)).
- **The CDC** it deploys is the runtime that actually reconciles Compositions —
  built from this same monorepo and version-locked to the release.

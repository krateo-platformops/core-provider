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

#### Narrowing transitions discard spec fields, and say so (#188)

The `krateo.io/composition-version` label is written by the cluster-wide
`krateo-composition-version` `MutatingAdmissionPolicy`, which fires on CREATE and
UPDATE and unconditionally sets it to the request's version. The label therefore
*means* "the served version this object was last written through", so the only way
to move an instance onto a version is to **write it through that version's
endpoint** — and the apiserver prunes every `spec` field the request version's
schema does not declare. When the target schema is **narrower**, the relabel
deletes data.

This is not confined to downgrades. Composition CRD schemas are generated from each
chart's `values.schema.json`, so an *upgrade* narrows too whenever a chart author
removes or tightens a values property. The predicate is schema containment, not
version ordering.

The transition is **allowed** — the usual narrowing transition is a rollback, and
blocking it would turn a recovery path into a dead end at the worst moment — but it
is no longer silent. Before the write, the instance is re-read through the *source*
version's endpoint and run through the apiserver's own pruner
(`structuralpruning.PruneWithOptions`, so the prediction cannot disagree with the
write), and the doomed subtree is recorded **in the same update that discards it**:

| where | what |
| --- | --- |
| `krateo.io/pruned-fields` annotation | the dropped subtree as JSON, **with its values**, so it can be re-applied by hand |
| `krateo.io/pruned-fields-truncated` | `"true"` when the subtree exceeded 16 KiB and only the paths were stored |
| `CompositionFieldsPruned` Event (Warning) | on the composition instance, naming the paths |
| controller log (Warn) | the paths and whether the subtree was recorded, **never the values** |

**Above 16 KiB the values are not kept anywhere in the cluster**: restore them from
the source manifest, and the Event says so. They stay out of the controller log on
purpose, because a log reaches further than the object does, and a composition can
hold values nobody wants there.

Annotations are used rather than Events alone because Events expire, and this is
the only remaining copy of the data. They are safe for it: the pruner skips
`ObjectMeta` at the resource root, so the annotation survives the write that prunes
the spec.

Reads are pruned too, not only writes — an object fetched through a narrower served
version comes back already missing the undeclared fields, even though the data is
still in storage (`vacuum` preserves it) and still visible through the wider
endpoint. That is why detection re-reads through the source version rather than
reusing the object the migration already listed.

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
   uses private-repo credentials; plus the **ownership Lease grant**, a
   Role/RoleBinding `<plural>-<version>-lease` in the controller's namespace
   (`leaserbac.go`): `create` on leases, and `get`/`update` on the one Lease the
   controller takes. Deleted by `Undeploy` with the ServiceAccount, after the drain.
2. **JSON-schema ConfigMap** holding the chart's `values.schema.json`.
3. **CDC config ConfigMap** carrying the SA name/namespace plus the
   status-projection config: `COMPOSITION_CONTROLLER_STATUS_DATA_TEMPLATE` (the
   encoded `statusDataTemplate`) and
   `COMPOSITION_CONTROLLER_API_REF_NAME`/`_NAMESPACE`/`_EXTRAS` (the `apiRef`),
   empty when not declared; per-definition `spec.controller` tuning rides as args.
   `URL_AUTHN` is the authn the controller exchanges its projected token with, for
   where it runs (`controllerAuthnURL`, #251): a remote target's
   `KubernetesTarget.spec.authnUrl`, core-provider's own `CORE_PROVIDER_AUTHN_URL`
   (chart `apiRefRBAC.authnUrl`) for a local controller, else
   `http://authn.<COMPOSITION_AUTHN_NAMESPACE>.svc.cluster.local:8082`. Never empty:
   the controller's `env.String` reads a set-but-empty value as `""`.
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
   The digest covers what is rendered, not the live object: Observe also reads the
   live mapping, and one that is missing, or authorizes another ServiceAccount or other
   groups, is "update pending" (#642).
   **core-provider's own mapping is not in this list and not written by `Deploy`**
   (`selfauthnmapping.go`). It is what lets core-provider fetch the read-set of step 7,
   and Observe runs `Deploy` as a dry run, which persists nothing: while `Deploy` wrote
   it, a missing one could never come back (#642). Observe, Create and Update ensure it
   with a real write on the management cluster before calling `Deploy`, for a definition
   with an `apiRef`, when `CORE_PROVIDER_APIREF_SELF_SA_NAME` is set. A write is sent only
   when the mapping is absent or its spec differs. Observe writes nothing for a definition
   that is paused, being deleted, or whose `krateo.io/management-policy` withholds update.
   It stays outside the digest.
   **`AuthnMappingApplied`** is computed from what was read of the two mappings on that
   pass: `True/Applied` when both authorize, `False/AuthnNotInstalled` when a cluster one
   of them belongs on serves no mapping kind (the message names the cluster; nothing
   fails because of it, and every pass looks again), and no condition when one could not
   be read or is missing while the kind is served.
7. **`apiRef` read-set RBAC** — when `apiRefRBAC` is enabled: snowplow's
   `GET /rbac` enumerates the (group,version,resource,namespace,verb) rows the
   RESTAction's in-cluster calls touch; `restactionrbac.go` turns them into a
   ClusterRole/Binding for the per-composition group. A snowplow 422 (unresolvable
   stage) surfaces as the `ApiRefRBACComplete=False` condition — **partial RBAC is
   never written**.

**One reconciler per Kind × version** ([pool step 2](../design/cdc-pooling.md)).
The config ConfigMap carries `COMPOSITION_CONTROLLER_LEASE_NAME`, the
`coordination.k8s.io` Lease `<version>.<plural>.<group>`
(`templates.ControllerLeaseName`; the same template value renders the Role's
`resourceNames`). The CDC (`internal/ownership`, unstructured-runtime's
`controller.Ownership`) takes it in its own namespace as its pod name before any
worker starts:

- it lists first and is **Ready while waiting**. A Deployment's default rolling
  update (maxUnavailable 0) stops the old pod only once the new one is Ready, so
  a readiness that waited for the Lease would never let the old pod go;
- the old pod drains its in-flight reconciles, **then** releases the Lease; the
  new pod takes it on its next attempt (2s). The two never reconcile at the same
  time, so reconciling resumes about drain + 2s after the old pod stops, not when
  the new pod turns Ready. The witness that a controller reconciles is the Lease's
  `spec.holderIdentity` being its pod name, not pod readiness;
- a Lease it cannot read or write (e.g. missing RBAC) fails `/readyz`, naming the
  Lease;
- losing the Lease (no renewal within 10s) cancels in-flight reconciles at once
  and exits the process, which restarts and waits for the Lease again;
- an unset `COMPOSITION_CONTROLLER_LEASE_NAME` (a ConfigMap rendered before this)
  runs without a Lease, as before. A set name with no `POD_NAMESPACE`/`POD_NAME`
  refuses to start.

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
  `OTEL_LOGS_ENABLED`; service name `core-provider`. `OTEL_AUDIT_LOGS_ENABLED` (chart
  `otel.auditLogs`, for core-provider and every composition controller) exports only the controller
  audit records (`event.name` = `audit`), leaving the log stream on stderr. Catalog and example queries in
  `telemetry/metrics-reference.md`.
- **chart-inspector** — called (by the CDC, and projected onto spokes) to compute
  chart RBAC; **snowplow `GET /rbac`** and **authn** — the `apiRef` authorization
  chain ([configuration](../configuration.md)).
- **The CDC** it deploys is the runtime that actually reconciles Compositions —
  built from this same monorepo and version-locked to the release.

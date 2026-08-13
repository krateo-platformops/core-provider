---
type: Architecture
title: core-provider — engine internals (architecture)
description: How the engine binary is built — entry point, the three controllers, the API types, and the internal/tools packages that do the work. Code-traced against the monorepo tree.
resource: ghcr.io/krateo-platformops/core-provider
tags: [internals, controller-runtime, engine]
timestamp: 2026-08-07T00:00:00Z
---

# core-provider — architecture

How the engine is built. This is the internals view; the deployment/chart view lives
in this same monorepo under [`helm/core-provider/`](../../helm/core-provider) and
[configuration](../configuration.md). Every claim below is traced to the current tree
(paths relative to `go/core-provider/`, cited by file + symbol — line numbers drift);
if a comment or README disagrees with the code, the code wins.

> **Read this as a map.** core-provider is a controller-runtime manager hosting
> **three controllers**; the interesting logic is in the `internal/tools/*` packages
> the main reconciler calls. (This doc predates the monorepo fold and used to say
> "exactly one controller" — the `kubernetestargets` and `compositionmirror`
> controllers have since been added.)

## What core-provider is

A Krateo provider that makes a Helm chart a first-class Kubernetes API. For each
`CompositionDefinition` custom resource (a chart reference) it:

1. fetches the chart and derives a `GroupVersionKind` from its `Chart.yaml`;
2. generates a versioned CRD from the chart's `values.schema.json`;
3. deploys a per-version **composition-dynamic-controller (CDC)** — a Deployment plus
   a config ConfigMap, a JSON-schema ConfigMap, optional Service, and RBAC — that
   reconciles instances ("Compositions") of the generated CRD.

Module `github.com/krateo-platformops/core-provider` (`go.mod`) — the canonical home
since the 2026-08 independence migration; the historical upstream org is dead. It is
real service code (~10k+ LOC of Go), not a thin wrapper, and shares this monorepo
with the CDC and chart-inspector it orchestrates.

## Entry point (`main.go`)

`main()` is a standard controller-runtime program:

- Flags/env are prefixed `CORE_PROVIDER_*`: `-debug`, `-sync` (cache re-list,
  **default 10m** — deliberately short so a missed watch event self-heals within
  minutes instead of hanging a live version bump for up to an hour), `-poll` (drift
  poll, default 3m), `-max-reconcile-rate` (default 5), `-leader-election`,
  exponential retry bounds (`-min/-max-error-retry-interval`).
- Telemetry is opt-in per signal: `-otel-enabled` (OTLP metrics),
  `-otel-tracing-enabled`, `-otel-logs-enabled` (env `OTEL_ENABLED` /
  `OTEL_TRACING_ENABLED` / `OTEL_LOGS_ENABLED`). Logs are always emitted as
  OTel-model JSON on stderr via provider-runtime's `logging.NewOTelHandler` (the
  old local `loghandler` duplicate is gone); `telemetry.Setup` +
  `coretelemetry.SetupOTLPLogs`/`SetupTracing` wire the exporters.
- The manager binds a metrics server on `:8080` and uses a priority work queue
  (`ctrl.NewManager` options). **There is no webhook server or serving
  certificate** — core-provider hosts no admission webhooks since 2.0.0 (see the
  comment above `ctrl.NewManager`).
- It registers the APIs (`apis.AddToScheme`) and wires **three** controllers:
  `compositiondefinitions.Setup` (with a non-fake `pluralizer.New(false)`),
  `kubernetestargets.Setup`, and `compositionmirror.Setup`.

## The API types (`apis/compositiondefinitions/v1alpha1/types.go`)

Two CRDs live in this group, both **namespaced**:

- **`CompositionDefinition`**: `spec.chart` (`ChartInfo`: `url`, `version` (max 20
  chars), `repo`, `insecureSkipVerifyTLS`, optional `credentials`) and optional
  `spec.deploy.targetRef` selecting a remote target (`TargetReference`, resolved in
  this object's own namespace). Since 2.3.0 it carries the two **status-projection**
  fields — `spec.statusDataTemplate` (`[]StatusFieldMapping`:
  `{forPath, expression, type?, schema?, preserveUnknownFields?}`, snowplow
  `widgetDataTemplate`-shaped `${ jq }` mappings written under `.status`) and
  `spec.apiRef` (`ApiReference`: RESTAction `name`/`namespace` + static `extras`) —
  plus, later, `spec.upgradePolicy` (`Automatic`/`Manual`/`Paused` +
  the `krateo.io/upgrade-to-version` annotation gate, `MigrationApproved`) and
  `spec.controller` (`ControllerConfig`: per-Kind CDC `workers`/`resyncInterval`).
  Its `status` carries the conditioned status, last-applied
  `apiVersion`/`kind`/`resource`, `managed.versionInfo`, `packageUrl`, a `target`
  block (mode/connection/version/secret resourceVersion) and the **`digest`** of the
  rendered CDC resources.
- **`KubernetesTarget`** (namespaced — migrated from cluster-scoped 2026-07-14):
  `spec.kubeconfigRef` — a `SecretKeySelector` to a Secret key holding the target
  cluster's kubeconfig; the credential-rotation seam. It now has its **own status**
  (`connectionStatus`, `version`, `kubeconfigSecretResourceVersion`,
  `lastProbeTime`) filled by the `kubernetestargets` controller.

The provider's own CRD manifests are checked into `crds/` (drift-gated in CI; **not**
packaged in the chart — the installer bootstrap applies them).

## The controllers (`internal/controllers/`)

### `compositiondefinitions/` — the main reconciler

`Setup` builds a `provider-runtime` reconciler over `CompositionDefinition` and:

- on startup, removes an obsolete finalizer label for backward compatibility
  (`cleanupObsoleteFinalizerLabels`);
- watches **Secrets** and **KubernetesTargets** so a rotated chart credential or
  repointed kubeconfig re-reconciles the affected `CompositionDefinition`s promptly.

`Connect` builds the `external` client. By default it targets the local management
cluster; when `clusterkube.IsRemote(cr.Spec.Deploy)` it resolves the remote clients
and flips `kube`/`dynamic`/`client` to the target cluster while keeping `mgmt`
local. The `external.mgmt` client always holds the `CompositionDefinition` and its
secrets; `external.kube`/`dynamic`/`client` are where the CRD, RBAC and CDC land.

The four reconcile methods are detailed in [behavior.md](./behavior.md).

### `kubernetestargets/` — spoke registration

Periodically probes each `KubernetesTarget`'s API server and records reachability
(Healthy/Down), the reported Kubernetes version, and the kubeconfig Secret
`resourceVersion` used — turning the passive reference into an observable spoke.

### `compositionmirror/` — hub→spoke reflection

For remote-targeted definitions, reconciles the *set* of hub `Composition` instances
against their spokes: spec down (hub wins on drift), status back up (best-effort),
set-difference GC of orphaned spoke instances. Per-instance fan-out via the
`krateo.io/target` annotation; falls back to the definition's `targetRef`. Design:
[remote-composition-mirror](../design/remote-composition-mirror.md).

## The key flows (the `internal/tools/*` packages)

The controller is thin; the work is in these packages:

- **`chart`** (`chart.go`): fetches the chart bytes with bounded retry
  (`ChartInfoFromSpec`; retryability classifier treats 400/401/403/404/422 as
  permanent), unpacks the single-root tgz (`ChartInfoFromBytes`), derives the GVK —
  group is the fixed `composition.krateo.io`, version is
  `v<chart-version-with-dots-as-dashes>`, kind is the Pascalized chart name
  (`ChartGroupVersionKind`) — and reads `values.schema.json` (`ChartJsonSchema`).
  `chartfs/` provides the same over an `fs.FS` view.
- **`crd/generation`** (`generation.go`): `GenerateCRD` turns the chart's spec
  schema + a static status schema into a CRD via `plumbing/crdgen`. `AppendVersion`
  adds a new served version to an existing CRD and injects a permissive **`vacuum`**
  storage version for lossless multi-version storage. `StatusEqual` compares only
  the status sub-schema by FNV hash. `statusfields.go` extends the generated status
  schema from `statusDataTemplate`: `ValidateStatusFields` (non-empty `forPath`, no
  collision with reserved baseline status keys, no duplicates,
  type/schema/`preserveUnknownFields` mutual exclusion, parseable `${ jq }`) and
  `InjectStatusFields` (writes each `forPath` as a possibly-nested property under
  the status schema of **every** version, so projected writes survive
  status-subresource pruning). The controller calls both around `GenerateCRD` on
  Create/Update and re-injects on Observe.
- **`crd`** (`crd.go`): `ApplyOrUpdateCRD` creates the CRD if absent, else
  status-only-updates, else appends a version; it always sets **`None` conversion**
  (`setNoneConversion`) and waits for the CRD to be Established.
- **`deploy`** (`deploy.go` + siblings): `Deploy` renders and applies the CDC's
  RBAC, the two ConfigMaps, the Deployment and optional Service, hashing every
  object into a single FNV **digest**; `Lookup` recomputes that digest from the live
  cluster; `Undeploy` tears it down. Resources are named
  `<plural>-<version>-controller` etc. (`resourceNamer`). Siblings grown since the
  first version of this doc: `chartinspector.go` (derives the chart-inspector
  Service coords from the cdc-configmap template so it can be projected onto
  spokes), `restactionrbac.go` (turns snowplow's `GET /rbac` read-set into
  ClusterRole/Binding for the per-composition group; propagates `ErrIncomplete` →
  the `ApiRefRBACIncomplete` condition, never partial RBAC), `authnmapping.go`
  (auto-provisions the `serviceaccount.authn.krateo.io/ServiceAccount` allowlist
  mapping named `cdc-<namespace>-<sa>`, hashed into the digest, removed on
  `Undeploy`), `remoteseed.go` (`SeedRemoteTarget` — projects the embedded
  `CompositionDefinition` CRD, chart-inspector and a shadow definition onto a spoke;
  ref-counted teardown), `chartreach.go`, `controllerconfig` (per-definition
  `spec.controller` args). The status-projection config rides to the CDC through its
  config ConfigMap as `COMPOSITION_CONTROLLER_STATUS_DATA_TEMPLATE` and
  `COMPOSITION_CONTROLLER_API_REF_NAME`/`_NAMESPACE`/`_EXTRAS`; when `apiRef` is
  declared the Deployment also mounts a projected `authn`-audience ServiceAccount
  token at `/var/run/secrets/krateo.io/serviceaccount/token` (1h expiry, gated on
  `api_ref_name` in the deployment asset template).
- **`policy`** (`policy.go`): projects the composition-version
  `MutatingAdmissionPolicy` + binding (`krateo-composition-version`) into clusters
  where composition CRDs live — the management chart ships it locally; **remote
  targets get it from the engine** during seeding.
- **`clusterkube`** (`clusterkube.go`): resolves the local-or-remote target clients
  from a `KubernetesTarget` + kubeconfig Secret, re-read every reconcile so external
  rotation is picked up (`Remote`).
- **`authn`** / **`restactionrbac`** (top-level packages): the authn login client
  (projected-token → service JWT) and the snowplow `GET /rbac` client used by
  `deploy/restactionrbac.go`.
- Supporting: `pluralizer` (GVK→GVR via discovery), `objects` (render a template
  into a typed k8s object), `kube` (apply/uninstall over plumbing's
  `dynamic.Interface`), `deployment` (CDC restart + readiness), `resolvers` (Secret
  lookup), `retry`, `tgzfs`, `strutil`, `context` (logger in ctx). Object hashing
  moved to `plumbing`'s hasher.

## Build & runtime shape

- `go/core-provider/Dockerfile` builds the static `manager` binary; the monorepo CI
  builds all three module images from one tag ([release](../release.md)).
- The CDC image it deploys is `ghcr.io/krateo-platformops/composition-dynamic-controller`,
  **wired by the chart** (`cdc.image.*` in `helm/core-provider/values.yaml`, tag
  tracking the chart `appVersion`). The copy under
  `internal/controllers/compositiondefinitions/testdata/manifests/` is test fixture
  only — the runtime templates are mounted from the chart
  ([gotchas](./gotchas.md)).
- `telemetry/` ships an OTel collector config, a Grafana dashboard and the metric
  catalog (`telemetry/metrics-reference.md`).

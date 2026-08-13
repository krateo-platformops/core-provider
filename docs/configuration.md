---
type: Configuration
title: core-provider — configuration
description: The whole config surface — chart values for the engine, chart-inspector and every spawned CDC, the env ConfigMap contract, and the CDC asset templates.
resource: oci://ghcr.io/krateo-platformops/charts/core-provider
tags: [helm-values, env, configmap, cdc]
timestamp: 2026-08-07T00:00:00Z
---

# Configuration

Everything is driven by [`helm/core-provider/values.yaml`](../helm/core-provider/values.yaml)
(fully typed by [`values.schema.json`](../helm/core-provider/values.schema.json) —
comments in `values.yaml` carry the operational rationale and are worth reading
verbatim). The chart configures **three things**: the engine Deployment, the
chart-inspector Deployment, and the asset templates every spawned CDC is rendered
from.

## Engine (top level)

| Value | Default | Notes |
|---|---|---|
| `image.registry/repository/tag` | `ghcr.io` / `krateo-platformops/core-provider` / `""` | Empty tag → chart `appVersion`. |
| `global.imageRegistry` | `""` | Replaces the registry **host** of every image in the chart (engine, chart-inspector, CDC) — one value for mirrored / air-gapped installs; repository paths are preserved. |
| `resources` | requests `500m`/`512Mi`, limit mem `2Gi`, **no CPU limit** | Cold start reconciles every `CompositionDefinition` at once; <2Gi OOM-kills on a real umbrella, and a CPU limit throttles the reconcile loop to zero progress (measured — see the values.yaml comment). |
| `livenessProbe` / `readinessProbe` | TCP `:8080` | The binary has no `/healthz` yet; the always-bound metrics port is probed. |
| `leaderElection.enabled` | `false` | REQUIRED at `replicaCount>=2` or replicas double-reconcile; grants Lease RBAC when on. |
| `podDisruptionBudget` / `topologySpreadConstraints` | off / `[]` | Rendered only when meaningful (`replicaCount>1`). |
| `terminationGracePeriodSeconds` | `45` | Must exceed the manager's ~30s graceful drain. |
| `env` | `CORE_PROVIDER_DEBUG: "false"` | Extra engine env, rendered into the env ConfigMap. |

## OpenTelemetry (`otel.*`) — engine AND every CDC

Default **off**: with `otel.enabled=false` the deployments emit no `OTEL_*` env and
are byte-identical (no churn). `otel.enabled=true` streams metrics+traces
(`otel.tracing`) to `otel.endpoint` — empty endpoint defaults to the node-local
daemonset collector in the release namespace. Logs are always-on structured JSON
regardless. Metric catalog:
[`go/core-provider/telemetry/metrics-reference.md`](../go/core-provider/telemetry/metrics-reference.md).

## `apiRefRBAC.*` — authorization for `apiRef` status projection

On by default; **requires** authn's serviceaccount strategy (CRD
`serviceaccount.authn.krateo.io`) and snowplow — disable on clusters without them.

| Value | Default | Notes |
|---|---|---|
| `apiRefRBAC.enabled` | `true` | Auto-generate the RBAC that authorizes a definition's `apiRef` RESTAction reads (snowplow `GET /rbac` read-set). |
| `apiRefRBAC.snowplowUrl` / `authnUrl` | `""` | Empty derives `http://snowplow.<ns>.svc.cluster.local:8081` / `http://authn.<ns>...:8082` at render time. |
| `apiRefRBAC.authnNamespace` | `""` (release ns) | Where allowlist mappings are created (where authn watches) — becomes `COMPOSITION_AUTHN_NAMESPACE`. |
| `apiRefRBAC.group` | `krateo:core-provider` | The engine's own nominal group. |
| `apiRefRBAC.tokenAudience` / `tokenExpirationSeconds` | `authn` / `3600` | The projected-token contract. |

The engine's own allowlist mapping is provisioned **at runtime** (first `apiRef`
use), not as a chart CR — the authn CRD is installed later in a platform bootstrap.

## `chartInspector.*`

Its own image (`krateo-platformops/chart-inspector`, tag tracks `appVersion`),
replicas/HPA/PDB, probes (**must** stay on `:8081` — the server listens there; `:80`
probes liveness-killed every pod, observed live), `service.port: 8081`, and generous
CPU (`requests: 1`, `limits: 2`) because it renders whole umbrellas inside
core-provider's ~30s `/rbac` timeout. `env.HOME=/tmp` works around Helm's
writable-home requirement.

## `cdc.*` — defaults for every spawned composition controller

| Value | Default | Notes |
|---|---|---|
| `cdc.image.tag` | `""` | **Tracks the chart `appVersion`** — every release ships the matching CDC build (a literal pin previously let a CDC fix ship without deploying, see values.yaml history note). Set explicitly only to hold the CDC back. |
| `cdc.workers` / `cdc.resyncInterval` | unset | Optional fleet-wide floor for reconcile concurrency / resync; a definition's `spec.controller` still overrides per Kind. |
| `cdc.env` | `COMPOSITION_CONTROLLER_WORKERS: "10"`, `..._RESYNC_INTERVAL: "60s"`, `..._GRACEFUL_SHUTDOWN_TIMEOUT: "30s"`, `HOME: /tmp` | The 60s resync makes umbrella self-bootstrap advance in minutes; the drain window must stay below `cdc.terminationGracePeriodSeconds` (45). |
| `cdc.resources` | requests `50m`/`64Mi` | One Deployment per composition Kind runs concurrently — keep the per-controller footprint small. |
| `cdc.metrics.enabled` | `false` | No port exposed by default, hence no probes. |

Per-definition override: `CompositionDefinition.spec.controller`
(`workers`, `resyncInterval`) tunes ONE Kind's CDC without touching the fleet
([api](./api.md)).

## The env ConfigMap contract

The engine reads env from a ConfigMap rendered by
[`templates/configmap.yaml`](../helm/core-provider/templates/configmap.yaml):
`OTEL_*` (when enabled: `OTEL_ENABLED`, `OTEL_TRACING_ENABLED`, `OTEL_LOGS_ENABLED`,
`OTEL_EXPORT_INTERVAL`, exporter endpoint/protocol), everything under `.Values.env`
(all engine flags are env-overridable with the `CORE_PROVIDER_` prefix: `_DEBUG`,
`_SYNC` cache re-list default 10m — deliberately short so a missed watch event
self-heals without a manual restart, see the `main.go` comment — `_POLL_INTERVAL`
drift poll default 3m, `_MAX_RECONCILE_RATE` default 5, `_LEADER_ELECTION`,
`_MIN/_MAX_ERROR_RETRY_INTERVAL`), `CORE_PROVIDER_LEADER_ELECTION` (when HA), and the
`apiRefRBAC` block: `CORE_PROVIDER_SNOWPLOW_URL`, `CORE_PROVIDER_AUTHN_URL`,
`CORE_PROVIDER_APIREF_SELF_SA_NAME/_NAMESPACE`, `CORE_PROVIDER_APIREF_GROUP`,
`COMPOSITION_AUTHN_NAMESPACE`.

## The CDC asset templates (chart-owned, engine-consumed)

The engine renders every CDC from **template files mounted by this chart** into its
pod at `/tmp/assets/{cdc-deployment,cdc-configmap,cdc-rbac,json-schema-configmap,cdc-service}`
(sources under [`helm/core-provider/assets/`](../helm/core-provider/assets)). They are
not embedded in the binary: changing them re-digests and re-rolls **every** CDC on
the next reconcile — treat asset edits as a fleet-wide reconcile event
([gotchas](./internals/gotchas.md)).

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
| `leaderElection.enabled` | `true` | One reconciling replica. A rolling update overlaps the old and new pod even at `replicaCount: 1`, and without the Lease both reconcile and restart every cdc after each other (#382). Required at `replicaCount>=2`; grants Lease RBAC when on. |
| `podDisruptionBudget` / `topologySpreadConstraints` | off / `[]` | Rendered only when meaningful (`replicaCount>1`). |
| `terminationGracePeriodSeconds` | `45` | Must exceed the manager's ~30s graceful drain. |
| `strategy` | `{}` | The engine Deployment's strategy. Empty: not rendered, so Kubernetes' default applies (the new pod starts beside the old one, which needs room for one more pod). `rollingUpdate: {maxSurge: 0, maxUnavailable: 1}` replaces the pod in place, which fits a full pod quota; a new pod that cannot start then leaves no engine running. |
| `priorityClassName` | `""` | PriorityClass of the engine's pod. Empty: not rendered. The class must exist. It does not bypass a `ResourceQuota`. |
| `env` | `CORE_PROVIDER_DEBUG: "false"` | Extra engine env, rendered into the env ConfigMap. |

## OpenTelemetry (`otel.*`) — engine AND every CDC

Default **off**: with `otel.enabled=false` the deployments emit no `OTEL_*` env and
are byte-identical (no churn). `otel.enabled=true` streams metrics+traces
(`otel.tracing`) to `otel.endpoint` — empty endpoint defaults to the node-local
daemonset collector in the release namespace. Logs are always-on structured JSON on
stderr regardless; `otel.logs=true` (default false, needs `otel.enabled`) also exports them over
OTLP, for core-provider and every CDC alike (`OTEL_LOGS_ENABLED`). Controller **audit records**
travel only as OTLP logs, so with `otel.logs` off no audit record reaches `krateo_audit` (#469). An
explicit `OTEL_LOGS_ENABLED` under `env` or `cdc.env` still wins for that one process. Metric catalog:
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

The engine's own allowlist mapping is provisioned **at runtime**, not as a chart CR — the
authn CRD is installed later in a platform bootstrap. Every reconcile of a definition with
an `apiRef` ensures it: until the CRD is there the definition reads
`AuthnMappingApplied=False/AuthnNotInstalled`, and a mapping deleted later is written again.

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
| `cdc.resources` | requests `50m`/`128Mi`, limits `512Mi` (no CPU limit) | One Deployment per composition Kind runs concurrently. `64Mi` was too low, but the 200-239Mi fleet figures in #99 were inflated by the helm-client goroutine leak (fixed separately) — sizing from them would over-reserve. The clean control (`builderpublishes`, zero compositions, never reconciles) idles at `37Mi`, so `128Mi` covers the floor plus real reconcile working room. Limits are not reserved, so `512Mi` is free headroom for a large chart render. No CPU limit — a limit on a bursty reconcile loop throttles it to zero progress (same measured finding as the engine). **Re-derive from post-fix measurements.** |
| `cdc.metrics.enabled` / `cdc.metrics.port` | `false` / `8080` | No metrics port exposed by default. Readiness is served separately on `cdc.healthProbe.port`, so probes do not depend on this. |
| `cdc.ownershipLease.duration` / `.renewDeadline` / `.retryPeriod` | `30s` / `20s` / `5s` | The ownership Lease every composition controller holds while it reconciles (#660), client-go's leaderelection timings. ONE value for both binaries: rendered under the same env names (`COMPOSITION_CONTROLLER_OWNERSHIP_LEASE_DURATION`, `_RENEW_DEADLINE`, `_RETRY_PERIOD`) into every controller's ConfigMap and into core-provider's, which measures every wait on these Leases by them (pool member freshness and its grace G = duration + 2.5 x retryPeriod, 42.5s; a controller leaving a pool). **Write load at rest:** one Lease PUT per controller per `retryPeriod`, 0.2/s at 5s (was 0.5/s at unstructured-runtime's 2s: ~25/s for 50 controllers, the largest write load of a SQLite tenant at rest). **Cost:** a pod that dies without releasing its Lease and is replaced by a differently named pod is waited for up to `duration` + 2.2 x `retryPeriod` (41s, was 19.4s: the replacement measures expiry from its own first read of the Lease and notices it at its next jittered attempt; G, 42.5s, still covers it); on a graceful rollout the Lease is released at once and the new pod takes it at its next attempt, within 2.2 x `retryPeriod` (11s, was 4.4s); an in-pod restart reacquires at once. **Benefit besides the write cut:** `renewDeadline` 20s (was 10s) doubles how long a holder tolerates slow renewals before it gives the Lease up and exits, which on a CPU-starved SQLite tenant control plane is the difference between a slow API server and a controller restart loop. Required as a set, `duration > renewDeadline > retryPeriod x 1.2`, all > 0: the chart refuses to render, and both binaries to start, otherwise. Setting the keys in `cdc.env` or `env` is refused. |

Per-definition override: `CompositionDefinition.spec.controller`
(`workers`, `resyncInterval`) tunes ONE Kind's CDC without touching the fleet
([api](./api.md)).

### CPU-saturation gating (opt-in, off by default)

Two `cdc.env` variables let a composition controller slow ITSELF down when its own CPU is saturated
(unstructured-runtime v1.12.0, #66). Nothing changes unless the first is set:

| Env var | Default | Meaning |
|---|---|---|
| `COMPOSITION_CONTROLLER_SATURATION_ENABLED` | `false` | Bound reconciles by the container's cgroup v2 `cpu.pressure`: the share of the last 10s window in which some task waited for CPU, from its cumulative "some total" counter. Local capacity starts at 1 and ramps +1 per 2s to `workers`. Pressure over the threshold for a whole 10s window halves it (minimum 1, at most once per window), and it then ramps +1 per 2s again (unstructured-runtime v1.16.0, #82). |
| `COMPOSITION_CONTROLLER_CPU_PRESSURE_THRESHOLD` | `0` (runtime default 50, provisional) | Percentage of the 10s window over which (strictly greater) the controller counts as saturated. Out of range (outside 0-100) fails startup. |

Low capacity behaves like fewer workers: a worker takes a slot before it takes work, so the queue's
order is the order work runs in, and a spec change goes ahead of a resync. Nothing is written on the
object, because the controller's own saturation is not a state of the object. With the default
`cdc.resources` (no CPU limit), `cpu.pressure` reflects contention on the node, not a quota.

**Measured cost** (unstructured-runtime#66 harness, GKE single node, one cdc at a 250m CPU limit, 64
compositions, threshold 50 against off, n=3 per arm, alternated). Raw records are on the branch
`test/66-saturation-measurement-harness`, under `hack/saturation-measure/results/`.

| | rule off | threshold 50 | cost |
|---|---|---|---|
| **core-provider 2.13.106** (the current rule, unstructured-runtime v1.16.0+, #82): steady | 400.2 s | 490.8 s | **1.23x** |
| 2.13.106: restart into a backlog | 380.8 s | 474.6 s | **1.25x** |
| 2.13.73 (the rule before #82): steady | 384 s | 544 s | 1.42x |
| 2.13.73: restart into a backlog | 344 s | 524 s | 1.52x |

#82 replaced a cut on every 1s sample of "some avg10" (a single stall kept avg10 high for several
samples, so it cut several times) with one cut per 10s window. On 2.13.106 the rule engaged: local
capacity averaged 2.8, and the container was throttled for 92% of the run, against 98.6% with the
rule off. It holds pressure less tightly than the old rule did: peak avg10 PSI was 67.5 (old rule:
50-58) and ~2% of the quota went unused (old rule: 7-10%). That is n=3 per arm, on one node, one load
shape and one limit.

**The chart default stays off.** Diego decided on 2026-11-16, with these numbers in front of him, to
enable the rule for every composition controller on the Krateo platform. That is set in the
installer chart's **core-provider subchart values** (the top-level `core-provider:` block, installer
#997), at threshold 50. It is not a change to this chart's default, and not an Installer CR
`componentValues` entry: core-provider is a subchart there, not a pinned component, and a key set
under `componentValues.core-provider` would be pruned silently. On
krateo-057 no controller ran more than one reconcile at a time over 24h (`in_flight_max <= 1`,
docs/design/cdc-pooling.md §5b), and the rule's floor is 1, so the enablement is expected to change
nothing measurable there today. It is measured before and after the roll all the same.

With `otel.enabled=true`, `unstructured_runtime.ratelimit.local.capacity` and `.saturation.input`
show what the rule is doing. See the
[metrics reference](../go/composition-dynamic-controller/telemetry/metrics-reference.md).

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

Each of the five ConfigMaps (`<fullname>-cdc-deployment-`, `-cdc-configmap-`, `-rbac-configmap-`,
`-jsonschema-configmap-`, `-service-configmap-`) is **immutable and named by the digest of its own
data**, and the engine Deployment's volumes name it (#651). So an engine pod reads the templates it was
deployed with for as long as it lives: during an upgrade, a rollback or a values change the old pod keeps
its own, and the new templates take effect when the new pod takes over. Their names change with their
content, so find one by prefix or through the Deployment's volumes, and don't reference one by name from
outside the chart. While a roll is stuck, the old pod reports `FailedMount ... configmap "..." not found`:
helm has deleted the previous ConfigMaps and the kubelet keeps the files the pod already has. That is
expected, and ends with the old pod; a container restart in it keeps them too.

If that old pod is **deleted** while the roll is stuck, its ReplicaSet replaces it, and the replacement
renders the same templates or none, never the new ones. On the same node it normally starts: the kubelet
stops watching an immutable ConfigMap once it has read it and keeps it for as long as any pod on the node
names it, and the replacement arrives while the old pod is still terminating. It can't mount them
(`FailedMount`), and no engine runs until the new pod starts, when nothing on its node still holds them: on
another node, after a kubelet restart or a node reboot, if the old pod was already gone, or when the previous
chart was from before #651, whose ConfigMaps aren't immutable and so aren't kept. After a kubelet restart the
old pod itself goes on running, but a container that exits in it may not be started again. `kubectl rollout
undo` is subject to the same rule, so it normally can't mount either: use `helm rollback`, which writes the
ConfigMaps back.

The per-definition `<plural>-<version>-jsonschema-configmap` is a different object, written by the engine;
its name does not change.

---
type: Architecture
title: core-provider — what the CDC emits (events and logs)
description: Every Kubernetes Event and log line a composition-dynamic-controller instance produces, traced to the code that emits it across three modules, plus the ClickHouse predicates that select them. Written for alert authoring — each predicate was executed against a live cluster before being written here.
resource: ghcr.io/krateo-platformops/composition-dynamic-controller
tags: [internals, cdc, events, logs, observability, alerting, clickhouse]
timestamp: 2026-09-22T00:00:00Z
---

# What the CDC emits

One `composition-dynamic-controller` (CDC) Deployment exists per CompositionDefinition
version: core-provider renders it when the CompositionDefinition reconciles, and it watches
the one generated Kind. It is an ordinary Kubernetes controller, so it says what it is doing
on two channels — **Kubernetes Events** on the composition CR, and **log lines** on stderr —
and both reach ClickHouse as rows in `default.otel_logs`.

This file exists because writing a correct alert on those rows needs three things that are not
visible from the rows themselves: which module actually emits a given line, which fields are
constant across every CDC instance, and which of them the CDC shares with other Krateo
controllers.

Traced against `go/composition-dynamic-controller/` at engine 2.13.x. Counts were measured on a
live cluster (krateo-057, 88.6M `otel_logs` rows, 7-day window) and are illustrative of shape and
order of magnitude, not of your cluster.

## The three modules that emit

| module | version | what it emits |
|---|---|---|
| `go/composition-dynamic-controller` | engine 2.13.x | the composition lifecycle — helm install/upgrade/uninstall, child teardown, graceful pause. 11 `Event` calls in `internal/composition/composition.go`. |
| `unstructured-runtime` | v1.4.0 | the generic reconcile loop (`pkg/controller/worker.go`). **Most Warning events come from here**, not from the CDC's own code. It also owns the JSON log handler (`pkg/logging/jsonhandler.go`). |
| `plumbing` | v1.14.3 | `kubeutil/event` builds the Event; `kubeutil/eventrecorder` creates the recorder. |

All three are in the repo-mcp-server corpus, so each claim below can be read at its source.

## Identity: there is no single "CDC" to filter on

Every CDC instance is a separate Deployment named `<plural>-v<major>-<minor>-<patch>-controller`
(`alerttroubleshooters-v0-2-42-controller`, `awsrdsstacks-v0-3-0-controller`). 44 of them ran on
krateo-057. The OTel `ServiceName` column therefore holds a **different value per instance**, and
so do `k8s.container.name` and `k8s.deployment.name`. None of them select "the CDC".

Two fields are constant across every instance:

| channel | discriminator | why it is stable |
|---|---|---|
| logs | `JSONExtractString(Body, 'service.name') = 'composition-dynamic-controller'` | `serviceName` is a package constant (`main.go:49`) and the default of `--otel-service-name`; it is written into every structured line. Measured: one value across 74 distinct instances. Survives a dev build or a mirrored registry. |
| events | `JSONExtractString(Body, 'object', 'reportingComponent') = 'composition-dynamic-controller'` | passed to `eventrecorder.Create(ctx, cfg, "composition-dynamic-controller", nil)` in `main.go:242`. |

`ResourceAttributes['container.image.name'] = 'ghcr.io/krateo-platformops/composition-dynamic-controller'`
also works for logs and additionally catches the non-JSON lines (below), at the cost of breaking if
the image is pulled from a mirror or a dev registry.

## Kubernetes Events

### How one is built

`plumbing/kubeutil/event` constructs it — `Normal(reason, action, message)` or
`Warning(reason, action, err)`, where a Warning's message is `err.Error()` — and the recorder
calls `kube.Eventf(obj, related, type, reason, action, message)`. That is the **`events.k8s.io/v1`**
recorder, so `action`, `reportingComponent` and `reportingInstance` are all populated, which a
`core/v1` Event would leave empty.

In `otel_logs` an Event arrives under `ResourceAttributes['telemetry.source'] = 'k8s-events'` with
the watch object as JSON in `Body`:

```
object.reason              CompositionUpdated | CannotCreateExternalResource | …
object.action              Update | CreateExternalResource | …
object.type                Normal | Warning
object.message             the human-readable note (a Warning's is the error text)
object.reportingComponent  composition-dynamic-controller
object.reportingInstance   composition-dynamic-controller-<deployment>-<pod-suffix>
object.involvedObject      { apiVersion: composition.krateo.io/v<X-Y-Z>, kind, name, namespace }
```

`involvedObject.apiVersion` is always under `composition.krateo.io`, and `involvedObject.kind` is
the composition Kind — which is how an alert scopes to one blueprint.

### The CDC's own reasons

Emitted from `internal/composition/composition.go`; `action` here is a plain verb string rather
than one of the framework's action constants.

| reason | type | action | raised when |
|---|---|---|---|
| `CompositionCreated` | Normal | `Create` | the helm release was installed |
| `CompositionUpdated` | Normal | `Update` | the release was upgraded |
| `CompositionDeleted` | Normal | `Delete` | the release was uninstalled, was already absent, or was handed over to a new GVK version |
| `ChildTeardownAbandoned` | **Warning** | `Delete` | finalizer-bearing children outlived `COMPOSITION_CONTROLLER_CHILD_TEARDOWN_GRACE` (default 5m) and the composition uninstalled anyway — external resources may be left behind |
| `ReconciliationGracefullyPaused` | Normal | `Observe`/`Update`/`Delete` | the graceful-pause annotation is set |

`ChildTeardownAbandoned` is the one Warning the CDC raises in its own right, and it reports a
consequence that nothing else will tell you about.

### The framework's reasons — shared, not CDC-specific

`unstructured-runtime/pkg/controller/worker.go` raises these. Every Krateo controller built on
that runtime raises the **same strings**.

| reason | type | typical action |
|---|---|---|
| `CannotObserveExternalResource` | Warning | `ObserveExternalResource` |
| `CannotCreateExternalResource` | Warning | `CreateExternalResource` |
| `CannotUpdateExternalResource` | Warning | `UpdateEvent` |
| `CannotDeleteExternalResource` | Warning | `DeleteExternalResource` |
| `CannotUpdateManagedResource` | Warning | `UpdateManagedResource` |
| `CannotSetConditions` | Warning | `UpdateManagedResource` |
| `CannotFetchManagedResource` | Warning | `FetchManagedResource` |
| `CannotInitializeManagedResource` | Warning | `ProcessEvent` |
| `CannotResolveResourceReferences` | Warning | `ProcessEvent` |
| `CannotPublishConnectionDetails` / `CannotUnpublishConnectionDetails` | Warning | — |
| `ReconciliationPaused` | Normal | `ProcessEvent` |
| `CompositionCreated` | Normal | `CreateEvent` |

**Pair every one of these with `reportingComponent`.** Measured over 7 days, a single reason spans
four controllers:

| `CannotObserveExternalResource` reported by | rows |
|---|---|
| `rest-dynamic-controller` | 22,140 |
| `composition-dynamic-controller` | 689 |
| `managed/compositiondefinition.core.krateo.io` (core-provider) | 624 |
| `managed/localresource.git.krateo.io` (git-provider) | 70 |

An alert on the bare reason is therefore ~97% *not* the CDC. The default
`sre-krateo-composition-reconcile-error` in the observability chart matches the bare reason and has
this property: it is a useful platform-wide reconcile-error signal, and it is not a CDC alert.

`reportingComponent` values follow two shapes: a plain component name for controllers on
unstructured-runtime (`composition-dynamic-controller`, `rest-dynamic-controller`), and
`managed/<kind>.<group>` for controllers on provider-runtime.

## Logs

### The handler

`main.go:176` builds
`logging.NewLogrLogger(logr.FromSlogHandler(logging.NewOTelHandler(level, os.Stderr, *otelServiceName)))`,
whose handler lives in `unstructured-runtime/pkg/logging/jsonhandler.go`. It writes **JSON on
stderr in the OTel log model**: `timestamp` as RFC3339Nano, the slog level as `level`, a sibling
`SeverityNumber`, and `service.name` (plus a legacy `service`) from `pkg/logging/otelbridge.go`.
`--debug` moves the floor from INFO to DEBUG.

A reconcile line carries, from `contextutils`:

```json
{"timestamp":"2026-09-22T08:34:16.656091041Z","level":"INFO",
 "msg":"External resource is up to date","service.name":"composition-dynamic-controller",
 "kind":"AlertTroubleshooter","apiVersion":"composition.krateo.io/v0-2-42",
 "name":"alert-troubleshooter","namespace":"krateo-system",
 "event":"observe","queuedAt":"2026-09-22T08:34:14.776802565Z",
 "traceId":"U-qNr4XDg","SeverityNumber":9}
```

`event` is the reconcile phase — `observe`, `create`, `update`, `delete` — and `kind` /
`namespace` / `name` identify the composition being reconciled.

### The severity is inside `Body`, not in the column

The OTel `SeverityText` and `SeverityNumber` **columns** are empty: measured 0 non-empty of
88,620,936 rows. The collector stores the controller's JSON as raw text in `Body` rather than
parsing it into the OTel fields, so severity is reached with
`JSONExtractString(Body, 'level') = 'ERROR'`.

### Four line shapes share one container

A CDC pod's stderr is not homogeneous. Over 7 days, 1,645,873 rows from CDC containers split as:

| shape | rows | example | comes from |
|---|---|---|---|
| structured JSON (above) | 551,384 | `{"timestamp":…,"level":"ERROR","msg":"Cannot create external resource",…}` | the controller, via unstructured-runtime's handler |
| Go stdlib `log` | 872,212 | `2026/09/22 08:34:43 INFO CDC Metrics Status initialized=false deploymentName=""` | the metrics heartbeat, and Helm's own `warning:` lines |
| klog (client-go) | 222,291 | `I0922 …] "Warning: unknown field \"spec.resources\""`, `E0922 …] "Unhandled Error" err="…"` | apimachinery, discovery cache, reflectors |
| Helm engine | (within the stdlib count) | `warning: cannot overwrite table with non table for …` | the Helm render |

The consequence for alerting: a `Body` substring match on `error` sweeps in klog's
`E0922 … "Unhandled Error"` and the field-validation warnings, which are steady background on a
healthy cluster. `JSONExtractString(Body, 'level') = 'ERROR'` selects only what the controller
itself decided was an error.

### The message vocabulary

`msg` is a fixed string per call site, so it is safe to match exactly. ERROR, over 7 days:

| `msg` | rows |
|---|---|
| `Cannot create external resource` | 34,400 |
| `Cannot observe external resource` | 8,740 |
| `Cannot fetch managed resource` | 64 |
| `Running controller.` | 23 |
| `Cannot reconcile external-create annotations after incomplete create` | 8 |
| `Cannot update external resource` | 3 |
| `Cannot update status` | 2 |

INFO, over 2 days — the lifecycle narration:

| `msg` | rows | meaning |
|---|---|---|
| `External resource is up to date` | 152,562 | the steady state: an observe found no drift |
| `Successfully requested update of external resource` | 3,251 | a helm upgrade was issued |
| `Successfully requested creation of external resource` | 12 | a helm install was issued |
| `Successfully requested deletion of external resource` | 34 | a helm uninstall was issued |
| `GVK version-migration handover detected; skipping uninstall so the new-version controller upgrades the release in place` | 34 | version migration, not a deletion |
| `Labels do not match composition definition` | 29 | the CR was skipped by this instance |
| `Starting controller` / `Controller ready.` / `Starting workers: 10` | 157/149/149 | startup |
| `Stopping controller; draining in-flight reconciles` / `All workers drained cleanly` / `Shutting down worker` | 149/139/1,480 | shutdown |
| `starting composition-dynamic-controller` | 157 | carries `version` and `commit` of the build |

A restart is visible either as `starting composition-dynamic-controller` appearing, or as the
startup/shutdown pair; `Shutting down worker` fires once per worker (10 by default), so it counts
workers, not restarts.

## Selecting these rows in ClickHouse

Every predicate below was executed against krateo-057 before being written here; the count is what
it returned over 7 days. A zero means the condition did not occur in the window — it still proves
the clause parses and names real columns.

### Base scopes

```sql
-- CDC events
ResourceAttributes['telemetry.source'] = 'k8s-events'
  AND JSONExtractString(Body, 'object', 'reportingComponent') = 'composition-dynamic-controller'
-- 30,964

-- CDC structured logs
JSONExtractString(Body, 'service.name') = 'composition-dynamic-controller'
-- 551,384
```

### Verified predicates

| intent | predicate (appended to the matching base scope) | rows/7d |
|---|---|---|
| any CDC failure | `AND JSONExtractString(Body, 'object', 'type') = 'Warning'` | 2,880 |
| a composition will not install | `AND JSONExtractString(Body, 'object', 'reason') = 'CannotCreateExternalResource'` | 2,177 |
| a composition cannot be read back | `AND JSONExtractString(Body, 'object', 'reason') = 'CannotObserveExternalResource'` | 689 |
| children abandoned on delete | `AND JSONExtractString(Body, 'object', 'reason') = 'ChildTeardownAbandoned'` | 0 |
| scope to a namespace | `AND JSONExtractString(Body, 'object', 'involvedObject', 'namespace') = 'krateo-system'` | 28,775 |
| scope to one blueprint | `AND JSONExtractString(Body, 'object', 'involvedObject', 'kind') = '<Kind>'` | — |
| controller-level errors | `AND JSONExtractString(Body, 'level') = 'ERROR'` | 43,255 |
| one error class | `AND JSONExtractString(Body, 'msg') = 'Cannot create external resource'` | 34,415 |
| one reconcile phase | `AND JSONExtractString(Body, 'event') = 'observe'` | 493,363 |
| scope logs to one blueprint | `AND JSONExtractString(Body, 'kind') = '<Kind>'` | 10,094 for `AlertTroubleshooter` |

### Choosing a channel

Events are the better default. They are already scoped to the CDC by `reportingComponent`, they name
the composition in `involvedObject`, and they are far fewer — 30,964 against 1.6M log rows over the
same week — so a threshold over them is stable. Reach for the logs when the signal has no Event
behind it: the lifecycle `msg` strings, the reconcile phase, the startup/shutdown narration, and the
build stamp.

### Thresholds

`observe` runs continuously against every composition, so anything counting observes is a
high-rate signal and wants a threshold measured against the cluster's own baseline. The failure
reasons are the opposite: `ChildTeardownAbandoned` returned 0 over a week, and `above 1` is the
right reading of "this happened at all". Measure before choosing, with the same `count()` over a
7-day window used to build this table.

## Related

- `docs/internals/behavior.md` — the reconcile lifecycle these emissions narrate.
- `docs/internals/gotchas.md` — runtime pitfalls.
- `clickstack-chart/charts/krateo-observability/values.yaml` + `docs/alerts.md` — the default
  `sre-*` alert set, and the three rules that decide whether any Alert can fire.

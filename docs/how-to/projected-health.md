---
type: Usage
title: "How-to: a chart's own health verdict (projected health)"
description: How a CompositionDefinition's statusDataTemplate projects .status.health so a composition stays not-ready (Creating) until the chart says it is complete, how to withdraw the verdict with {}, and what it can and cannot override.
resource: compositiondefinitions.core.krateo.io
tags: [how-to, health, ready, statusDataTemplate]
timestamp: 2026-11-14T00:00:00Z
---

# A chart's own health verdict (projected health)

The composition-dynamic-controller (cdc) decides a composition's `Ready` condition from the children
its chart rendered (`.status.managed`). That rollup can only judge what was emitted. A chart that
emits its children in stages (one dependency tier per render, for example) looks "all healthy"
between renders while later tiers are still to come.

A chart can give its own verdict instead, by projecting `.status.health`. This is
krateo-core-provider#234, read by `readProjectedHealth` in
`go/composition-dynamic-controller/internal/composition/support.go` and folded in by
`resolveReadyVerdict` in `childhealth.go`.

## The fields

| Field | Values | Meaning |
|---|---|---|
| `status.health.ready` | bool, or the string `"true"`/`"false"` (case-insensitive) | the author's verdict. **Absent** means the author has no opinion. |
| `status.health.reason` | only `Creating` is recognised (case-insensitive) | with `ready: false`: `Creating` = still converging, anything else or absent = `Unavailable` (broken) |
| `status.health.message` | free text | stamped on `Ready` |

## What it decides

- **`ready: false`, `reason: Creating`** → `Ready=False` / `Creating`, with your message. A parent
  composition reads that child as converging, not failed.
- **`ready: false`, no or another reason** → `Ready=False` / `Unavailable`. A misspelled `Creating`
  lands here on purpose: the loud direction.
- **A child the rollup positively observed as FAILED still wins** over a projected `ready: false`:
  `Unavailable`, with your message first and `also unhealthy: …` after it.
- **`ready: true` is not neutral.** It cannot override an observed sick child. But when the rollup
  has no evidence (`ZeroEvidence`, `InventoryNotRecorded`, `ChildrenForbidden`), it is honoured as
  evidence and the composition becomes `Ready=True` / `Available`. Project `ready: true` only when
  your expression really knows the composition is healthy.

## Withdrawing the verdict: project `{}`

When the chart has nothing to say, project an object **without** a `ready` key, e.g. `{}`:

- `statusprojection.Project` writes the mapping's value over `.status.health` as a whole, so the
  previous `{ready: false, …}` is removed, and with no `ready` key the generic rollup decides.
- **Do not produce no output** (an empty jq result). An expression with no output leaves the field
  untouched ("leave the field unset rather than writing it", unstructured-runtime
  `pkg/tools/statusprojection`). The composition then keeps its last `{ready: false, …}` forever.
- **Do not project `{ready: true}`** to mean "done": see above, it hides a composition whose children
  could not be read.

Measured end to end on a real cdc (core-provider 2.13.94, openstack-as-a-service blueprint 0.3.55,
32-component cold install, 2026-11-13). The umbrella was `Ready=False` / `Creating` while the chart
held components back (32 pending down to 1). `status.health` then became exactly `{}`, and `Ready=True`
followed the rollup 50 s after the last component went Ready. It was never Ready while anything
was pending.

## What the expression can read

`spec.statusDataTemplate` entries are `{ forPath, expression }`, with `${ jq }` evaluated over one root
(cdc `setStatus` → `statusprojection.Project`; unstructured-runtime v1.16.0, unchanged from v1.15.0):

| Key | Content | Needs snowplow |
|---|---|---|
| `.self` | the whole composition object | no |
| `.spec`, `.status` | sugar for `.self.spec`, `.self.status` | no |
| `.helm` | `{url, version, status, revision, name}` of the release | no |
| `.api` | the `spec.apiRef` RESTAction, resolved through snowplow | **yes**; absent without it, and mappings that use it are skipped |

`.status.managed[]` items have exactly `{apiVersion, resource, name, namespace, path}` (no `kind`).
`.spec` holds what the CR carries, including defaults the apiserver applied from the generated
CRD's schema. A `values.yaml` default that is not a default in `values.schema.json` is not visible.

**Order within one reconcile:** `setManagedResources` → `Project` → `deriveReadyFromHealth`. A health
projection computed from `.status.managed` takes effect on the same reconcile that recorded the
inventory. `.status.managed` is rewritten only by the phases that decode a release (install, upgrade,
reconcile); Observe leaves the last recorded one in place.

## Example: hold Ready until every selected component is emitted

The chart emits a marker ConfigMap while anything selected is still held back, and nothing once all
are emitted. The selection logic stays in the chart, in one place. The projection only looks for the
marker:

```yaml
spec:
  statusDataTemplate:
    - forPath: health
      expression: >-
        ${ if ([.status.managed[]? | select(.resource == "configmaps" and .name == "install-pending")] | length) > 0
           then {ready: false, reason: "Creating", message: "selected components not yet emitted (see ConfigMap install-pending)"}
           else {} end }
```

This is the shape openstack-as-a-service shipped (blueprint #257, ConfigMap
`openstack-install-pending`).

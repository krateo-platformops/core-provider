---
type: Example
title: Status projection — staged design manifests
description: The staged manifests accompanying the composition-status-projection design — minimal built-ins, full definition with apiRef, the RESTAction, and an instance.
resource: compositiondefinitions.core.krateo.io
tags: [example, status-projection, design]
timestamp: 2026-08-07T00:00:00Z
---

# Composition status projection — example manifests

Worked manifests for [`../../composition-status-projection.md`](../../composition-status-projection.md).
**Design draft — these reference proposed `spec` fields (`statusDataTemplate`, `apiRef`,
`apiRef.extras`) that do not exist yet.** They illustrate the design; they are not applyable
today.

> **`extras` shape is settled** — copied from snowplow's shipped "inline-extras design P"
> (2026-06-20): `apiRef.extras` is a free-form, preserve-unknown-fields map of **static**
> author values (input-only), nested **inside `apiRef`**. The CDC injects per-instance
> context (`compositionId`/`namespace`/…) as request-extras that merge over inline
> (request-wins).

| File | What |
|---|---|
| `01-phase1-minimal.yaml` | Phase 1 — built-ins only (`.self`/`.helm`); no I/O, no RBAC |
| `02-compositiondefinition.yaml` | Full definition — built-ins + `apiRef` + author-declared `extras` |
| `03-restaction.yaml` | The referenced `RESTAction` + endpoint Secrets (in-cluster kube API + external) |
| `04-composition-instance.yaml` | A `Composition` instance + the resulting `.status` (in comments) |

## Data flow (full example)

1. Author declares static `apiRef.extras` on the CompositionDefinition (e.g.
   `region: eu-west`) — snowplow's inline-extras shape, input-only.
2. The CDC injects per-instance context (`compositionId`, `namespace`, `name`, `spec`) as
   request-extras and asks **snowplow** to resolve the `apiRef` RESTAction **under its own
   ServiceAccount**; snowplow merges request-extras over the inline ones (request-wins).
3. `Extras` seeds the RESTAction's jq root; each `api[]` call's `path`/`payload` reads it
   (label-scoped kube reads + external calls). Responses merge back keyed by call name.
4. The CDC projects the combined root (`.self`, `.helm`, `.api.*`) via `statusDataTemplate`
   `${ jq }` and writes the **Composition's** `.status`. Only the Composition status is
   persisted; the RESTAction result is an ephemeral runtime query.

## Notes

- **No `resourcesRefs`**: the kube API is just an HTTP endpoint, so in-cluster reads are
  RESTAction calls.
- **Credentials live only on snowplow's SA** (the endpoint Secret tokens), never in the CDC.
- **Phase 1** (file `01`) needs none of the apiRef machinery and is independently shippable.

---
type: Architecture
title: core-provider — overview
description: What core-provider does and how it works — the three monorepo components, the CompositionDefinition reconcile flow, the digest contract, local vs remote targets.
resource: oci://ghcr.io/krateo-platformops/charts/core-provider
tags: [kco, architecture, compositiondefinition, cdc, chart-inspector]
timestamp: 2026-08-07T00:00:00Z
---

# Overview

core-provider makes a Helm chart a first-class Kubernetes API. You declare a
`CompositionDefinition` naming a chart (any chart that ships a `values.schema.json`);
the engine:

1. **fetches the chart** and derives a GroupVersionKind from its `Chart.yaml` — group
   is the fixed `composition.krateo.io`, version is `v<chart-version-with-dots-as-dashes>`
   (chart `1.2.3` → `v1-2-3`), kind is the Pascalized chart name
   (`go/core-provider/internal/tools/chart/chart.go`, `ChartGroupVersionKind`);
2. **generates a versioned CRD** from the chart's `values.schema.json` (spec) plus a
   status schema, via `plumbing/crdgen`
   (`internal/tools/crd/generation`);
3. **deploys a per-version composition-dynamic-controller (CDC)** — Deployment,
   config + JSON-schema ConfigMaps, ServiceAccount and least-privilege RBAC computed
   by dry-run-rendering the chart through **chart-inspector**
   (`internal/tools/deploy`).

Instances of the generated CRD are **Compositions**; the CDC drives one Helm release
per instance and projects declared status fields each reconcile.

## The three components (one monorepo, one version line)

| Component | Path | Role |
|---|---|---|
| **core-provider engine** | `go/core-provider/` | The controller manager reconciling `CompositionDefinition`s; also hosts the `KubernetesTarget` reachability controller and the composition-mirror controller. |
| **composition-dynamic-controller (CDC)** | `go/composition-dynamic-controller/` | Spawned per Kind × served version by the engine; reconciles composition instances into Helm releases; evaluates the `statusDataTemplate` projection; generates its own chart RBAC via chart-inspector. |
| **chart-inspector** | `go/chart-inspector/` | Stateless HTTP service: `helm template --server`-renders a chart and returns the concrete resources an install touches, so RBAC can be generated instead of hand-authored. |

All three images and the `helm/core-provider` chart release from a single tag; the
chart wires the CDC and chart-inspector image tags to its `appVersion`, so a release
is internally consistent by construction ([release](./release.md)).

## The engine's controllers

`go/core-provider/main.go` starts a controller-runtime manager (metrics on `:8080`,
priority work queue, **no webhook server** — see below) hosting three controllers:

- **compositiondefinitions** (`internal/controllers/compositiondefinitions/`) — the
  main reconciler; Observe/Create/Update/Delete detailed in
  [internals/behavior.md](./internals/behavior.md). It also watches **Secrets** and
  **KubernetesTargets**, so a rotated chart credential or repointed kubeconfig
  re-reconciles the affected definitions promptly.
- **kubernetestargets** (`internal/controllers/kubernetestargets/`) — periodically
  probes each `KubernetesTarget`'s API server and records reachability
  (`status.connectionStatus` Healthy/Down), the target's Kubernetes version, and the
  kubeconfig Secret `resourceVersion` used — a registered, observable spoke.
- **compositionmirror** (`internal/controllers/compositionmirror/`) — for
  remote-targeted definitions, mirrors hub `Composition` specs down to the spoke and
  spoke status back up (hub wins on drift; set-difference GC), so a remote
  composition is authored exactly like a local one
  ([design](./design/remote-composition-mirror.md)).

## The idempotency / digest contract

`status.digest` is an order-stable FNV hash over every rendered CDC object. Observe
computes the *would-render* digest (server-dry-run Deploy) **and** the *live* digest
(Lookup) and compares both against `status.digest`; only a real difference triggers
Create/Update. A reconcile that changes nothing is a no-op — this is what keeps the
engine from churning running controllers fleet-wide
([internals/behavior.md](./internals/behavior.md#the-idempotency--digest-contract)).

## No admission webhooks (since 2.0.0)

The manager hosts no webhook server and no serving certificate. Generated CRDs use
`None` conversion, and the per-object `krateo.io/composition-version` label is stamped
in-apiserver by a **MutatingAdmissionPolicy** (GA `admissionregistration.k8s.io/v1`,
hence the Kubernetes >= 1.36 floor). The management chart ships the policy for local
targets; for **remote targets the engine projects the policy itself** during seeding
(`internal/tools/policy`).

## Local vs remote targets

Without `spec.deploy.targetRef` everything lands on the management cluster. With a
`targetRef` (a namespaced `KubernetesTarget` resolved in the definition's own
namespace → kubeconfig Secret), the generated CRD, RBAC and CDC are **projected onto
the spoke**, along with everything the spoke-local CDC needs to be self-sufficient:
chart-inspector, the MutatingAdmissionPolicy, and a shadow definition
(`internal/tools/deploy/remoteseed.go`, `SeedRemoteTarget`). The
`CompositionDefinition`, its secrets and its status always stay on the hub. Steady
state is spoke-local: the projected CDC installs charts under the spoke's own
ServiceAccount.

## Status projection (since 2.3.0)

A definition may declare `spec.statusDataTemplate` (snowplow-style `${ jq }` mappings
written under `.status`) and `spec.apiRef` (a RESTAction resolved via snowplow whose
result is exposed as the `.api` source). The engine widens the generated CRD's status
schema and ships the config to the CDC; the CDC evaluates the projection each
reconcile. With `apiRef`, the engine also provisions the CDC's authn identity and —
when `apiRefRBAC` is enabled — **auto-generates the RBAC** for exactly the reads the
RESTAction performs, from snowplow's `GET /rbac` read-set
([how-to](./how-to/apiref-status-projection-authn.md),
[design](./design/apiref-rbac-generation.md)).

## Where it sits in the platform

The Krateo installer runs core-provider as its own first composition (the umbrella
chart is a `CompositionDefinition` too — self-hosting). Peers: **snowplow** (resolves
the RESTActions `apiRef` references, and answers `GET /rbac`), **authn** (exchanges
the CDC's projected ServiceAccount token for a service JWT), the **frontend/portal**
(renders composition status the CDC projects).

Deeper: [internals/architecture.md](./internals/architecture.md) (engine internals),
[internals/behavior.md](./internals/behavior.md) (runtime lifecycle),
[api](./api.md) (the CRD contract).

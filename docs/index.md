---
type: Component
title: core-provider — index
description: The map of the core-provider doc bundle — the KCO engine monorepo that turns Helm charts into Kubernetes-native APIs (engine + composition-dynamic-controller + chart-inspector).
resource: oci://ghcr.io/krateo-platformops/charts/core-provider
tags: [kco, compositiondefinition, engine]
timestamp: 2026-08-07T00:00:00Z
---

# core-provider

core-provider is the **engine of Krateo Composable Operations**: for each
`CompositionDefinition` (a Helm-chart reference) it generates a versioned CRD from the
chart's `values.schema.json` and deploys a per-version
**composition-dynamic-controller (CDC)** that reconciles instances of that CRD
("Compositions") — one Helm release per instance — on the management cluster or on a
remote spoke. This monorepo carries three components under `go/` (engine, CDC,
chart-inspector), the Helm chart under `helm/core-provider/`, and one version line:
all three images and the chart ship together from a single plain-semver tag.

## The bundle (start here)

- [overview](./overview.md) — what it does and how it works: the three components,
  the reconcile flow, the digest contract, local vs remote targets.
- [usage](./usage.md) — install via the Krateo installer pin or direct
  `helm install oci://…`; the Kubernetes >= 1.36 floor; local render.
- [configuration](./configuration.md) — the whole config surface: chart values
  (engine / chart-inspector / cdc / apiRefRBAC / otel), env vars, asset templates.
- [api](./api.md) — the two owned CRDs (`CompositionDefinition`,
  `KubernetesTarget`), the generated composition CRDs, the chart-inspector HTTP API,
  the metrics endpoint.
- [examples](./examples.md) — the runnable examples under `examples/`.
- [release](./release.md) — how a release ships (one tag → three images + chart).
- [log](./log.md) — curated history.
- [llms.txt](./llms.txt) — the version-pinned agent index of this bundle.

## Internals (code-traced, authoritative for the engine's runtime)

- [internals/architecture.md](./internals/architecture.md) — how the engine binary is
  built: entry point, the three controllers, the `internal/tools/*` packages.
- [internals/behavior.md](./internals/behavior.md) — the reconcile lifecycle
  (Observe/Create/Update/Delete), what is deployed per version, the digest contract,
  status projection, remote seeding.
- [internals/gotchas.md](./internals/gotchas.md) — real runtime pitfalls, each
  grounded in code.

Component-local docs: the CDC's runtime contract is in
[go/composition-dynamic-controller/README.md](../go/composition-dynamic-controller/README.md);
the chart-inspector's API in [go/chart-inspector/README.md](../go/chart-inspector/README.md)
(OpenAPI spec: `go/chart-inspector/docs/swagger.yaml`). The engine's OTLP metric
catalog is [go/core-provider/telemetry/metrics-reference.md](../go/core-provider/telemetry/metrics-reference.md).

## How-tos

- [how-to/remote-target-credentials.md](./how-to/remote-target-credentials.md) —
  wiring a `KubernetesTarget`'s kubeconfig Secret with External Secrets Operator;
  target-side RBAC ([remote-target-rbac.yaml](./how-to/remote-target-rbac.yaml)).
- [how-to/apiref-status-projection-authn.md](./how-to/apiref-status-projection-authn.md) —
  how `spec.apiRef` projects a RESTAction into composition status: the authn
  allowlist mapping, the issued group, and the auto-generated RBAC.
- [how-to/migrate-off-remoteinstall.md](./how-to/migrate-off-remoteinstall.md) —
  migrating the removed `RemoteInstall` kind to first-class remote Compositions.

## Design records (`docs/design/`)

Each carries a frontmatter `status` reconciled against the code (2026-08-07):

- [multicluster-compositions](./design/multicluster-compositions.md) — local + remote
  deployment via `KubernetesTarget` (**diverged**: implemented, but the target is now
  namespaced, not cluster-scoped).
- [remote-composition-mirror](./design/remote-composition-mirror.md) — the hub
  `Composition` mirrored onto the spoke; retired `RemoteInstall` (**implemented**).
- [remote-projection-self-seeding](./design/remote-projection-self-seeding.md) —
  Path A: the projected CDC made self-sufficient on the spoke (**implemented**).
- [remote-installer-proposal](./design/remote-installer-proposal.md) — deploy a full
  Krateo onto a spoke (**superseded** by remote-composition-mirror).
- [composition-status-projection](./design/composition-status-projection.md) —
  declarative `statusDataTemplate` + `apiRef` status sources (**implemented**).
- [composition-status-implementation-roadmap](./design/composition-status-implementation-roadmap.md)
  — the sequencing plan for the above (**superseded**; both tracks shipped).
- [apiref-rbac-generation](./design/apiref-rbac-generation.md) — auto-generate the
  RBAC for `apiRef` reads from snowplow's `GET /rbac` read-set (**implemented**).
- [composition-version-management](./design/composition-version-management.md) —
  multi-version CRDs, instance migration, `upgradePolicy` (**implemented**).
- [cdc-sharding](./design/cdc-sharding.md) — horizontal sharding of one CDC
  (**implemented decision: do NOT build it yet**; nothing is coded, by design).
- [design/examples/composition-status-projection](./design/examples/composition-status-projection/README.md)
  — the staged manifests accompanying the status-projection design.

## Archive (`tags: [archive]` — point-in-time, not current truth)

- [audit/core-provider-consolidation-plan.md](./audit/core-provider-consolidation-plan.md)
  — the 2026 consolidation audit (fork vs upstream roadmap), preserved as a dated
  record; the org migration and monorepo fold it discusses have since landed.

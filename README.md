# core-provider

The engine of Krateo Composable Operations: it turns any Helm chart with a
`values.schema.json` into a Kubernetes-native API (a generated CRD) reconciled by a
dedicated composition-dynamic-controller.

## What is this

A Go monorepo with three components that ship together from a single tag: the
**core-provider engine** (`go/core-provider/` — reconciles `CompositionDefinition`s:
fetches the chart, generates a versioned CRD from its schema, deploys a per-version
CDC with least-privilege RBAC, locally or onto a remote cluster), the
**composition-dynamic-controller** (`go/composition-dynamic-controller/` — the spawned
per-Kind controller that drives one Helm release per composition instance), and the
**chart-inspector** (`go/chart-inspector/` — the dry-run render service both use to
compute chart RBAC). The Helm chart lives in `helm/core-provider/`.
Full picture: [docs/index.md](docs/index.md).

## Install

Normally installed by the **Krateo installer**, which pins the chart. Standalone
(requires Kubernetes >= 1.36 — see [docs/usage.md](docs/usage.md)):

```sh
helm install core-provider oci://ghcr.io/krateo-platformops/charts/core-provider \
  --version 2.12.4 --namespace krateo-system --create-namespace
```

## Configure

See [docs/configuration.md](docs/configuration.md). Most used:

| Setting | Default | Effect |
|---|---|---|
| `cdc.image.tag` | `""` (tracks `appVersion`) | The CDC image every composition controller runs; empty = version-locked to the release. |
| `apiRefRBAC.enabled` | `true` | Auto-generate the RBAC that authorizes `apiRef` status-projection reads (needs authn's serviceaccount strategy). |
| `global.imageRegistry` | `""` | One value relocates every image (engine, chart-inspector, CDC) for mirrored / air-gapped installs. |

## Examples

- [examples/basic-composition](examples/basic-composition) — the smallest
  `CompositionDefinition`: one OCI chart becomes a namespaced Kubernetes API.
- [examples/remote-target](examples/remote-target) — a `KubernetesTarget` +
  remote-targeted `CompositionDefinition`: the composition deploys onto a spoke cluster.

## Docs

- [docs/index.md](docs/index.md) — the map (bundle + the code-adjacent deep corpus)
- [docs/overview.md](docs/overview.md) — what it does and how it works
- [docs/usage.md](docs/usage.md) — how to install / consume it
- [docs/configuration.md](docs/configuration.md) — the whole config surface
- [docs/api.md](docs/api.md) — the CRDs it owns, the CRDs it generates, the HTTP surfaces
- [docs/examples.md](docs/examples.md) — examples index
- [docs/release.md](docs/release.md) — how a release ships
- [docs/log.md](docs/log.md) — curated history

Internals (code-traced): [docs/internals/architecture.md](docs/internals/architecture.md),
[docs/internals/behavior.md](docs/internals/behavior.md),
[docs/internals/gotchas.md](docs/internals/gotchas.md), and the design corpus under
[docs/design/](docs/design).

## Develop & release

`cd go/<module> && go test ./...` per module (three independent Go modules; the
`integration`-tagged suites need a kind cluster). Tag `X.Y.Z` (no `v`) ships all three
images + the chart — release runbook: [docs/release.md](docs/release.md).

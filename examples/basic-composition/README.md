---
type: Example
title: Basic CompositionDefinition — a chart becomes a Kubernetes API
description: The smallest CompositionDefinition — the core-provider chart itself served as a composition API (the installer's self-hosting pattern).
resource: compositiondefinitions.core.krateo.io
tags: [compositiondefinition, oci, self-hosting]
timestamp: 2026-08-07T00:00:00Z
---

# Basic CompositionDefinition

One `CompositionDefinition` referencing an OCI chart that ships a
`values.schema.json`. This one points at the core-provider chart itself — the exact
self-hosting pattern the Krateo installer uses (core-provider managing its own
chart as a composition); swap `spec.chart` for any schema-bearing chart of yours.

## Preconditions

- core-provider running — a stock Krateo installer deploy, or the standalone
  install in [docs/usage.md](../../docs/usage.md) (Kubernetes >= 1.36).
- The target namespace exists (`krateo-system` on a stock deploy).

## Apply

```sh
kubectl apply -f ./manifest.yaml
```

## What happens

The engine fetches the chart, generates the CRD
`coreproviders.composition.krateo.io` with served version `v2-12-4` (chart version
`2.12.4`, dots→dashes), and deploys the CDC Deployment
`coreproviders-v2-12-4-controller` (+ ConfigMaps + RBAC) in `krateo-system`. Watch:

```sh
kubectl -n krateo-system get compositiondefinition core-provider -w
# until READY True; then:
kubectl api-resources --api-group=composition.krateo.io
```

Creating an instance of the generated `CoreProvider` kind would install the chart
with the instance's spec as Helm values. Cleanup: delete instances first, then the
definition ([gotchas](../../docs/internals/gotchas.md)).

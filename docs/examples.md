---
type: ExampleIndex
title: core-provider — examples
description: Index of the runnable examples under examples/ — a basic CompositionDefinition and a remote-target pair.
resource: oci://ghcr.io/krateo-platformops/charts/core-provider
tags: [examples, compositiondefinition, kubernetestarget]
timestamp: 2026-08-07T00:00:00Z
---

# Examples

- [examples/basic-composition](../examples/basic-composition/README.md) — the
  smallest `CompositionDefinition`: the core-provider chart itself served as a
  Kubernetes API (the installer's self-hosting pattern).
- [examples/remote-target](../examples/remote-target/README.md) — a
  `KubernetesTarget` + a remote-targeted `CompositionDefinition`: the generated CRD,
  RBAC and CDC are projected onto a spoke cluster.

Design-doc staged manifests (illustrative, tied to the status-projection design):
[docs/design/examples/composition-status-projection](./design/examples/composition-status-projection/README.md).

---
type: Example
title: Remote target — project a composition onto a spoke cluster
description: A KubernetesTarget plus a remote-targeted CompositionDefinition — the generated CRD, RBAC and CDC are seeded onto the spoke reached via a kubeconfig Secret.
resource: kubernetestargets.core.krateo.io
tags: [kubernetestarget, remote, multicluster]
timestamp: 2026-08-07T00:00:00Z
---

# Remote target

A namespaced `KubernetesTarget` (pointing at a kubeconfig Secret) plus a
`CompositionDefinition` with `spec.deploy.targetRef`. The definition, its Secret
and its status stay on the hub; the generated CRD, RBAC, CDC — and everything the
CDC needs to be self-sufficient (chart-inspector, the composition-version admission
policy) — are projected onto the spoke.

## Preconditions

- core-provider running on the hub (stock installer deploy or
  [docs/usage.md](../../docs/usage.md)); **both** clusters on Kubernetes >= 1.36.
- A `demo-system` namespace on the hub, and a Secret `spoke-kubeconfig` in it whose
  `kubeconfig` key holds a complete kubeconfig for the spoke (or the
  token+server+ca.crt shape — recipes, including ESO-managed rotation, in
  [remote-target-credentials](../../docs/how-to/remote-target-credentials.md)).
- The spoke identity bound to sufficient RBAC —
  [remote-target-rbac.yaml](../../docs/how-to/remote-target-rbac.yaml).

## Apply

```sh
kubectl apply -f ./manifest.yaml
```

## What happens

`status.target` on the definition reports `mode: Remote` and `connectionStatus:
Healthy`; the `KubernetesTarget`'s own status carries the spoke's Kubernetes
version and probe times. On the spoke, the generated CRD and the CDC Deployment
appear in `demo-system`. Hub `Composition` instances of the generated Kind are
mirrored onto the spoke (spec down, status back up) by the composition-mirror
controller — author them on the hub exactly like local ones.

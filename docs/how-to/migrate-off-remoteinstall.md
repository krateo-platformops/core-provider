---
type: Usage
title: "How-to: migrating off RemoteInstall"
description: RemoteInstall is removed — detach the backing CompositionDefinition + Composition before upgrading, then author the two objects directly.
resource: compositiondefinitions.core.krateo.io
tags: [how-to, migration, multicluster]
timestamp: 2026-08-07T00:00:00Z
---

# Migrating off `RemoteInstall`

`RemoteInstall` has been **removed**. It was a deprecation shim over the remote-composition-mirror
model: a `RemoteInstall{ targetRef, chart, values }` was exactly a remote-targeted
`CompositionDefinition{ chart, deploy.targetRef }` plus a `Composition{ spec: values }`, which the
shim created and owned. See [`../design/remote-composition-mirror.md`](../design/remote-composition-mirror.md) §6.

## ⚠️ If you have existing `RemoteInstall` objects — migrate BEFORE upgrading

The shim already created the backing `CompositionDefinition` and `Composition` for each
`RemoteInstall`, **but they are owned by the `RemoteInstall`** (a controller `ownerReference`).
Removing the `RemoteInstall` CRD deletes the `RemoteInstall` objects, which **cascades to the owned
`CompositionDefinition` + `Composition`** — tearing down the spoke deployment.

Detach the backing objects so they survive, for each `RemoteInstall <name>` in namespace `<ns>`
(its `CompositionDefinition` shares the same name/namespace; the `Composition` it authored shares the
same name/namespace and is of the generated `<Kind>.composition.krateo.io`):

```sh
# 1. drop the RemoteInstall ownerReference so these are no longer GC'd with it
kubectl -n <ns> patch compositiondefinition <name> \
  --type=json -p='[{"op":"remove","path":"/metadata/ownerReferences"}]'
kubectl -n <ns> patch <kind> <name> \
  --type=json -p='[{"op":"remove","path":"/metadata/ownerReferences"}]'

# 2. now the RemoteInstall can be deleted without taking them down
kubectl -n <ns> delete remoteinstall <name>
```

The remote-targeted `CompositionDefinition` and its `Composition` now stand on their own and are
reflected onto the spoke exactly as before — the reflector doesn't care that a `RemoteInstall` once
owned them.

Once every `RemoteInstall` is migrated (or you have none), it is safe to upgrade to a core-provider
build without the `RemoteInstall` kind.

## New usage

Author the two objects directly — a remote-targeted `CompositionDefinition` (the type + where its
instances go) and a `Composition` (the instance, i.e. the chart values):

```yaml
apiVersion: core.krateo.io/v1alpha1
kind: CompositionDefinition
metadata:
  name: krateo
  namespace: team-a
spec:
  chart:
    url: oci://ghcr.io/krateo-platformops/charts/installer
    version: "0.2.180"
  deploy:
    targetRef:
      name: spoke        # a KubernetesTarget in team-a
---
apiVersion: composition.krateo.io/v1-0-0   # the generated Kind for this chart
kind: Installer
metadata:
  name: krateo
  namespace: team-a
spec:
  # the chart values (what RemoteInstall.spec.values carried)
  ...
```

The `CompositionDefinition` generates the `Composition` CRD on both hub and spoke; the reflector
mirrors the hub `Composition` onto the spoke and reads its status back.

---
type: Usage
title: core-provider — usage
description: How to install and consume core-provider — the installer pin, direct helm install from OCI, the Kubernetes 1.36 floor, and authoring your first CompositionDefinition.
resource: oci://ghcr.io/krateo-platformops/charts/core-provider
tags: [install, helm, oci, kubernetes-1.36]
timestamp: 2026-08-07T00:00:00Z
---

# Usage

## Requirements

- **Kubernetes >= 1.36** on the management cluster **and on every remote target**.
  Since 2.0.0 core-provider hosts no admission webhooks: generated CRDs use `None`
  conversion and the `krateo.io/composition-version` label is stamped by a
  `MutatingAdmissionPolicy` (GA in `admissionregistration.k8s.io/v1` at 1.36).
- Charts you manage must ship a **`values.schema.json`** and be a single-root tgz —
  the CRD is generated from that schema; a chart without it fails CRD generation.
- The `apiRefRBAC` feature (on by default) expects the **authn** component's
  serviceaccount login strategy (CRD `serviceaccount.authn.krateo.io`) and
  **snowplow** in-cluster; set `apiRefRBAC.enabled=false` on clusters without them
  ([configuration](./configuration.md)).

## Via the Krateo installer (the normal path)

The installer's umbrella pins the `core-provider` chart and runs it as a composition
of the platform itself. You do not install it by hand; you get it (and its upgrades)
by moving the installer pin. Fresh installer deploys need the bootstrap flag
(`bootstrap.coreProvider.enabled=true`) so the engine exists before the umbrella's
compositions are reconciled — that flag belongs to the installer chart, not this one.

## Direct install (standalone)

The chart does **not** package the two owned CRDs (the installer's bootstrap owns
them); standalone, apply them first — from this repo, or via the separately-versioned
`core-provider-crds` chart also published from here (`helm/core-provider-crds/`,
`oci://ghcr.io/krateo-platformops/charts/core-provider-crds`):

```sh
kubectl apply -f go/core-provider/crds/
helm install core-provider oci://ghcr.io/krateo-platformops/charts/core-provider \
  --version 2.12.4 --namespace krateo-system --create-namespace
```

The chart deploys the engine **and** a chart-inspector Deployment/Service, ships the
CDC asset templates (mounted into the engine pod under `/tmp/assets/…`), and applies
the composition-version `MutatingAdmissionPolicy`.
Upgrades: `helm upgrade` with the new `--version`; the CDC and chart-inspector image
tags track the chart `appVersion`, so every spawned controller rolls to the matching
build on the next reconcile.

## First composition

Apply a `CompositionDefinition` referencing any schema-bearing chart:

```yaml
apiVersion: core.krateo.io/v1alpha1
kind: CompositionDefinition
metadata:
  name: core-provider
  namespace: krateo-system
spec:
  chart:
    url: oci://ghcr.io/krateo-platformops/charts/core-provider
    version: "2.12.4"
```

(This is the self-hosting pattern the Krateo installer uses: core-provider managing
its own chart as a composition.) When it reports `Ready=True`, a new API exists —
`coreproviders.composition.krateo.io/v2-12-4` — and a CDC Deployment named
`coreproviders-v2-12-4-controller` runs in the definition's namespace. Creating an
instance of that API installs the chart with the instance's spec as values. See
[examples](./examples.md) for runnable manifests, including a remote-target pair.

## Local render (no cluster)

The chart templates cleanly offline — useful for CI and review:

```sh
helm template core-provider ./helm/core-provider \
  --namespace krateo-system --set image.tag=2.12.4
```

(The packaged `Chart.yaml` carries `CHART_VERSION`/`APP_VERSION` placeholders that CI
substitutes at release; when rendering from a source checkout, pass an explicit
`image.tag` or substitute the placeholders first, as the lint workflow does.)

## Consuming the docs at the right version

Image tag == chart version == repo tag (one version line). Read this bundle at the
tag matching the running build; [llms.txt](./llms.txt) carries the pin.

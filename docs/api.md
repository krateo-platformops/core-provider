---
type: API
title: core-provider — API
description: The contract — the CompositionDefinition and KubernetesTarget CRDs, the generated composition CRDs, the chart-inspector HTTP API, and the metrics endpoint.
resource: compositiondefinitions.core.krateo.io
tags: [crd, compositiondefinition, kubernetestarget, chart-inspector]
timestamp: 2026-08-07T00:00:00Z
---

# API

Source of truth:
[`go/core-provider/apis/compositiondefinitions/v1alpha1/types.go`](../go/core-provider/apis/compositiondefinitions/v1alpha1/types.go);
generated manifests in [`go/core-provider/crds/`](../go/core-provider/crds)
(drift-gated in PR CI). Both CRDs are **namespaced**, group `core.krateo.io`,
version `v1alpha1`.

## `CompositionDefinition`

The input: "serve this chart as a Kubernetes API".

### `spec`

| Field | Type | Meaning |
|---|---|---|
| `chart.url` | string (required) | OCI ref, repo URL or direct tgz URL. |
| `chart.version` | string (max 20 chars) | Chart version; becomes the CRD version `v<dots-as-dashes>`. |
| `chart.repo` | string (max 20 chars) | Repo-style chart name when `url` is a classic Helm repo. |
| `chart.credentials` | `{username, passwordRef{namespace,name,key}}` | Private-repo credentials; the referenced Secret is watched — rotation re-reconciles. |
| `chart.insecureSkipVerifyTLS` | bool | Skip TLS verification on fetch. |
| `deploy.targetRef.name` | string | A `KubernetesTarget` in this object's OWN namespace; set = deploy the generated CRD + CDC + RBAC onto that spoke. Omitted = local. |
| `apiRef.{name,namespace}` | strings | A snowplow `RESTAction` resolved each reconcile (under the CDC's own authn identity); result exposed to the projection as `.api`. |
| `apiRef.extras` | free-form JSON | Static values merged into the RESTAction's jq root; per-instance context (`compositionId`, …) merges over them, request-wins. |
| `statusDataTemplate[]` | `{forPath, expression, type?, schema?, preserveUnknownFields?}` | Projected status fields: `${ jq }` over `self`/`spec`/`status`/`helm`/`api`, written under `.status` at `forPath`. `type` XOR `schema` XOR `preserveUnknownFields`; validated at reconcile (reserved baseline keys rejected). |
| `upgradePolicy` | `Automatic` \| `Manual` \| `Paused` | Instance migration on a chart-version bump. `Automatic` (default) migrates eagerly; `Manual` waits for the `krateo.io/upgrade-to-version: <version>` annotation; `Paused` freezes migration. |
| `controller.{workers,resyncInterval}` | int / duration | Per-Kind CDC tuning; overrides the chart-global `cdc.workers`/`cdc.resyncInterval` without restarting other controllers. |

### `status`

Conditioned status plus: `apiVersion`/`kind`/`resource` last applied,
`managed.versionInfo[]` (served CRD versions and their CDC digests), `packageUrl`,
`digest` (the FNV idempotency digest — [overview](./overview.md)), and `target`
(`mode` Local/Remote, `connectionStatus` Healthy/Down, target k8s `version`,
kubeconfig Secret `resourceVersion`). With `apiRef` + `apiRefRBAC`, an
`ApiRefRBACIncomplete` condition reports a RESTAction whose read-set could not be
fully enumerated (snowplow 422) — partial RBAC is never written.

## `KubernetesTarget`

A registered spoke: `spec.kubeconfigRef` (`{namespace, name, key}` of a Secret
holding a complete kubeconfig — the rotation seam, ESO-friendly). Its own controller
probes the target periodically and fills `status`: `connectionStatus`
(Healthy/Down), the target's Kubernetes `version`,
`kubeconfigSecretResourceVersion`, `lastProbeTime`, and a Ready condition.
Referenced by name from `CompositionDefinition.spec.deploy.targetRef` and resolved
in the referencing object's own namespace (no cross-namespace targeting).

## The generated CRDs (per chart)

For each definition the engine generates a CRD: group `composition.krateo.io`,
version `v<chart-version-dashed>`, kind = Pascalized chart name; `spec` = the chart's
`values.schema.json`; `status` = baseline keys + any `statusDataTemplate` paths. One
CRD per (group, kind) carries **many served versions** plus a permissive `vacuum`
storage version; conversion is always `None`; instances get the
`krateo.io/composition-version` label stamped by the chart-shipped
`MutatingAdmissionPolicy`. Instances of these CRDs are Compositions, reconciled by
the per-version CDC.

## HTTP surfaces

- **Engine `:8080`** — controller-runtime metrics only. No content API; core-provider
  is a controller, not an HTTP service. OTLP export is opt-in
  ([configuration](./configuration.md)); catalog in
  [`go/core-provider/telemetry/metrics-reference.md`](../go/core-provider/telemetry/metrics-reference.md).
- **chart-inspector `:8081`** — `GET /resources?compositionName=…&compositionNamespace=…&compositionDefinitionName=…&compositionDefinitionNamespace=…&compositionVersion=…&compositionResource=…`
  server-side-renders the chart (`helm template --server`, lookups evaluated against
  the live cluster) and returns the resources an install would touch — the CDC's
  RBAC generator consumes it. Swagger UI at `/swagger/`. OpenAPI:
  [`go/chart-inspector/docs/swagger.yaml`](../go/chart-inspector/docs/swagger.yaml),
  narrative in [`go/chart-inspector/README.md`](../go/chart-inspector/README.md).
- **Consumed, not exposed**: snowplow `GET /rbac` (the `apiRef` read-set) and authn
  `POST /serviceaccount/login` (the CDC token exchange) — see the
  [apiRef how-to](./how-to/apiref-status-projection-authn.md).

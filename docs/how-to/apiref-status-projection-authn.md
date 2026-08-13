---
type: Usage
title: "How-to: the apiRef status source (authn allowlist + RBAC)"
description: How spec.apiRef projects a RESTAction into composition status — the per-composition identity, the auto-created allowlist mapping, and the issued group's RBAC.
resource: compositiondefinitions.core.krateo.io
tags: [how-to, apiref, authn, rbac]
timestamp: 2026-08-07T00:00:00Z
---

# The `apiRef` status source (authn allowlist)

When a `CompositionDefinition` declares `spec.apiRef`, the composition-dynamic-controller
(CDC) resolves that RESTAction via snowplow **under its own identity** each reconcile, and
projects the result under `.api` for `statusDataTemplate` mappings to read.

To authenticate, the CDC presents a **projected ServiceAccount token** (audience `authn`) to
authn's `POST /serviceaccount/login`, which exchanges it for a short-lived service JWT. authn
only performs that exchange for ServiceAccounts that are on its **allowlist** — i.e. for which
a `serviceaccount.authn.krateo.io/ServiceAccount` mapping exists **in the authn operator
namespace** (e.g. `krateo-system`).

All three pieces are **provisioned automatically by core-provider** when `apiRef` is set:

1. the projected token volume on the CDC Deployment
   (`/var/run/secrets/krateo.io/serviceaccount/token`);
2. the authn allowlist mapping (this document); and
3. — when the chart's `apiRefRBAC.enabled` is on (the default) — the **RBAC for the
   issued group**, generated from the RESTAction's read-set (snowplow `GET /rbac`;
   [design](../design/apiref-rbac-generation.md)). authn itself never authors RBAC.

Manual RBAC authoring remains only for clusters with `apiRefRBAC` disabled, or when
the read-set cannot be fully enumerated (the definition then carries the
`ApiRefRBACIncomplete` condition and **no partial RBAC is written**) — see below.

## The per-composition ServiceAccount

core-provider creates one ServiceAccount **per composition**, named `<resource>-<apiVersion>`
in the **composition's namespace**. For a composition resource
`fireworksapps.composition.krateo.io/v1-0-0` deployed in namespace `apps`, the CDC SA is:

```
namespace: apps
name:      fireworksapps-v1-0-0
```

## The allowlist mapping (auto-created)

When `apiRef` is set, core-provider creates this in the authn operator namespace
(`COMPOSITION_AUTHN_NAMESPACE`, default `krateo-system`) and deletes it on undeploy:

```yaml
apiVersion: serviceaccount.authn.krateo.io/v1alpha1
kind: ServiceAccount
metadata:
  name: cdc-apps-fireworksapps-v1-0-0       # cdc-<compositionNamespace>-<resource>-<apiVersion>
  namespace: krateo-system                  # the authn operator namespace
spec:
  serviceAccountRef:
    namespace: apps                          # the composition's namespace
    name: fireworksapps-v1-0-0               # <resource>-<apiVersion>
  groups:
    - krateo:cdc:fireworksapps-v1-0-0        # krateo:cdc:<resource>-<apiVersion>
  displayName: "CDC (apps/fireworksapps-v1-0-0)"
```

This requires core-provider to have manage rights on
`serviceaccounts.serviceaccount.authn.krateo.io` (granted by the core-provider chart
ClusterRole) and to know the authn namespace (`COMPOSITION_AUTHN_NAMESPACE`).

## RBAC for the issued identity (auto-generated; manual fallback)

`spec.groups` become the issued clientconfig certificate's `O=` (organization), so **standard
Kubernetes RBAC bound to that group** scopes what the resolved RESTAction may read. The group
is **per composition** — `krateo:cdc:<resource>-<apiVersion>` — so each composition type is
granted exactly the reads its RESTAction performs.

With `apiRefRBAC.enabled=true` (chart default) core-provider **generates this RBAC itself**:
it asks snowplow's dispatch-free `GET /rbac` for the RESTAction's read-set (the
group/version/resource/namespace/verb rows its in-cluster calls touch) and writes the
ClusterRole/Binding (`internal/tools/deploy/restactionrbac.go`). If any stage is
unresolvable, snowplow answers 422 and core-provider sets `ApiRefRBACIncomplete` instead of
writing partial RBAC.

With `apiRefRBAC` disabled (or while `ApiRefRBACIncomplete` stands), author the binding by
hand:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: krateo-cdc-fireworksapps-restaction-read
subjects:
  - kind: Group
    name: krateo:cdc:fireworksapps-v1-0-0
    apiGroup: rbac.authorization.k8s.io
roleRef:
  kind: ClusterRole
  name: krateo-cdc-fireworksapps-restaction-read   # grants the reads the RESTAction performs
  apiGroup: rbac.authorization.k8s.io
```

## Configuration summary

| Setting | Where | Default |
| --- | --- | --- |
| authn operator namespace | `COMPOSITION_AUTHN_NAMESPACE` (core-provider) | `krateo-system` |
| token audience | CDC Deployment projected volume | `authn` |
| token path | CDC env `COMPOSITION_CONTROLLER_SERVICEACCOUNT_TOKEN_PATH` | `/var/run/secrets/krateo.io/serviceaccount/token` |
| snowplow / authn URLs | CDC env `URL_SNOWPLOW` / `URL_AUTHN` | in-cluster service DNS |
| issued group | derived | `krateo:cdc:<resource>-<apiVersion>` |

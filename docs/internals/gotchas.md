---
type: Usage
title: core-provider — gotchas
description: Real runtime pitfalls — the 1.36 floor, chart-owned asset templates, digest churn, reference-counted CRDs, chart constraints, and the apiRef authorization chain.
resource: ghcr.io/krateo-platformops/core-provider
tags: [internals, gotchas, operations]
timestamp: 2026-08-07T00:00:00Z
---

# core-provider — gotchas

Real runtime pitfalls, each grounded in code/config (paths relative to
`go/core-provider/` unless noted). If a note here ever disagrees with the code at
the deployed tag, the code wins.

## Requires Kubernetes >= 1.36 on every cluster a composition CRD lives in

Since 2.0.0 core-provider hosts no admission webhooks. Generated CRDs use `None`
conversion (`internal/tools/crd/crd.go`, `setNoneConversion`) and the per-object
`krateo.io/composition-version` label is stamped by a **`MutatingAdmissionPolicy`**
(GA `admissionregistration.k8s.io/v1`), which needs k8s >= 1.36
(`internal/tools/policy/policy.go`). This applies to **remote targets too** — not
just the management cluster.

## Who installs the MutatingAdmissionPolicy depends on the cluster

For the **management cluster** the policy ships declaratively with the chart
(`helm/core-provider/templates/compositions-version-policy.yaml`). For **remote
targets** the engine projects it itself during seeding and re-ensures it on Observe
(`internal/tools/policy`, `ensureCompositionVersionPolicy`) — an earlier version of
this doc said core-provider never installs the policy; that changed with the remote
self-seeding work. Running the bare binary without the chart on a local cluster
still leaves local compositions unlabeled.

## CDC asset templates are read from disk at runtime, not embedded

The engine loads its CDC templates from `os.TempDir()/assets/...` — deployment,
configmap, RBAC folder, json-schema configmap, service
(`compositiondefinitions.go`, the `CDCtemplate*Path` vars). They are mounted into
the pod by the chart from
[`helm/core-provider/assets/`](../../helm/core-provider/assets) — **not** baked into
the image. If those paths are missing or stale, `objects.CreateK8sObject` fails
during Deploy. The **Service** in particular is only rendered when its template
file exists on disk (`os.Stat(opts.ServiceTemplatePath)` gates it). No service
template → no CDC Service, silently. (The copies under
`internal/controllers/compositiondefinitions/testdata/manifests/` are test fixtures
— editing them changes tests, not the fleet.)

## The CDC image is version-locked to the release — don't re-pin it casually

The chart wires `ghcr.io/krateo-platformops/composition-dynamic-controller` with an
empty tag that falls back to the chart `appVersion`
(`helm/core-provider/values.yaml`, `cdc.image`). Earlier versions pinned a literal
tag, which let a CDC fix build in a release yet **not deploy with it** (the 2.12.3
chart still deployed cdc 2.12.2 — the #67 self-heal fix sat unshipped). Set
`cdc.image.tag` explicitly only to hold the CDC back, and remember to clear it.

## A shared generated CRD is reference-counted across CompositionDefinitions

Delete only removes the generated CRD when it is the **last**
`CompositionDefinition` for that group/kind; otherwise `SkipCRD` is set and the CRD
is left in place. Likewise it only deletes a version's Compositions when it is the
last definition *for that version*. Deleting one definition will NOT remove a CRD
another definition still uses. Separately, Observe **prunes served versions** no
definition references anymore (`pruneStaleServedVersions`) — a version can
disappear from the CRD without any Delete.

## Delete blocks until Compositions are gone (by design)

If Compositions of the version still exist, Delete returns an error to requeue
rather than force-removing them (`deploy.ErrCompositionStillExist`). A
`CompositionDefinition` stuck "Deleting" usually means live Compositions remain —
delete those first (the still-running CDC must finalize them; Undeploy runs only
after they are gone).

## The chart `version` field is `v<dots-as-dashes>`, and capped

The generated CRD version is `v` + the chart version with dots replaced by dashes
(`internal/tools/chart/chart.go`, `ChartGroupVersionKind`) — chart `1.2.3` → CRD
version `v1-2-3`. `spec.chart.version` is validated `MaxLength=20` (`types.go`);
long/odd version strings are rejected or produce surprising CRD version names.
Corollary: **renaming a chart renames the generated Kind** (Pascalized chart name)
— a new API, not an upgrade.

## The chart MUST be a single-root tgz with a `values.schema.json`

`ChartInfoFromBytes` rejects archives whose top level isn't exactly one directory,
and `ChartJsonSchema` opens `values.schema.json` directly — a chart without that
file fails CRD generation. Charts must ship a JSON schema for their values.

## Chart-fetch retry classifies some errors as permanent

`ChartInfoFromSpec` retries with bounded attempts, but treats 400/401/403/404/422
(and the apimachinery equivalents) as **non-retryable**. A bad credential or a
missing chart fails fast — it will not be masked by retries.

## Secret/Target watches drive credential rotation — don't expect only poll-based pickup

core-provider watches Secrets and KubernetesTargets and re-enqueues every affected
`CompositionDefinition`. Rotating a chart-credential Secret or repointing a
`KubernetesTarget.spec.kubeconfigRef` triggers a reconcile promptly; remote clients
are rebuilt from the kubeconfig **every reconcile** (`clusterkube.Remote`), so
External-Secrets-style rotation is picked up without a restart. The kubeconfig
Secret's `resourceVersion` is recorded in `status.target` for traceability.

## Idempotency depends on the digest — beware changing rendered templates

Whether a reconcile is a no-op is decided by comparing `status.digest` to the
dry-run render digest AND the live-lookup digest. The digest hashes object
identity + spec. Any change to the chart's asset templates (or to fields the hasher
includes) flips **every** existing definition to "not up to date" and
re-applies + restarts each CDC Deployment — which churns running controllers
fleet-wide. Treat template changes (including a chart upgrade, which also moves the
CDC image tag) as a fleet-wide reconcile event.

## `apiRef` needs the authn operator; RBAC is auto-generated only when `apiRefRBAC` is on

Declaring `spec.apiRef` makes core-provider auto-provision the authn allowlist
mapping and project an `authn`-audience token onto the CDC (`authnmapping.go`, the
deployment asset template). Prerequisites that are NOT auto-provisioned: the authn
operator must be installed and watching `COMPOSITION_AUTHN_NAMESPACE` (wrong
namespace / absent authn = the token exchange never succeeds), and snowplow must be
reachable at `CORE_PROVIDER_SNOWPLOW_URL`. The per-composition group's RBAC **is**
auto-generated from snowplow's `GET /rbac` read-set when `apiRefRBAC.enabled=true`
(the chart default); an unresolvable RESTAction surfaces as the
`ApiRefRBACIncomplete` condition and **no partial RBAC is written**. With
`apiRefRBAC` disabled you are back to hand-authoring the binding for
`krateo:cdc:<resource>-<apiVersion>` — without it the resolved RESTAction is
unauthorized and the projection silently reads nothing
([how-to](../how-to/apiref-status-projection-authn.md)).

## core-provider needs manage rights on `serviceaccounts.serviceaccount.authn.krateo.io`

The authn mapping is created/deleted by core-provider itself, so its ClusterRole
must grant manage on `serviceaccounts.serviceaccount.authn.krateo.io` (granted by
`helm/core-provider/templates/clusterrole.yaml`). If that RBAC (or the authn CRD)
is missing, Deploy fails creating the mapping; on undeploy a not-found /
no-CRD-match is tolerated so it never blocks composition teardown.

## `statusDataTemplate` forPaths can't shadow baseline status fields, and are validated at reconcile

`InjectStatusFields` widens the generated CRD's status schema, but
`ValidateStatusFields` runs first and **fails the reconcile** on: an empty or
duplicate `forPath`; a `forPath` whose top segment is a reserved baseline key
(`conditions`, `digest`, `previousDigest`, `managed`, `helmChartUrl`,
`helmChartVersion`, `observedGeneration`); combining `type` with
`schema`/`preserveUnknownFields`, or `schema` with `preserveUnknownFields`; or an
unparseable `${ jq }` expression (`internal/tools/crd/generation/statusfields.go`).
core-provider only shapes the schema — the `${ jq }` is evaluated by the CDC, so a
syntactically valid expression that resolves to nothing simply writes nothing under
`.status`.

## Changing statusDataTemplate / apiRef re-renders the CDC (digest churn)

The encoded `statusDataTemplate`, the `apiRef`, the projected-token volume, the
authn mapping and the generated read-set RBAC all feed the FNV digest. Editing any
of them flips the definition to "not up to date" and re-applies + restarts the CDC
Deployment, exactly like any other template change.

## No webhook server / no serving cert — don't look for one

Unlike pre-2.0.0, the manager starts with no webhook server and no serving
certificate (`main.go`, the comment above `ctrl.NewManager`). There is nothing on a
webhook port; the only server is the `:8080` metrics endpoint. Debugging "webhook
not reachable" against 2.x is a category error.

## A version bump is a migration event — mind `upgradePolicy`

Bumping `spec.chart.version` appends a served CRD version and (under the default
`Automatic` policy) migrates every owned instance and retires the old version's
controller. On definitions whose instances must not move yet, set
`upgradePolicy: Manual` (approve per-version with the
`krateo.io/upgrade-to-version` annotation) or `Paused`
([design](../design/composition-version-management.md)).

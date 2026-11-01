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

## A composition's own Helm release is absent from its `status.managed` and carries no parent label

A composition's release is named after the composition itself: `CalculateReleaseName`
(`composition-dynamic-controller/pkg/meta/meta.go`) returns `obj.GetName()`, or
`<name>-<uid-hash>` when `safeReleaseName` is on. That release **is** the composition's output, so
two absences around it are structural rather than evidence of anything:

- **It never appears in that composition's own `status.managed`.** The list is built by
  `populateManagedResources` from `processor.DecodeMinRelease(rel)` — the objects rendered *inside*
  the release. A release cannot be a child of itself.
- **Its Helm secret carries no `krateo.io/composition-name`.** That label
  (`ParentCompositionNameLabel`, `pkg/meta/meta.go`) marks a *child's parent* and drives
  `IsParentRendered`; the release is not a child, so nothing stamps it.

So "no Krateo ownership labels on the Helm secret" and "absent from the parent composition's
`status.managed`" are both true of a release the CDC is actively managing.

**Their absence carries no information at all** — which is a stronger statement than "it can
mislead". Neither marker *can* be present on a composition's own release, for any composition, so
observing that they are missing tells you nothing about ownership and cannot be weighed as weak
evidence either. Treating the absence of a value that can never appear as though it meant something
is unsound rather than unlucky. Reading it that way, and then patching such a release directly,
briefly flapped the frontend on krateo-demo (2026-10-29) before being reverted.

To ask the real question — *is this release CDC-managed?* — go the other way and look for the
composition, not for markers on the release. Every composition CR carries its release name in the
`krateo.io/release-name` label: `SetReleaseName` stamps it on the CR when absent
(`pkg/meta/meta.go:168`) and `GetReleaseName` reads it back, so the label is present whether or not
`safeReleaseName` is on. A composition carrying that label for the release name **is** the owner.

The source of truth is the composition CR. Patch that and let the CDC converge; a direct edit to the
release is reverted on the next reconcile, which is the behaviour and not a race.

The same fact is recorded operationally in the installer repo (its release and upgrade/rollback
runbooks), reached from a different task. It is restated here because this is the component whose
behaviour it is, and a reader looking at the CDC should not have to find it in another repo.

## Auditing `status.managed[].path` — the raw empty-namespace count is not the defect count

`managedPath` (`internal/composition/support.go:493`) keys the path on the resource's **scope**, not
on whether a namespace string happens to be set:

```go
if !isNamespaced {
	return prefix + "/" + gvr.Resource + "/" + name
}
return prefix + "/namespaces/" + namespace + "/" + gvr.Resource + "/" + name
```

So **an empty `namespace` is the correct recorded value for every cluster-scoped child**, and on a
blueprint heavy with cluster-scoped CRs most entries have one. Counting empty namespaces to look for
the #79 defect therefore measures the wrong thing by a wide margin: on one 3.0-line composition the
raw count was **58** and the defect count was **1** (a namespaced `ecr.services.k8s.aws`
`repositories`); the other 57 were `azuredevops.krateo.io` kinds, all genuinely cluster-scoped.

The defect is the **off-diagonal cell**: a *namespaced* kind recorded with an *empty* namespace.

### Audit by differential, not by count

Recompute what `managedPath` should emit for every entry, taking `isNamespaced` from the cluster's
discovery API — a source independent of the code under test — and compare exactly:

```
# scope, as a boolean, with no table parsing
kubectl get --raw /apis/<group>/<version>    # or /api/v1 for the core group
#   -> .resources[] | {name, namespaced}

# then, per status.managed[] entry, assert:
#   want = managedPath(apiVersion, resource, namespaced, namespace, name)
#   got  = entry.path
```

Measured this way on krateo-057 (2.13.x, 85 compositions, 1184 entries): **1184 exact matches, 0
mismatches, 0 entries in the defect cell**, with 162 cluster-scoped-and-empty entries as the
positive control proving the check could see that cell at all. A differential with no positive arm
is a zero with extra steps.

### Four traps, each of which produced a plausible wrong number

- **Do not key scope on the plural alone.** 10 plurals collide across groups on a stock Krateo
  cluster — `pods`, `nodes`, `events`, `alerts`, `forms` among them. A plural-keyed map reported
  **831** cluster-scoped entries carrying a namespace. Join on `apiVersion` **and** `resource`.
- **Do not parse `kubectl api-resources -o wide` positionally.** `SHORTNAMES` and `CATEGORIES` are
  optional columns, so position is unsound from either end; one attempt put `KIND` where
  `NAMESPACED` belonged (`namespaced=Pod`) for **511 of 607** rows. Use the discovery API.
- **Do not test the path with a `/namespaces/` substring.** A cluster-scoped `namespaces` entry's
  *correct* path is `/api/v1/namespaces/<name>`, so it matches by construction. Compare the whole
  path.
- **Distinguish "group absent" from "cluster unreachable".** A `get` against an unreachable cluster
  returns nothing and reads exactly like an estate with no compositions. Pair it with
  `kubectl get --raw /version`.
- **Assert the instrument collected something, because the same zero arrives from your own
  tooling.** On one run the shell lost its `PATH` mid-script — `python3`, `awk`, `sort` and `head`
  all "command not found" — and the script printed `scope-map entries: 0`. Without the error lines
  that reads as *this cluster serves no resources*, which is the unreachable-cluster trap arriving
  from the audit script rather than from the cluster. A scope map built from a real cluster has
  hundreds of entries (383 on one estate, 607 on another), so assert a floor on it and fail loudly;
  an instrument that silently collects nothing otherwise reports a clean result.

### What the field means on older lines

`status.managed[].path` does not exist before the 3.0 line — a 2.4.x estate (CDC 0.17.31, 14,097
entries across 188 compositions) has **zero** entries carrying one, because the namespace arrived
already resolved at the source and there was no path to get wrong. Exposure to a malformed path
scales with **versions that emit `path`**, not with blueprint count.

## A chart whose rendered manifest has two boolean-ish keys in one mapping LOSES one of them

YAML 1.1 resolves `y`, `yes`, `on`, `true` to `true` and `n`, `no`, `off`, `false` to `false` — for
**map keys** as well as values. The conversion to JSON then turns the key into the string `"true"` or
`"false"`, so two keys from the same truth bucket become **one key**, and one value is destroyed with
no error anywhere.

Measured through `kubectl v1.36.2` (`create --dry-run=client -o json`, which is the same
YAML-to-JSON path Helm's `kube.Client.Build` uses, so it is also what happens when the cdc applies a
rendered manifest):

```
data:                      ->   "data": {"true": "beta"}
  y: alpha                        1 key out of 2 in; "alpha" is GONE
  on: beta

data:                      ->   "data": {"true": "alpha", "false": "beta"}
  y: alpha                        2 keys out of 2 — both survive, RENAMED only
  n: beta
```

**The second case is the control and it is why the rule is narrower than it first looks.** Two
boolean-ish keys are not enough: they must resolve to the *same* boolean. `y` with `n` is a renaming
problem; `y` with `on` is data loss.

```
true  <- y  yes  on   true        two keys from ONE bucket = a key is destroyed
false <- n  no   off  false       one from each = both keys survive, both renamed
```

Verified independently by the `frontend` session across the full matrix, including a neutral control:

```
y + on       1 key of 2   {"true": "beta"}                   alpha destroyed
yes + true   1 key of 2   {"true": "beta"}                   alpha destroyed  (same bucket,
                                                             different spellings)
y + n        2 keys of 2  {"true":"alpha","false":"beta"}     renamed only
on + off     2 keys of 2                                     renamed only
x + z        2 keys       unchanged                          CONTROL — not boolean-ish
```

`yes + true` is the case worth noticing: the two keys share no characters and still collide, because
the bucket is about *resolved value* rather than spelling.

Both parsers this repository pins behave this way, so there is no "we are on the newer library"
escape: `gopkg.in/yaml.v2 v2.4.0` and `sigs.k8s.io/yaml v1.6.0` both resolve 1.1. They differ in
*scope* — `sigs.k8s.io/yaml` converts keys because it round-trips through JSON, while
`gopkg.in/yaml.v2` converts only values and leaves keys as keys.

**The remedy is quoting**, and it is verified rather than assumed: with `"y"` and `"on"` quoted, all
keys survive unchanged. Chart authors writing axis keys (`x`/`y`, exactly how `@ant-design/plots`
config is written) or short flags are the realistic exposure.

Not a defect in this component — it is the behaviour of the YAML-to-JSON conversion that kubectl and
Helm share — but core-provider is what renders and applies those charts, so a reader here is who
needs to know. A lint for it is proposed in `krateo-platformops/frontend#575`; the finding is the
`frontend` session's, measured first through `js-yaml` (which is YAML **1.2** and does none of this)
against `kubectl` (1.1), and corroborated from the library end here.

Two framings worth keeping because each sends the reader to the wrong place: the **apiserver is not
the 1.1 component** (the API is JSON over HTTP and never parses YAML — kubectl and Helm convert
client-side), and **no upgrade caused this** (`js-yaml` has been 1.2 since v4, so the split has been
live the whole time).

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

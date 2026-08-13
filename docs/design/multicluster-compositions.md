---
type: Decision
title: "Design: multi-cluster compositions (local + remote deployment)"
description: Evolve core-provider so a composition deploys locally or onto a remote cluster via KubernetesTarget — the controller-projection model.
resource: kubernetestargets.core.krateo.io
tags: [design, multicluster, kubernetestarget]
status: diverged
timestamp: 2026-08-07T00:00:00Z
---

> **Status (re-verified 2026-08-07): diverged in one detail, otherwise implemented.**
> The mechanism shipped as designed (`spec.deploy.targetRef` → `KubernetesTarget` →
> kubeconfig Secret, per-reconcile client resolution, `None` conversion, k8s >= 1.36).
> Since then: `KubernetesTarget` was **migrated cluster-scoped → namespaced**
> (2026-07-14; resolved in the referencing object's own namespace) and grew its own
> reachability controller + status — the doc below still says cluster-scoped.
> Current truth: [internals/behavior.md](../internals/behavior.md).

# Design: Multi-Cluster Compositions (local + remote deployment)

> Status: **Draft for discussion** · Author: design exploration · Date: 2026-06-13
>
> Goal: evolve `core-provider` so a Composition can be deployed to the **local
> (management) cluster** *or* to **remote/managed clusters**, learning from
> Crossplane `provider-helm` (push model) and Project Sveltos (hybrid push-deploy
> + pull-drift, with an optional true-pull mode).

> **Implementation status (2026-06-13):** Landed on `main` — a `CompositionDefinition`
> chooses where the CDC (+ generated CRD + RBAC) is deployed via
> `spec.deploy.targetRef.name`, which references a **cluster-scoped `KubernetesTarget`
> CRD** (`spec.kubeconfigRef` → a native, ESO-rotatable Secret). No `targetRef` ⇒ local
> (default, backward compatible). Clients are resolved per-reconcile in `Connect()`
> (`internal/tools/clusterkube` resolves targetRef → KubernetesTarget → Secret →
> kube/dynamic/clientset): the CompositionDefinition + secrets + status stay on the
> management client (`e.mgmt`); CRD/RBAC/CDC install on the target clients
> (`e.kube`/`e.dynamic`/`e.client`). Hand-rolled, kubeconfig-only; no SA-token renewal
> in-process (delegated to ESO). `status.target` reports
> mode/connectionStatus/version/kubeconfigSecretResourceVersion. Watches on Secret and
> KubernetesTarget re-reconcile on credential rotation / target repoint.
>
> **Conversion: dropped the webhook, use `None` (resolved 2026-06-14).** The conversion
> webhook handler was a pure passthrough — it copied `metadata`/`spec`/`status` verbatim
> and only relabeled `apiVersion`, which is exactly what Kubernetes' built-in `None`
> converter does. So generated CRDs now use `strategy: None` for **both local and
> remote** targets (`crd.setNoneConversion`); no webhook is registered on any CRD. This
> **dissolves the whole cross-cluster conversion problem** — there is nothing to deploy
> into a remote target, no per-target cert, no `CORE_PROVIDER_WEBHOOK_URL`. The permissive
> **`vacuum` storage version** still provides lossless storage across heterogeneous
> per-version schemas (orthogonal to conversion). Verified on kind: a `None` + vacuum CRD
> with heterogeneous v1/v2 schemas establishes, and an object created under v1 round-trips
> losslessly through storage.
>
> **Mutating webhook → `MutatingAdmissionPolicy` (decided 2026-06-14; planned).** The
> `/mutate` webhook only stamps `metadata.labels["krateo.io/composition-version"]` =
> the request's served version (consumed by `getters.go` for per-version listing/migration;
> needed because the vacuum storage version erases the apiVersion). This is replaceable by
> a single cluster-wide **`MutatingAdmissionPolicy`** (CEL, in-apiserver — no webhook
> server, cert, or `MutatingWebhookConfiguration`) matching `apiGroups:[composition.krateo.io],
> resources:[*]`. Prototyped on kind 1.34: stamps the label from `request.requestKind.version`
> with zero webhook configs, `spec` untouched, label-selection works. **Hard requirement:
> Kubernetes ≥ 1.36** (the GA `v1` API, on by default) on the management cluster **and all
> remote targets** — accepted by Diego. (1.34/1.35 beta needs `--runtime-config`, unsettable
> on managed targets, so 1.36 is the floor.)
>
> **Cascading payoff:** with both `/convert` (None) and `/mutate` (policy) gone, core-provider
> has **no admission webhooks at all** → the webhook server, serving cert, the whole
> `certManager` + cert-reconciler, and the `MutatingWebhookConfiguration` template become
> removable.
>
> **Remote-target policy projection — implemented.** For remote targets core-provider now
> projects the `MutatingAdmissionPolicy` (+ binding) into the target during bootstrap
> (`internal/tools/policy`, create-if-absent, called from `Create`/`Update` when
> `e.remote`), so the version label is reliably stamped without a manual onboarding step;
> an existing/chart-managed policy is left untouched. The label is load-bearing for safe
> deletion, so `deploy.Undeploy` also gained a guard: when a teardown would delete the CRD
> it first does an **unfiltered** list of instances and refuses with
> `ErrCompositionStillExist` if any remain — preventing a missing/lagging policy from
> letting a CRD delete cascade-garbage-collect live composition instances.
>
> **e2e-validated (2026-06-13):** the remote-targeting path was exercised against real
> clusters — a kind management cluster + a disposable single-node GKE target, with a
> self-contained ServiceAccount-token kubeconfig (the how-to recipe). The e2e drives the
> **real `crd.ApplyOrUpdateCRD` remote path** (v1alpha1 create + v1alpha2 AppendVersion →
> `NoneConverter`) against the GKE apiserver, plus `clusterkube.Remote` connectivity and
> target/management isolation. Reproduce with `scripts/e2e-remote-targeting.sh`
> (build-tagged e2e in `internal/tools/clusterkube`).
>
> This e2e **caught a real remote bug**: `crd.Get` returned a CRD without `TypeMeta`
> (typed reads via the direct remote client don't populate it), so `kube.Apply` failed
> with "unstructured object has no kind" on remote multi-version updates. Fixed by
> setting the GVK in `crd.Get`. Still not exercised: the full controller reconcile with
> the CDC image + certManager across two clusters.

---

## 1. Where core-provider is today

`core-provider` is **not** a Helm installer. It is a *schema-driven API generator*:

1. `CompositionDefinition` (`core.krateo.io/v1alpha1`) points at a Helm chart that
   carries a `values.schema.json`.
2. On reconcile, core-provider downloads the chart, derives a GVK from `Chart.yaml`,
   and **generates a CRD** from the JSON schema (`crdgen`, `Managed: true`).
3. It then **deploys a `composition-dynamic-controller` (CDC)** Deployment, scoped
   with RBAC mined from the chart templates, to reconcile *instances* of that CRD.
4. The **CDC** (separate repo) is what actually renders/installs the chart's
   resources when a user creates a Composition instance.

**Everything is single-cluster.** All clients come from `ctrl.GetConfig()`
(in-cluster). Grep for `kubeconfig`/`rest.Config`/`targetCluster` → **0 hits**.
There is no notion of a target cluster, credentials, or remote apply.

Key files: `apis/compositiondefinitions/v1alpha1/types.go`,
`internal/controllers/compositiondefinitions/compositiondefinitions.go`
(Observe/Create/Update/Delete), `internal/tools/deploy/`, `internal/tools/rbacgen/`,
`internal/tools/deployment/`, `internal/tools/chartfs/`.

**Critical insight:** there are two distinct "where does it run" questions, and they
can be answered independently:

- **Control plane** — where the *generated CRD + CDC* live.
- **Data plane** — where the *chart's resources (the actual workload)* land.

Multi-cluster targeting is fundamentally a question about the **data plane**; the
control plane can stay central (push) or be projected into the target (pull).

---

## 2. The two reference models (distilled)

### provider-helm — **pure push**

- `Release` CRD (`helm.crossplane.io/v1beta1`); `spec.providerConfigRef` selects a
  `ProviderConfig`.
- `ProviderConfig.spec.credentials.source ∈ {None, Secret, InjectedIdentity,
  Environment, Filesystem}` (+ optional cloud `identity`: GKE/AKS/EKS-IRSA/Upbound).
  The whole credential→`rest.Config` machinery is imported from **provider-kubernetes**
  (`IdentityAwareBuilder.KubeForProviderConfig`).
- `Connect()` builds a `*rest.Config` for the **target** cluster:
  `InjectedIdentity → rest.InClusterConfig()` (local); otherwise a **kubeconfig from a
  Secret** → remote. Optional `identity` wraps the config with a token RoundTripper.
- Helm SDK (`helm.sh/helm/v3/pkg/action`) runs **in the provider pod** but a custom
  `RESTClientGetter` + `secret` storage driver point every op at the target.
  **Helm release Secrets live in the target cluster.**
- `Observe/Create/Update/Delete` ↔ `GetLastRelease+isUpToDate / Install / Upgrade /
  Uninstall`. Drift = compare desired vs observed values/patches on each poll.
- **No agent in the target.** No connection pooling. Mgmt → target, outbound.
- Trade-offs: central "keys to the kingdom" credential store; mgmt outage = fleet-wide
  blast radius; requires inbound reachability to every target API server; weak tenant
  isolation; value-source changes only picked up on poll.

### Project Sveltos — **hybrid (push deploy + in-cluster drift agent)**, with optional **true pull**

- `ClusterProfile`/`Profile` (`config.projectsveltos.io/v1beta1`) carry `helmCharts[]`,
  `policyRefs[]`, a `clusterSelector` (`metav1.LabelSelector`), and a **`syncMode`**:
  `OneTime | Continuous | ContinuousWithDriftDetection | DryRun`.
- Managed clusters registered as `SveltosCluster` (`lib.projectsveltos.io/v1beta1`);
  kubeconfig stored in a mgmt-cluster Secret (`<name>-sveltos-kubeconfig`), often via a
  ServiceAccount token minted with the **TokenRequest API** and auto-renewed. CAPI
  `Cluster`s are auto-discovered.
- **Default deploy = push:** `addon-controller` (mgmt) reads the target kubeconfig and
  applies Helm/YAML directly into the managed API server (Helm SDK in-process; release
  Secrets land in the **managed** cluster). Direction mgmt → managed.
- **Drift = pull-ish agent:** `drift-detection-manager` runs **inside** each managed
  cluster, watches resources via informers (list driven by a `ResourceSummary` CR the
  mgmt cluster writes into the managed cluster), hashes them, and on change **flips
  `ResourceSummary.Status`** as a *signal*. The mgmt cluster polls those summaries
  (~10s), nulls the cached feature hash, and **re-pushes** the original spec. The agent
  only *detects + signals*; the authoritative re-apply stays central.
- **Agentless option:** run the agents in the mgmt cluster instead (one per target).
- **True pull mode (v1.0.0+):** a `sveltos-applier` agent in the managed cluster *pulls*
  a prepared `ConfigurationBundle` and applies locally — for firewalled/air-gapped/edge.
  Only here does the deploy connection flip to managed → mgmt (no inbound to targets).
- Multi-tenancy: `ClusterProfile` (cluster-scoped/platform) vs `Profile`
  (namespaced/tenant); deploys via **SA impersonation** bounded by tenant RBAC;
  `RoleRequest` + access-manager provision per-tenant RBAC into targets.

### Side-by-side

| Aspect | provider-helm | Sveltos default | Sveltos pull mode |
|---|---|---|---|
| Who applies | provider pod (mgmt) | addon-controller (mgmt) | sveltos-applier (in target) |
| Deploy direction | mgmt → target | mgmt → target | target → mgmt |
| Agent in target | none | yes (classify + drift) | yes (applier) |
| Drift detection | mgmt re-reconcile on poll | in-target informer → signal → mgmt re-push | in-target reconcile |
| Helm release state | target cluster | target cluster | target cluster |
| Best for | reachable clusters | reachable clusters + fast drift recovery | firewalled / edge |

**Takeaway:** "which cluster" reduces to *building a `rest.Config` in `Connect()`*.
Push is simplest and the natural first step. The differentiator worth borrowing from
Sveltos is the **in-target drift agent that only signals, with central re-apply**, plus
a **pull escape hatch** for unreachable clusters.

---

## 3. Proposed architecture (decisions locked)

> **Locked decisions (2026-06-13):**
> 1. **Push-first.** Ship the management-driven path first; in-target drift agent + true
>    pull deferred to a later phase.
> 2. **Definition-level targeting.** The `CompositionDefinition` names one target; all
>    its instances land in that cluster.
> 3. **Hand-rolled, kubeconfig-only credential builder** — no `provider-kubernetes`
>    dependency. (Cloud-identity wrapping out of scope for v1; handled via ESO — see 3.6.)
> 4. **Credentials are native Kubernetes Secrets, rotation owned by External Secrets
>    Operator (ESO).** Supports a kubeconfig Secret and an SA-token, but core-provider
>    does **not** own a bespoke rotation loop — it consumes Secrets and reacts to their
>    rotation (see 3.6).
> 5. **Poll-based drift now**; in-target drift agent planned for a later phase.
> 6. **core-provider projects the CDC *into* the target cluster.** The CDC then runs
>    in-cluster in the target and applies locally using the target's own ServiceAccount.

### 3.1 The model: controller projection (not central remote-apply)

Because a whole `CompositionDefinition` targets one cluster (decision 2) and we deploy
the CDC into that cluster (decision 6), the architecture is **controller projection**:

```
            management cluster                         target cluster (prod-eu)
   ┌───────────────────────────────┐        ┌──────────────────────────────────┐
   │ core-provider                 │  push  │  generated CRD (composition.*)    │
   │  • reconciles CompDefinition  │ ─────► │  composition-dynamic-controller   │
   │  • resolves KubernetesTarget  │ kcfg/  │   (runs here, in-cluster SA)      │
   │  • installs CRD + RBAC + CDC  │ token  │   • installs Helm chart locally   │
   │    into the target            │        │   • Helm release Secrets local    │
   └───────────────────────────────┘        └──────────────────────────────────┘
        needs target creds only at              steady-state data plane uses the
        bootstrap/reconcile time                target's OWN in-cluster identity
```

- The remote credential is needed **only by core-provider, only to bootstrap** the CRD +
  RBAC + CDC Deployment into the target. Steady-state chart installs use the **CDC's
  in-cluster ServiceAccount in the target** — so the "keys to the kingdom" standing-access
  problem of pure push is sharply reduced.
- **Local case is unchanged:** no `targetRef` ⇒ `InjectedIdentity` ⇒ core-provider
  installs the CRD + CDC into the management cluster exactly as today ⇒ **fully backward
  compatible.**

### 3.2 The cluster-target CRD

```yaml
apiVersion: core.krateo.io/v1alpha1
kind: KubernetesTarget
metadata: { name: prod-eu, namespace: krateo-system }
spec:
  credentials:
    source: Secret            # InjectedIdentity (local, default) | Secret
    secretRef:                # a NATIVE k8s Secret; populated/rotated by ESO (see 3.6)
      name: prod-eu-kubeconfig
      namespace: krateo-system
      key: kubeconfig         # or 'token' + 'server'/'ca.crt' for the SA-token form
status:
  connectionStatus: Healthy   # Healthy | Down
  version: v1.30.2            # discovered k8s version of the target
  lastObservedSecretVersion: "…"  # resourceVersion of the consumed Secret (rotation trace)
```

`CompositionDefinition.spec.targetRef` → `{name, namespace}` of a `KubernetesTarget`.
Absent ⇒ local.

### 3.3 Credential builder (hand-rolled, kubeconfig-only)

A small internal package `internal/tools/clusterkube` (or similar):

```go
// BuildRESTConfig returns a *rest.Config for the target named by the CompositionDefinition.
//   InjectedIdentity -> ctrl.GetConfig() (management cluster, current behaviour)
//   Secret           -> read kubeconfig bytes from the referenced Secret,
//                       clientcmd.RESTConfigFromKubeConfig(...)  (or token+server+ca form)
func BuildRESTConfig(ctx, kube client.Client, t *v1alpha1.KubernetesTarget) (*rest.Config, error)
```

No crossplane-runtime / provider-kubernetes imports. Two accepted Secret shapes:
(a) full **kubeconfig** under `key`; (b) **SA token** + `server` + `ca.crt` keys (the
form ESO emits when syncing a minted token). Set sane `QPS`/`Burst`.

### 3.4 What core-provider does per CompositionDefinition

Reuse the existing Observe/Create/Update/Delete flow, but every cluster-touching step
runs against the **target** `rest.Config` instead of the in-cluster one:

| Step (today, local) | New (target-aware) |
|---|---|
| `crd.Install` (CRD into local) | install generated CRD into **target** |
| `rbacgen` + `rbactools.Install*` (local) | mine RBAC, install SA/Role/Binding into **target** |
| `deployment.Install` of CDC (local) | install CDC Deployment into **target** |
| webhook conversion → core-provider `/convert` | reachability caveat — see Risks |

The CDC image/args are unchanged except it now runs in the target and talks to its local
API server (no remote kubeconfig needed by the CDC itself).

### 3.5 Drift (phase-now vs later)

- **Now:** the in-target CDC re-reconciles its Composition instances on its poll interval
  and corrects drift locally; core-provider re-reconciles CRD/RBAC/CDC presence on its
  own poll. No extra component.
- **Later:** Sveltos-style in-target drift agent that hashes applied resources and **only
  signals**; the authoritative re-apply stays with the (in-target) CDC. Informer-based,
  no polling of N remote API servers.

### 3.6 Secrets & rotation — ESO-native (decision 4)

core-provider is a **pure consumer of native Kubernetes Secrets**. It never stores,
mints, or rotates credentials itself. Rotation and authentication are delegated to
**External Secrets Operator**, which syncs from a backing store (Vault, AWS/GCP/Azure
secret managers, etc.) into the `Secret` referenced by a `KubernetesTarget`.

Requirements this imposes on our code:

- **Read on every reconcile** (don't cache the kubeconfig across reconciles); pick up
  ESO-rotated values automatically. ✅ *Implemented* — `clusterkube.Remote` reads the
  Secret each `Connect()`.
- **Watch the referenced Secret** and enqueue the owning `CompositionDefinition` on
  change, so a rotation triggers prompt re-validation (rather than waiting for the next
  poll). ✅ *Implemented* — the controller `.Watches(&corev1.Secret{}, …)` mapping a
  Secret event to every CompositionDefinition referencing it (kubeconfig or chart creds).
  See the ESO recipes in [`../how-to/remote-target-credentials.md`](../how-to/remote-target-credentials.md).
- **Target status** ✅ *Implemented* — `status.target` reports `mode`,
  `connectionStatus` (Healthy/Down, probed via the target's discovery endpoint),
  `version` (target k8s version) and `kubeconfigSecretResourceVersion` (rotation
  traceability). Surfaced as `TARGET`/`CONNECTION` printer columns (`-o wide`).
- **Tolerate brief auth failures** around a rotation boundary (retry/backoff, surface
  `connectionStatus: Down` only after a threshold) — ESO updates are eventually
  consistent.
- **Accept ESO's `ExternalSecret` output shapes** for both forms (kubeconfig blob; or
  `token`/`server`/`ca.crt` keys). Document an `ExternalSecret` example for minting +
  rotating a target ServiceAccount token, instead of building a TokenRequest renewal loop
  in core-provider.
- **No plaintext credentials in CRs or logs**; only Secret references. Honour
  `imagePullSecrets`/registry creds via the same Secret-reference pattern.

This keeps core-provider out of the secret-management business entirely and aligns with
the broader Krateo/Kubernetes secrets ecosystem.

### 3.7 Component / repo impact

| Change | Repo |
|---|---|
| `KubernetesTarget` CRD + types | `core-provider` |
| `CompositionDefinition.spec.targetRef` | `core-provider` |
| Hand-rolled kubeconfig/token → `rest.Config` builder | `core-provider` |
| Target-aware CRD / RBAC / CDC install (Observe/Create/Update/Delete vs target) | `core-provider` (`internal/tools/{crd,rbacgen,deployment,deploy}`) |
| Watch referenced Secret; react to ESO rotation; status | `core-provider` |
| CDC runs in target unchanged (in-cluster SA) — **likely little/no CDC change** | `composition-dynamic-controller` |
| ESO `ExternalSecret` examples for kubeconfig + SA-token | docs |
| Drift agent | later phase / new component |

> Note: because the CDC runs **in** the target with its own SA, the heavy data-plane
> changes that a *central remote-apply* design would have forced into the CDC are largely
> avoided — the bulk of new work is in **core-provider** (bootstrap-into-target +
> credential consumption). Confirm minimal CDC changes during Phase 1.

---

## 4. Phased roadmap

1. **Phase 0 — Foundations (no behaviour change).** `KubernetesTarget` CRD +
   hand-rolled `rest.Config` builder + `InjectedIdentity` local default. Secret-watch
   plumbing + status. Kind cluster-pair test harness.
2. **Phase 1 — Remote bootstrap (MVP).** `CompositionDefinition.spec.targetRef`; install
   CRD + RBAC + CDC into the target; CDC runs there on its in-cluster SA. Static
   kubeconfig Secret. Local-default path unchanged. Delivers "deploy locally **or**
   remote".
3. **Phase 2 — ESO-native rotation + registration UX.** Document/validate
   `ExternalSecret`-driven kubeconfig and SA-token rotation; react to rotation via the
   Secret watch; onboarding helper; optional CAPI auto-discovery. (Optional later:
   label-selector targeting.)
4. **Phase 3 — In-target drift agent (+ true pull / edge).** Signal-only drift agent with
   in-target re-apply; firewalled/edge support; multi-tenant SA scoping.

Each phase is independently shippable; Phase 1 is the smallest cut that delivers the
headline capability.

---

## 5. Resolved decisions & remaining risks

**Resolved** (see locked list in §3): push-first; definition-level targeting; hand-rolled
kubeconfig-only builder; native-Secret + ESO rotation; poll-based drift now; CDC projected
into the target.

**Remaining risks / to validate during Phase 1:**

- **Webhook conversion reachability.** ✅ *Dissolved* — the conversion webhook was a
  passthrough equivalent to `None`, so generated CRDs now use `strategy: None` (local and
  remote) and no conversion webhook exists. There is no reachability problem to solve and
  no in-target conversion component to deploy.
- **CDC ↔ management connectivity.** Confirm the in-target CDC needs nothing from the
  management cluster at steady state (it shouldn't). If it reports status back, that's a
  back-channel to design.
- **RBAC for bootstrap.** The target credential must allow creating CRDs + RBAC +
  Deployments in the target — document the minimum and provide an `ExternalSecret`/SA
  recipe.
- **Confirm minimal/zero CDC code changes** under the in-target model.

---

## 6. Sources

- Crossplane `provider-helm`: `apis/cluster/release/v1beta1/types.go`,
  `pkg/clients/helm/client.go`, `pkg/controller/cluster/release/{release,observe}.go`;
  credential machinery in `provider-kubernetes/pkg/kube/{config,client}`.
- Project Sveltos: `addon-controller` (`api/v1beta1/spec.go`,
  `controllers/handlers_helm.go`, `controllers/resourcesummary_collection.go`),
  `libsveltos` (`api/v1beta1/{sveltoscluster_type,resourcesummary_type}.go`),
  docs at projectsveltos.io.

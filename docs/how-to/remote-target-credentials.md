---
type: Usage
title: "How-to: credentials for remote deployment targets (ESO)"
description: Wiring a KubernetesTarget's kubeconfig Secret with External Secrets Operator — the contract, target-side RBAC, and rotation recipes.
resource: kubernetestargets.core.krateo.io
tags: [how-to, multicluster, eso, credentials]
timestamp: 2026-08-07T00:00:00Z
---

# How-to: credentials for remote deployment targets (with External Secrets Operator)

When a `CompositionDefinition` references a remote cluster via
`spec.deploy.targetRef`, core-provider resolves the named **namespaced
`KubernetesTarget`** (in the `CompositionDefinition`'s OWN namespace), then the kubeconfig
Secret it points at. core-provider is a **pure
consumer of a native Kubernetes Secret**: it reads the kubeconfig on every reconcile and
re-reconciles promptly when the Secret (or the KubernetesTarget) changes — watches are
wired in the controller. It does **not** mint or rotate credentials itself — that is
delegated to your secret manager via **External Secrets Operator (ESO)**.

## The contract

```yaml
# Namespaced: lives in the SAME namespace as every CompositionDefinition that
# references it (targetRef is resolved in the referencing object's own namespace).
apiVersion: core.krateo.io/v1alpha1
kind: KubernetesTarget
metadata:
  name: prod-eu
  namespace: demo-system
spec:
  kubeconfigRef:
    name: prod-eu-kubeconfig     # a native Secret in the management cluster
    namespace: demo-system
    key: kubeconfig              # key holding a complete kubeconfig
---
apiVersion: core.krateo.io/v1alpha1
kind: CompositionDefinition
metadata:
  name: fireworksapp-remote
  namespace: demo-system
spec:
  chart:
    url: https://example.com/fireworks-app-0.1.0.tgz
  deploy:
    targetRef:
      name: prod-eu             # the KubernetesTarget above, same namespace
```

The Secret value under `key` must be a complete kubeconfig that authenticates to the
target cluster (a token+`server`(+`ca.crt`) Secret shape — the form ESO mints for
ServiceAccount tokens — is also accepted since #36). See **RBAC for the target identity** below for what it needs to be able
to do.

## RBAC for the target identity

In the target, the bound identity installs the generated **CustomResourceDefinition**,
the **composition-dynamic-controller** (`Deployment` + `Service` + `ConfigMap` +
`ServiceAccount`), the **RBAC** that controller runs as (`Role`/`ClusterRole` +
bindings), and cleans up the composition instances on delete.

A `ClusterRole` covering this is in
[`remote-target-rbac.yaml`](remote-target-rbac.yaml). **Important caveat:** because the
RBAC it creates for the controller carries permissions *derived from each chart*, the
target identity must be allowed to grant them. Kubernetes privilege-escalation
prevention therefore requires `bind` **and** `escalate` on `rbac.authorization.k8s.io`
(already in the manifest) — without them, creating a Role/ClusterRole whose permissions
exceed the identity's own is rejected. For this reason a fully-static least-privilege
role is not achievable in the general case; `cluster-admin` is the simplest equivalent,
and the provided `ClusterRole` is the tightest practical alternative.

Bind it to the target ServiceAccount referenced by your kubeconfig:

```bash
kubectl apply -f remote-target-rbac.yaml
kubectl create clusterrolebinding core-provider-remote \
  --clusterrole=core-provider-remote-target \
  --serviceaccount=kube-system:core-provider-remote
```

## Rotation model

- **ESO owns rotation.** It syncs the kubeconfig (or a token rendered into a kubeconfig)
  from your backing store (Vault, AWS/GCP/Azure secret managers, …) into the Secret above,
  refreshing on `spec.refreshInterval`.
- **core-provider reacts.** It re-reads the Secret each reconcile and the controller's
  Secret watch enqueues the `CompositionDefinition` as soon as the Secret changes, so a
  rotation is picked up promptly rather than at the next poll.
- **No bespoke renewal loop** lives in core-provider (design decision): the management
  cluster never holds a standing token-minting process; that responsibility is ESO's.

## Recipe A — sync an existing kubeconfig from a secret store

Store a ready kubeconfig in your backing store (e.g. Vault key
`secret/clusters/prod-eu` field `kubeconfig`) and sync it:

```yaml
apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  name: prod-eu-kubeconfig
  namespace: demo-system
spec:
  refreshInterval: 1h
  secretStoreRef:
    name: vault-backend
    kind: SecretStore
  target:
    name: prod-eu-kubeconfig          # matches kubeconfigRef.name
    creationPolicy: Owner
  data:
  - secretKey: kubeconfig             # matches kubeconfigRef.key
    remoteRef:
      key: secret/clusters/prod-eu
      property: kubeconfig
```

## Recipe B — render a ServiceAccount token into a kubeconfig (rotating token)

When the backing store holds the target API endpoint, CA, and a (rotating) ServiceAccount
token separately, use ESO templating to assemble the kubeconfig. ESO re-renders on each
refresh, so token rotation flows straight through to core-provider:

```yaml
apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  name: prod-eu-kubeconfig
  namespace: demo-system
spec:
  refreshInterval: 30m
  secretStoreRef:
    name: vault-backend
    kind: SecretStore
  target:
    name: prod-eu-kubeconfig
    creationPolicy: Owner
    template:
      engineVersion: v2
      data:
        kubeconfig: |
          apiVersion: v1
          kind: Config
          clusters:
          - name: prod-eu
            cluster:
              server: {{ .server }}
              certificate-authority-data: {{ .ca }}
          contexts:
          - name: prod-eu
            context: { cluster: prod-eu, user: core-provider }
          current-context: prod-eu
          users:
          - name: core-provider
            user:
              token: {{ .token }}
  data:
  - { secretKey: server, remoteRef: { key: secret/clusters/prod-eu, property: server } }
  - { secretKey: ca,     remoteRef: { key: secret/clusters/prod-eu, property: ca } }
  - { secretKey: token,  remoteRef: { key: secret/clusters/prod-eu, property: token } }
```

> Tip: provision the target ServiceAccount + RBAC and mint its (short-lived, rotating)
> token in the target cluster out-of-band (CI, a cluster-bootstrap job, or the cloud
> provider's IRSA/Workload-Identity flow), publishing the token to your secret store.
> core-provider only consumes the resulting kubeconfig.

## Target prerequisite: the composition-version policy

core-provider hosts no admission webhooks. Generated CRDs use `None` conversion, and the
`krateo.io/composition-version` label (which core-provider relies on for per-version
listing and migration) is stamped by a cluster-wide **`MutatingAdmissionPolicy`** —
in-apiserver CEL, no webhook server or cert.

Because composition **instances are created in the target cluster**, that policy must
exist **in the target**, not just the management cluster. For remote targets core-provider
**projects the policy into the target automatically** during bootstrap (create-if-absent,
alongside the generated CRD + RBAC + CDC), so no manual onboarding step is required. The
target credential must therefore allow creating `MutatingAdmissionPolicy` /
`MutatingAdmissionPolicyBinding` (cluster-scoped, group `admissionregistration.k8s.io`) in
addition to CRDs, RBAC and Deployments.

The projection is create-if-absent: if you prefer to manage the policy declaratively
yourself (or already ship it via a chart), an existing policy in the target is left
untouched. To install it explicitly, apply the `core-provider-target` chart (in this
monorepo at [`helm/target-chart/`](../../helm/target-chart); published per release) into
the target cluster:

```bash
helm install core-provider-target oci://ghcr.io/krateo-platformops/charts/core-provider-target \
  --version <X.Y.Z> --kube-context <target-cluster>
```

**Requirement:** the GA `MutatingAdmissionPolicy` API (`admissionregistration.k8s.io/v1`)
needs **Kubernetes ≥ 1.36** on the management cluster **and every target**. See
[`../design/multicluster-compositions.md`](../design/multicluster-compositions.md).

## Chart URL reachability (remote targets)

The composition's chart `packageURL` is resolved on the **management** cluster and passed through to
the projected `composition-dynamic-controller` **unchanged**. The cdc runs on the **target**, so the
chart URL must be reachable **from the target**:

- ✅ Public OCI / HTTPS registries (`ghcr.io`, `registry-1.docker.io`, a public chart host).
- ❌ A management-cluster-local URL — a Service DNS (`*.svc`, `*.cluster.local`), `localhost`, or a
  loopback / RFC-1918 / link-local IP — resolves on the hub but **not** on the spoke.

core-provider validates this: when a `CompositionDefinition` targets a remote cluster and the resolved
chart URL is cluster-local, it does **not** project the composition and instead sets an
`Unavailable` condition with reason **`RemoteChartUnreachable`** on the CR:

```
status.conditions:
  - type: Ready
    status: "False"
    reason: RemoteChartUnreachable
    message: 'chart "oci://...svc.cluster.local:5000/..." is cluster-local to the management
      cluster and unreachable from the remote target; publish it to a spoke-reachable registry
      (public OCI/HTTPS)'
```

**Fix:** publish the chart to a registry both clusters can reach. There is no in-cluster URL rewrite —
mirroring a hub-local registry to the spoke is out of scope by design.

## Accepted credential Secret shapes

core-provider is a pure consumer of native Secrets — it never mints or rotates credentials (that is
delegated to ESO). The `kubeconfigRef` Secret is read on every reconcile and may take either shape:

1. **kubeconfig blob** — a full kubeconfig under `kubeconfigRef.key`. This also covers **cloud
   identity** (GKE/EKS/AKS): a kubeconfig whose user carries an `exec` credential plugin
   (`gke-gcloud-auth-plugin`, `aws eks get-token`, `kubelogin`, …) is resolved by client-go, provided
   the plugin binary + cloud credentials are available to the core-provider pod.

2. **token + server (+ ca.crt)** — the shape ESO emits when it mints and rotates a target
   ServiceAccount token via the TokenRequest API. No kubeconfig assembly needed; keys:
   - `token`  — the (short-lived, ESO-rotated) bearer token
   - `server` — the target API server URL, e.g. `https://spoke.example.com:6443`
   - `ca.crt` — the target cluster CA (optional; omit only for an already-trusted endpoint)

Shape (2) lets an `ExternalSecret` sync a minted SA token straight into the reference — no
TokenRequest renewal loop in core-provider. `kubeconfigRef.key` may stay `kubeconfig` (its absence is
what triggers the token-form fallback). Rotation on either shape is picked up automatically (the
Secret is re-read every reconcile and watched); `status.target.kubeconfigSecretResourceVersion`
records which version the last successful probe used.

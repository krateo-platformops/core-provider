# cdc CPU-saturation gate: off vs on

Question: under a reconcile load that actually produces CPU pressure in a composition-dynamic-controller
container, does `COMPOSITION_CONTROLLER_SATURATION_ENABLED=true` change

- (a) the time for N compositions to reach `Ready=True`,
- (b) per-reconcile latency,
- (c) the container's `cpu.pressure` and CPU throttling,

against the gate off; and (d) at idle or light load, does the gate ever cut capacity (a false throttle)?

The result is in [RESULTS-2026-11-28.md](RESULTS-2026-11-28.md); raw per-run data is under `results/`.

## The gate, with the lines that define it

- Switch: `COMPOSITION_CONTROLLER_SATURATION_ENABLED`, default false (`../../main.go`, `saturation-enabled` flag);
  threshold `COMPOSITION_CONTROLLER_CPU_PRESSURE_THRESHOLD`, 0 = the runtime default. Both are read from the
  chart's `cdc.env`, which is rendered into each controller's env ConfigMap (`helm/core-provider/templates/_helpers.tpl`,
  `core-provider.cdcProcessEnvTail`). There is no dedicated chart key.
- Algorithm (unstructured-runtime v1.33.0, the cdc's pin, `pkg/ratelimit/saturation.go`): local capacity starts at 1 and
  rises by 1 per 2s (`DefaultStartupRampInterval`) to the worker count; CPU pressure over the threshold
  (`DefaultCPUPressureThreshold = 50`, strictly greater) for a whole `CPUPressureWindow` (10s) halves it, minimum 1, at most
  once per window (`step`).
- Evidence the gate engaged: `unstructured_runtime.ratelimit.local.capacity` (`pkg/telemetry/metrics.go`, "Not reported when
  saturation gating is off") and the log line `Controller saturated: lowering local capacity` with `from`, `to`, `cause`
  (`pkg/ratelimit/governor.go`, in `sample`), written once per cut. There is no cut counter metric, so the log line is the cut count.
- Workers must exceed 1: the gate's floor is 1, so with the cdc default of 1 worker it can never cut. The benchmark sets 16.

## Design

One disposable GKE cluster in project `integration-test-431120`, named `sat-bench-<random>`, created and deleted by `run.sh`
(zonal, 3 x e2-standard-4, GKE 1.36.4). The delete is in a `trap` on EXIT, INT and TERM. `KUBECONFIG` is exported to a file under
`.bench/` before anything runs, so `~/.kube/config` and the active context are never read or written; the script records the
sha256 of `~/.kube/config` before and after and says whether it changed.

core-provider 2.13.135 is installed from `oci://ghcr.io/krateo-platformops/charts/core-provider-crds` and `.../core-provider`, the
same version in both arms. Only `cdc.env.COMPOSITION_CONTROLLER_SATURATION_ENABLED` differs. Both arms: 16 workers, CPU limit
250m (request 50m), the default resync interval, OTLP metrics exported every 2s to an in-cluster collector (`otel-collector.yaml`)
that `run.sh` reads through the API server's service proxy.

Workload: one `CompositionDefinition` of `chart/` (3 ConfigMaps and 1 Deployment of the pause image, 1 replica), served by an
in-cluster nginx (`chartrepo.yaml`). N=150 compositions are created in one burst in a fresh namespace per run.

Per run, in order: flip the arm if needed (helm upgrade; wait for the env ConfigMap and the CPU limit to show it), restart the
cdc Deployment and then the collector (the old pod's counters share the new pod's `instance` label, so the collector must start
after the old pod is gone), record the pod name and image digest, wait 45s past the gate's startup ramp, take a baseline,
create the burst, and sample until every composition is `Ready=True` (cap 30 min, a timeout is recorded). Two rest phases follow:
5 minutes with the N compositions at rest (at a 250m limit this is NOT idle: their resync alone saturates the container), then the
compositions are deleted, their Deployments and ConfigMaps awaited gone, and 5 minutes with 10 compositions at rest (the light-load
false-throttle probe), each after a 60s grace. Sequence: off, on, on, off, off, on (interleaved, so drift does not line up with an arm).

What is collected, and from where:

| measure | source |
|---|---|
| time to all-Ready | first `metadata.creationTimestamp` to last `Ready=True` `lastTransitionTime`, from the API server |
| reconcile latency p50/p95 | `unstructured_runtime_reconcile_duration_seconds` histogram from the collector, delta between snapshots, Prometheus-style interpolation inside a bucket |
| container cpu.pressure, CPU usage, throttling | kubelet cAdvisor through the API server node proxy, every 5s: `container_pressure_cpu_waiting_seconds_total` (the container's `cpu.pressure` "some total"), `container_cpu_usage_seconds_total`, `container_cpu_cfs_*` |
| gate | `local.capacity` and `saturation.input` gauges every 2s, and the cut log lines |
| restarts and OOMs | pod status and events |

The cdc image is distroless, so there is no `kubectl exec` into it; cAdvisor reads the same cgroup file, and the gate's own
`saturation.input` gauge is a second, independent reading of it.

Positive control (mandatory): an on run must show the gate's capacity below the worker count or a cut in the log, and every run
must show cpu.pressure over the threshold in some 10s window. If no on run engages the gate the comparison is vacuous and the
result says so rather than reporting no difference. The off arm's gate metrics and cut count are recorded too (expected: none).

## Run it

    ./run.sh                      # about 2.5 hours, see Cost
    N=300 LIMIT=200m ./run.sh     # more load or a smaller limit if no arm shows pressure
    ./run.sh teardown sat-bench-xxxxxx   # only if a run was killed hard enough to skip the trap

Needs gcloud (authenticated, access to the project), kubectl, helm, jq, python3. `python3 -I analyze.py results/<stamp>`
rebuilds `runs.csv` and the tables from the raw files. Dev knobs (`REUSE_CLUSTER`, `SKIP_INSTALL`) are described at the top of `run.sh`.

## Cost

An estimate, not read from billing: 3 x e2-standard-4 in us-central1 is roughly USD 0.40 per hour plus disks, and one zonal cluster's
management fee is covered by the free tier. The full run took 2h23m (cluster up to deleted), so a few US dollars at most. The cluster
exists only for the duration of `run.sh`.

## Limits

One cluster shape, one chart (3 ConfigMaps and a pause Deployment, which is a cheap reconcile), one CPU limit, one threshold
(the default 50), one load shape (a burst of creates), 16 workers. The production default is no CPU limit and 1 worker, where
the gate cannot cut at all.

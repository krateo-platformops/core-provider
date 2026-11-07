# Unstructured-Runtime Metrics Reference

This document describes the OpenTelemetry metrics emitted by `unstructured-runtime` and `composition-dynamic-controller`.

## Naming note

Metric names in code use dots. Prometheus normalizes them with underscores, and counters appear with a `_total` suffix.
Histogram queries use the generated `_bucket` series (cumulative count), and `_sum` / `_count` series for average duration.
For average duration, use `_sum` divided by `_count` instead of `_bucket`.

## Core Metrics

| Metric | Type | Unit | Description | PromQL example |
|---|---|---|---|---|
| `unstructured_runtime.startup.success` | Counter | count | Controller started successfully. | `sum(increase(unstructured_runtime_startup_success_total[1h]))` |
| `unstructured_runtime.startup.failure` | Counter | count | Controller startup failed. | `sum(increase(unstructured_runtime_startup_failure_total[1h]))` |
| `unstructured_runtime.reconcile.duration_seconds` | Histogram | seconds | Total reconciliation duration. | `histogram_quantile(0.95, sum by (le) (rate(unstructured_runtime_reconcile_duration_seconds_bucket[5m])))` |
| `unstructured_runtime.reconcile.in_flight` | Gauge | count | Number of reconciliations currently in progress. | `max(unstructured_runtime_reconcile_in_flight)` |
| `unstructured_runtime.ratelimit.local.capacity` | Gauge | count | Process-wide capacity the controller's own CPU saturation allows. Not reported when saturation gating is off: absence means off, never 0. | `min(unstructured_runtime_ratelimit_local_capacity)` |
| `unstructured_runtime.ratelimit.saturation.input` | Gauge | % | Last `cpu.pressure` "some avg10", attribute `input`. Not reported while the input is disabled. | `max(unstructured_runtime_ratelimit_saturation_input)` |
| `unstructured_runtime.ratelimit.saturation.input.enabled` | Gauge | 0/1 | 1 when the saturation input is read, 0 when disabled (no cgroup v2, no PSI). | `min(unstructured_runtime_ratelimit_saturation_input_enabled)` |
| `unstructured_runtime.ratelimit.deferrals` | Counter | count | Reconcile passes deferred by the governor, attributes `key` and `cause` (`throttled`, `capacity`). The CPU rule defers nothing: it bounds workers, so read `local.capacity` for it. | `sum by (cause) (rate(unstructured_runtime_ratelimit_deferrals_total[5m]))` |

## Queue Metrics

| Metric | Type | Unit | Description | PromQL example |
|---|---|---|---|---|
| `unstructured_runtime.reconcile.queue.depth` | UpDownCounter | count | Current number of items in the queue. | `max(unstructured_runtime_reconcile_queue_depth)` |
| `unstructured_runtime.reconcile.queue.wait.duration_seconds` | Histogram | seconds | Time spent waiting in queue before processing. | `histogram_quantile(0.95, sum by (le) (rate(unstructured_runtime_reconcile_queue_wait_duration_seconds_bucket[5m])))` |
| `unstructured_runtime.reconcile.queue.oldest_item_age_seconds` | Histogram | seconds | Age of the oldest item currently in queue. | `histogram_quantile(0.95, sum by (le) (rate(unstructured_runtime_reconcile_queue_oldest_item_age_seconds_bucket[5m])))` |
| `unstructured_runtime.reconcile.queue.work.duration_seconds` | Histogram | seconds | Time spent processing a dequeued item. | `histogram_quantile(0.95, sum by (le) (rate(unstructured_runtime_reconcile_queue_work_duration_seconds_bucket[5m])))` |
| `unstructured_runtime.reconcile.queue.requeues` | Counter | count | Total number of items requeued. | `sum(increase(unstructured_runtime_reconcile_queue_requeues_total[1h]))` |

## Reconciliation Results

| Metric | Type | Unit | Description | PromQL example |
|---|---|---|---|---|
| `unstructured_runtime.reconcile.success` | Counter | count | Successfully completed reconciliations. | `sum(increase(unstructured_runtime_reconcile_success_total[1h]))` |
| `unstructured_runtime.reconcile.failure` | Counter | count | Failed reconciliations. | `sum(increase(unstructured_runtime_reconcile_failure_total[1h]))` |
| `unstructured_runtime.reconcile.requeue.after` | Counter | count | Reconciliations that requested requeue after delay. | `sum(increase(unstructured_runtime_reconcile_requeue_after_total[1h]))` |
| `unstructured_runtime.reconcile.requeue.immediate` | Counter | count | Reconciliations that requested immediate requeue. | `sum(increase(unstructured_runtime_reconcile_requeue_immediate_total[1h]))` |
| `unstructured_runtime.reconcile.requeue.error` | Counter | count | Reconciliations requeued due to error. | `sum(increase(unstructured_runtime_reconcile_requeue_error_total[1h]))` |

## Get Operation Metrics

| Metric | Type | Unit | Description | PromQL example |
|---|---|---|---|---|
| `unstructured_runtime.reconcile.get.duration_seconds` | Histogram | seconds | Time to fetch/get resources. | `histogram_quantile(0.95, sum by (le) (rate(unstructured_runtime_reconcile_get_duration_seconds_bucket[5m])))` |
| `unstructured_runtime.reconcile.get.failure` | Counter | count | Failed get operations. | `sum(increase(unstructured_runtime_reconcile_get_failure_total[1h]))` |

## External Operation Metrics

| Metric | Type | Unit | Description | PromQL example |
|---|---|---|---|---|
| `unstructured_runtime.external.observe.duration_seconds` | Histogram | seconds | Time to observe external resources. | `histogram_quantile(0.95, sum by (le) (rate(unstructured_runtime_external_observe_duration_seconds_bucket[5m])))` |
| `unstructured_runtime.external.observe.failure` | Counter | count | Failed observe operations. | `sum(increase(unstructured_runtime_external_observe_failure_total[1h]))` |
| `unstructured_runtime.external.connect.duration_seconds` | Histogram | seconds | Time to connect/read external references. | `histogram_quantile(0.95, sum by (le) (rate(unstructured_runtime_external_connect_duration_seconds_bucket[5m])))` |
| `unstructured_runtime.external.connect.failure` | Counter | count | Failed connect operations. | `sum(increase(unstructured_runtime_external_connect_failure_total[1h]))` |
| `unstructured_runtime.external.create.duration_seconds` | Histogram | seconds | Time to create external resources. | `histogram_quantile(0.95, sum by (le) (rate(unstructured_runtime_external_create_duration_seconds_bucket[5m])))` |
| `unstructured_runtime.external.create.failure` | Counter | count | Failed create operations. | `sum(increase(unstructured_runtime_external_create_failure_total[1h]))` |
| `unstructured_runtime.external.update.duration_seconds` | Histogram | seconds | Time to update external resources. | `histogram_quantile(0.95, sum by (le) (rate(unstructured_runtime_external_update_duration_seconds_bucket[5m])))` |
| `unstructured_runtime.external.update.failure` | Counter | count | Failed update operations. | `sum(increase(unstructured_runtime_external_update_failure_total[1h]))` |
| `unstructured_runtime.external.delete.duration_seconds` | Histogram | seconds | Time to delete external resources. | `histogram_quantile(0.95, sum by (le) (rate(unstructured_runtime_external_delete_duration_seconds_bucket[5m])))` |
| `unstructured_runtime.external.delete.failure` | Counter | count | Failed delete operations. | `sum(increase(unstructured_runtime_external_delete_failure_total[1h]))` |

## Finalizer Operation Metrics

| Metric | Type | Unit | Description | PromQL example |
|---|---|---|---|---|
| `unstructured_runtime.finalizer.add.duration_seconds` | Histogram | seconds | Time to add finalizers. | `histogram_quantile(0.95, sum by (le) (rate(unstructured_runtime_finalizer_add_duration_seconds_bucket[5m])))` |
| `unstructured_runtime.finalizer.add.failure` | Counter | count | Failed add finalizer operations. | `sum(increase(unstructured_runtime_finalizer_add_failure_total[1h]))` |
| `unstructured_runtime.finalizer.remove.duration_seconds` | Histogram | seconds | Time to remove finalizers. | `histogram_quantile(0.95, sum by (le) (rate(unstructured_runtime_finalizer_remove_duration_seconds_bucket[5m])))` |
| `unstructured_runtime.finalizer.remove.failure` | Counter | count | Failed remove finalizer operations. | `sum(increase(unstructured_runtime_finalizer_remove_failure_total[1h]))` |

## Status Update Metrics

| Metric | Type | Unit | Description | PromQL example |
|---|---|---|---|---|
| `unstructured_runtime.status.update.duration_seconds` | Histogram | seconds | Time to update resource status. | `histogram_quantile(0.95, sum by (le) (rate(unstructured_runtime_status_update_duration_seconds_bucket[5m])))` |
| `unstructured_runtime.status.update.failure` | Counter | count | Failed status update operations. | `sum(increase(unstructured_runtime_status_update_failure_total[1h]))` |

## Composition Dynamic Controller Metrics

| Metric | Type | Unit | Description | PromQL example |
|---|---|---|---|---|
| `composition.rbac_generation.duration_seconds` | Histogram | seconds | Time to generate RBAC policies. **Includes** chart-inspector call internally. | `histogram_quantile(0.95, sum by (le) (rate(composition_rbac_generation_duration_seconds_bucket[5m])))` |
| `composition.rbac_apply.duration_seconds` | Histogram | seconds | Time to apply RBAC policies to the cluster. | `histogram_quantile(0.95, sum by (le) (rate(composition_rbac_apply_duration_seconds_bucket[5m])))` |
| `composition.helm_install.duration_seconds` | Histogram | seconds | Time to install Helm chart. | `histogram_quantile(0.95, sum by (le) (rate(composition_helm_install_duration_seconds_bucket[5m])))` |
| `composition.helm_upgrade.duration_seconds` | Histogram | seconds | Time to upgrade Helm chart. | `histogram_quantile(0.95, sum by (le) (rate(composition_helm_upgrade_duration_seconds_bucket[5m])))` |

### Histogram Bucket Configuration

All `composition.*` histograms use custom bucket boundaries optimized for observing operation latencies:

```
0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0,
2.5, 5.0, 10.0, 25.0, 50.0, 100.0, 250.0, 500.0, 1000.0,
2500.0, 5000.0, 10000.0 (seconds)
```

These boundaries provide millisecond-level precision for sub-second operations and extended range up to 10,000 seconds (2.7 hours) for long-running operations.

### Metric Design Notes

- **chart-inspector integration**: The `composition.rbac_generation.duration_seconds` metric measures the complete generation flow, which internally calls the chart-inspector service to fetch resources. This aggregated measurement eliminates redundant metric registration.
- **RBAC split**: The RBAC generation and apply operations are measured separately to distinguish policy generation time from cluster application time.
- **Helm operations**: Install and upgrade metrics help track deployment performance and identify bottlenecks in the Helm integration.

## Setup Notes

- Metrics are tagged with the following resource attributes:
  - `service.name`: Application identifier
  - `k8s.deployment.name`: Kubernetes deployment name (for multi-instance identification)
  - `service.instance.id`: Stable instance identifier (set to deployment name)
- All metrics are low-cardinality
- Metrics are exported via OTLP HTTP to the OpenTelemetry Collector
- Prometheus scrapes the Collector's Prometheus exporter endpoint
- Grafana connects to Prometheus to visualize the metrics
- Histograms are configured with auto-generated bounds optimized for latency observation

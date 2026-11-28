# CDC Metrics Integration Guide

## Overview
This document describes how to integrate the metrics instrumentation into the Composition Dynamic Controller (CDC) handler.

## Metrics Available

### Chart Inspector Metrics
- `composition_chart_inspector_duration_seconds` - Histogram of chart-inspector call duration
- `composition_chart_inspector_total` - Counter of chart-inspector calls
- `composition_chart_inspector_errors` - Counter of chart-inspector call failures

### RBAC Generation Metrics
- `composition_rbac_generation_duration_seconds` - Histogram of RBAC generation duration
- `composition_rbac_generation_total` - Counter of RBAC generation operations
- `composition_rbac_generation_errors` - Counter of RBAC generation failures
- `composition_rbac_generation_decisions_total{reason}` - Counter of Observe passes by what the derived-RBAC cache decided (#697): `skipped` re-applied the cached rules without asking chart-inspector; `lookup` (the chart calls `lookup`), `key_changed` and `unscannable` asked it. `composition_rbac_generation_total` counts the calls

### RBAC Apply Metrics
- `composition_rbac_apply_duration_seconds` - Histogram of RBAC apply duration
- `composition_rbac_apply_total` - Counter of RBAC apply operations
- `composition_rbac_apply_errors` - Counter of RBAC apply failures

### Helm Operation Metrics
- `composition_helm_install_duration_seconds` - Histogram of Helm install duration
- `composition_helm_install_total` - Counter of Helm install operations
- `composition_helm_install_errors` - Counter of Helm install failures

- `composition_helm_upgrade_duration_seconds` - Histogram of Helm upgrade duration
- `composition_helm_upgrade_total` - Counter of Helm upgrade operations
- `composition_helm_upgrade_errors` - Counter of Helm upgrade failures

- `composition_helm_uninstall_duration_seconds` - Histogram of Helm uninstall duration
- `composition_helm_uninstall_total` - Counter of Helm uninstall operations
- `composition_helm_uninstall_errors` - Counter of Helm uninstall failures

## Integration Steps

### 1. Initialize Metrics in main.go
Metrics initialization is optional and non-blocking. Refer to the handler setup code for integration details.

### 2. Update HandlerOptions in composition.go
Add metrics field to HandlerOptions structure.

### 3. Update Create() method
Wrap chart inspector and RBAC generator with metrics instrumentation. Use Helm metrics wrapper for install operations.

### 4. Update Observe() method
Use Helm metrics wrapper around upgrade operations.

### 5. Update Delete() method
Use Helm metrics wrapper around uninstall operations.

## Prometheus Queries

Once deployed with OTEL enabled, you can query these metrics in Prometheus using histogram_quantile for latency percentiles and rate() for error rates. See the metrics-reference.md for detailed query examples.

## Dashboard Integration

Add new panels to the Grafana dashboard to visualize:
- Chart Inspector latency and error rate
- RBAC generation latency and error rate
- Helm operation latencies (install, upgrade, uninstall) and error rates

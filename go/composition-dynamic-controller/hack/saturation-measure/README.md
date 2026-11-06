# CPU-saturation measurement (unstructured-runtime#66)

`run.py` measures the cdc's opt-in saturation rule on a disposable cluster. The main number is the time
to drain a fixed backlog, read from `-sentinel` ConfigMaps rather than from a condition the controller
writes. CPU pressure, throttling and usage come from cAdvisor, so off arms are measured the same way as
on arms. `local` and deferrals come from the cdc over OTLP (unstructured-runtime v1.11.0 or later).

Needs Kubernetes 1.36 or later, which is the chart floor. See the top of `run.py` for usage.

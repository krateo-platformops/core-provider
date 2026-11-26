{{/*
Expand the name of the chart.
*/}}
{{- define "core-provider.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "core-provider.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "core-provider.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "core-provider.labels" -}}
helm.sh/chart: {{ include "core-provider.chart" . }}
{{ include "core-provider.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: krateo
{{- end }}

{{/*
Selector labels
*/}}
{{- define "core-provider.selectorLabels" -}}
app.kubernetes.io/name: {{ include "core-provider.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "core-provider.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "core-provider.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- include "core-provider.requireServiceAccountName" . }}
{{- .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
With serviceAccount.create=false the chart uses a ServiceAccount created outside it, named by
serviceAccount.name, so the name is required. An empty name used to fall back to the namespace's
`default` ServiceAccount, which is shared by every pod in the namespace that names none. Refused at
render.
*/}}
{{- define "core-provider.requireServiceAccountName" -}}
{{- if not .Values.serviceAccount.name }}
{{- fail "serviceAccount.name is required when serviceAccount.create is false: name the ServiceAccount created outside the chart" }}
{{- end }}
{{- end }}


{{/*
chart-inspector's OBJECT labels (#570, Diego 2026-11-17): app.kubernetes.io/instance is the release,
like every other object of the chart, so `-l app.kubernetes.io/instance=<release>` selects all of
them; app.kubernetes.io/component tells chart-inspector's apart. NOT used on the pod template.
*/}}
{{- define "chart-inspector.labels" -}}
helm.sh/chart: {{ include "core-provider.chart" . }}
app.kubernetes.io/name: {{ include "chart-inspector.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: chart-inspector
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: krateo
{{- end }}

{{/*
chart-inspector's POD labels: exactly the labels the pods carried before #570, selector labels
included (instance <release>-chart-inspector). The Deployment's spec.selector is immutable, so the
labels it selects on cannot move; keeping the pod template byte-identical also means the change
restarts no pod.
*/}}
{{- define "chart-inspector.podLabels" -}}
helm.sh/chart: {{ include "core-provider.chart" . }}
{{ include "chart-inspector.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: krateo
{{- end }}

{{/*
Selector labels
*/}}
{{- define "chart-inspector.selectorLabels" -}}
app.kubernetes.io/name: {{ include "core-provider.name" . }}-chart-inspector
app.kubernetes.io/instance: {{ .Release.Name }}-chart-inspector
{{- end }}

{{- define "chart-inspector.fullname" -}}
{{ include "core-provider.fullname" . }}-chart-inspector
{{- end }}

{{- define "chart-inspector.name" -}}
{{ include "core-provider.name" . }}-chart-inspector
{{- end }}

{{- define "chart-inspector.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "chart-inspector.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- include "core-provider.requireServiceAccountName" . }}
{{- .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Fully-qualified image reference, honoring an optional global.imageRegistry override.
The registry host (.img.registry) is replaced by .Values.global.imageRegistry when that is set, so
a single value relocates EVERY image for a mirror / air-gapped install. Repository paths are
preserved, matching how registry replication mirrors by default. Canonical helper — identical
across all Krateo charts (shipped from the shared chart scaffold).
Usage:
  image: {{ include "krateo.image" (dict "img" .Values.image "global" .Values.global "defaultTag" .Chart.AppVersion) | quote }}
*/}}
{{- define "krateo.image" -}}
{{- $g := .global | default dict -}}
{{- $registry := $g.imageRegistry | default .img.registry -}}
{{- $tag := .img.tag | default .defaultTag -}}
{{- if $registry -}}
{{- printf "%s/%s:%s" $registry .img.repository $tag -}}
{{- else -}}
{{- printf "%s:%s" .img.repository $tag -}}
{{- end -}}
{{- end -}}

{{/*
core-provider.otlpNodeLocal is "true" when tracing/metrics export is on and no explicit otel.endpoint is
set: the Deployment then sends to the OpenTelemetry daemonset agent on ITS OWN NODE, through the
node IP and the daemonset's hostPort (Diego, 2026-11-09; the same standard for every provider).
Node-local matters: the agent's k8sattributes is node-filtered, so a span sent to another node's
agent loses its pod metadata, k8s.deployment.name included, which the audit join keys on. The
hostPort exists on every daemonset version (0.1.19 and later), unlike the <release>-local Service
(0.1.21+, named from the release). An explicit otel.endpoint wins: then this is "".
*/}}
{{- define "core-provider.otlpNodeLocal" -}}
{{- if and .Values.otel.enabled (not .Values.otel.endpoint) -}}true{{- end -}}
{{- end -}}

{{/*
core-provider.otlpPort is the daemonset's OTLP hostPort for the configured protocol: 4317 for gRPC,
4318 for http/protobuf (the default).
*/}}
{{- define "core-provider.otlpPort" -}}
{{- if eq (.Values.otel.protocol | default "http/protobuf") "grpc" -}}4317{{- else -}}4318{{- end -}}
{{- end -}}

{{/*
The composition controller's PROCESS-level environment: the keys every cdc process reads from its
chart, whatever it runs. Shared by a dedicated controller's ConfigMap (assets/cdc/configmap.yaml)
and a pool's process ConfigMap (assets/cdc/pool-configmap.yaml, docs/design/cdc-pooling.md §5c),
so the two cannot drift. Head and tail are separate only because the dedicated ConfigMap interleaves
per-member keys between them; a dedicated controller renders byte-for-byte what it rendered before
(TestPoolMemberConfigMapKeys and the differential in its commit).
*/}}
{{- define "core-provider.cdcProcessEnvHead" -}}
URL_CHART_INSPECTOR: http://{{ include "chart-inspector.fullname" . }}.{{ .Release.Namespace }}.svc.cluster.local:8081/
KRATEO_NAMESPACE: {{ .Release.Namespace }}
{{- end }}

{{- /*
core-provider.durationNs: a Go duration, as values.schema.json's pattern admits it (one or more
<number><unit>, units ns/us/ms/s/m/h), in nanoseconds, as a float. helm has no duration parser; this sums
the parts the way time.ParseDuration does, so the chart can apply client-go's rule to the ownership Lease
timings before anything is rendered from them (#660).
*/ -}}
{{- define "core-provider.durationNs" -}}
{{- $unit := dict "ns" 1.0 "us" 1000.0 "ms" 1000000.0 "s" 1000000000.0 "m" 60000000000.0 "h" 3600000000000.0 -}}
{{- $total := 0.0 -}}
{{- range regexFindAll "[0-9]+(\\.[0-9]+)?(ns|us|ms|s|m|h)" (toString .) -1 -}}
{{- $num := regexFind "^[0-9]+(\\.[0-9]+)?" . | float64 -}}
{{- $total = addf $total (mulf $num (get $unit (regexReplaceAll "^[0-9]+(\\.[0-9]+)?" . ""))) -}}
{{- end -}}
{{- $total -}}
{{- end }}

{{- /*
core-provider.ownershipLeaseEnv (#660): cdc.ownershipLease, the composition controllers' ownership Lease
timings, as the three env keys BOTH binaries read: rendered into every controller's process env
(cdcProcessEnvTail, dedicated and pool) and into core-provider's own ConfigMap (configmap.yaml), from this
one helper, so the two can never disagree. A set client-go's elector would refuse (duration >
renewDeadline > retryPeriod x 1.2, its JitterFactor, every one > 0) refuses the render, naming the three:
the same rule the binaries apply at startup.
*/ -}}
{{- define "core-provider.ownershipLeaseEnv" -}}
{{- $l := .Values.cdc.ownershipLease | default dict -}}
{{- $d := toString ($l.duration | default "") -}}
{{- $r := toString ($l.renewDeadline | default "") -}}
{{- $p := toString ($l.retryPeriod | default "") -}}
{{- $set := printf "cdc.ownershipLease duration=%q renewDeadline=%q retryPeriod=%q" $d $r $p -}}
{{- $re := "^([0-9]+(\\.[0-9]+)?(ns|us|ms|s|m|h))+$" -}}
{{- if not (and (regexMatch $re $d) (regexMatch $re $r) (regexMatch $re $p)) -}}
{{- fail (printf "%s: all three are required, each a Go duration such as 30s (#660)" $set) -}}
{{- end -}}
{{- $dn := include "core-provider.durationNs" $d | float64 -}}
{{- $rn := include "core-provider.durationNs" $r | float64 -}}
{{- $pn := include "core-provider.durationNs" $p | float64 -}}
{{- if or (le $dn 0.0) (le $rn 0.0) (le $pn 0.0) -}}
{{- fail (printf "%s: every one must be greater than zero (#660)" $set) -}}
{{- end -}}
{{- if le $dn $rn -}}
{{- fail (printf "%s: duration must be greater than renewDeadline (client-go's leader-election rule, #660)" $set) -}}
{{- end -}}
{{- /* client-go: RenewDeadline <= time.Duration(JitterFactor * float64(RetryPeriod)) is refused; the product truncates to whole nanoseconds. */ -}}
{{- if le (int64 $rn) (int64 (mulf 1.2 $pn)) -}}
{{- fail (printf "%s: renewDeadline must be greater than retryPeriod x 1.2 (client-go's JitterFactor, #660)" $set) -}}
{{- end -}}
# #660: the composition controllers' ownership Lease timings (values.yaml cdc.ownershipLease), read by
# every controller and by core-provider under these names. One renewal per controller per retryPeriod.
COMPOSITION_CONTROLLER_OWNERSHIP_LEASE_DURATION: {{ $d | quote }}
COMPOSITION_CONTROLLER_OWNERSHIP_LEASE_RENEW_DEADLINE: {{ $r | quote }}
COMPOSITION_CONTROLLER_OWNERSHIP_LEASE_RETRY_PERIOD: {{ $p | quote }}
{{- end }}

{{- define "core-provider.cdcProcessEnvTail" -}}
# Must match the container port named "health" in assets/cdc/deployment.yaml, which the probes use.
COMPOSITION_CONTROLLER_HEALTH_PROBE_PORT: "{{ .Values.cdc.healthProbe.port }}"
{{ include "core-provider.ownershipLeaseEnv" . }}
# #410: bound on every helm operation, hooks included (values.yaml cdc.helmTimeout).
COMPOSITION_CONTROLLER_HELM_TIMEOUT: {{ .Values.cdc.helmTimeout | quote }}
{{- if .Values.cdc.metrics.enabled }}
COMPOSITION_CONTROLLER_METRICS_SERVER_PORT: "{{ .Values.cdc.metrics.port | default 0 }}" # Set to 0 to disable metrics server
{{- end }}
{{- if .Values.otel.enabled }}
OTEL_ENABLED: "true"
OTEL_TRACING_ENABLED: "{{ .Values.otel.tracing }}"
{{- /* #469: one value for both processes. An explicit cdc.env key wins, rendered once (below), never twice. */}}
{{- if not (hasKey (.Values.cdc.env | default dict) "OTEL_LOGS_ENABLED") }}
OTEL_LOGS_ENABLED: "{{ .Values.otel.logs | default false }}"
{{- end }}
{{- if not (hasKey (.Values.cdc.env | default dict) "OTEL_AUDIT_LOGS_ENABLED") }}
OTEL_AUDIT_LOGS_ENABLED: "{{ .Values.otel.auditLogs | default false }}"
{{- end }}
{{- /* An explicit endpoint only. Unset, the Deployment derives a node-local one from the node IP (core-provider.otlpNodeLocal); the old default here, the daemonset's HEADLESS Service, spread spans across node agents and lost their pod metadata. */}}
{{- with .Values.otel.endpoint }}
OTEL_EXPORTER_OTLP_ENDPOINT: {{ . | quote }}
{{- end }}
OTEL_EXPORTER_OTLP_PROTOCOL: {{ .Values.otel.protocol | default "http/protobuf" | quote }}
SERVICE_VERSION: {{ .Values.cdc.image.tag | quote }}
{{- end }}
{{- range $key, $value := .Values.cdc.env }}
{{ $key }}: {{ $value | quote }}
{{- end }}
{{- end }}
